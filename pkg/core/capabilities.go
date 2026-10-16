package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The capabilities broker (docs/remote-core.md §6): heavy or shared calls an instance declares
// (stormo.yaml capabilities:) and grants per agent (agent.yaml capabilities:). Agents start jobs
// with cap_start and follow them with cap_status; the core queues them in core.db, keeps each
// capability to its concurrency, runs them through a worker that is never the core itself (an
// HTTP endpoint, a container here or on a runner), and meters what they cost. The credential a
// worker needs stays with the core or the runner; agents only hold their grant.

// BrokerOptions configure the broker.
type BrokerOptions struct {
	Inst *instance.Instance
	DB   *DB
	// Grants are the capabilities an agent may use (its manifest's).
	Grants func(agent string) []string
	// Secret is one of the core's secret values (secrets.local.yaml shared layer).
	Secret func(name string) (string, bool)
	// Runners carry container jobs to runner placements.
	Runners *LinkHub
	// Scrub cleans the previews kept for the owner's views.
	Scrub func(string) string
	HTTP  *http.Client
	// Container runs a container job on this host (link.RunContainer).
	Container func(ctx context.Context, image string, command []string, env map[string]string, input []byte) ([]byte, error)
	Now       func() time.Time
}

// Broker runs capability jobs.
type Broker struct {
	o       BrokerOptions
	mu      sync.Mutex
	running map[string]int
	wake    chan struct{}
}

// NewBroker builds the broker; Run works the queue.
func NewBroker(o BrokerOptions) *Broker {
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Container == nil {
		o.Container = link.RunContainer
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Scrub == nil {
		o.Scrub = func(s string) string { return s }
	}
	if o.Grants == nil {
		o.Grants = func(id string) []string {
			a, err := manifest.Load(o.Inst.Root, id, o.Inst.Names.Secret)
			if err != nil {
				return nil
			}
			return a.Capabilities
		}
	}
	return &Broker{o: o, running: map[string]int{}, wake: make(chan struct{}, 1)}
}

// JobView is a job as its agent (and the owner) sees it.
type JobView struct {
	ID         int64           `json:"job"`
	Capability string          `json:"capability"`
	Agent      string          `json:"agent"`
	Status     string          `json:"status"` // queued | running | done | failed
	Error      string          `json:"error,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Preview    string          `json:"preview,omitempty"`
	CostUSD    float64         `json:"costUSD"`
	Created    int64           `json:"created"`
	Started    *int64          `json:"started,omitempty"`
	Finished   *int64          `json:"finished,omitempty"`
}

const maxJobInput = 256 << 10

// Granted is what an agent may use, as declared.
func (b *Broker) Granted(agent string) []instance.Capability {
	out := []instance.Capability{}
	for _, name := range b.o.Grants(agent) {
		if c, ok := b.o.Inst.CapabilityNamed(name); ok {
			out = append(out, c)
		}
	}
	return out
}

// Start queues a job for agent.
func (b *Broker) Start(agent, capability string, input json.RawMessage) (*JobView, error) {
	c, ok := b.o.Inst.CapabilityNamed(capability)
	if !ok {
		return nil, toolErr("no capability %q in this instance", capability)
	}
	if !slices.Contains(b.o.Grants(agent), capability) {
		return nil, toolErr("%s is not granted to you (agent.yaml capabilities:)", capability)
	}
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if len(input) > maxJobInput || !json.Valid(input) {
		return nil, toolErr("input is JSON up to %d KB", maxJobInput>>10)
	}
	now := b.o.Now().UnixMilli()
	preview := b.o.Scrub(clip(string(input), 200))
	res, err := b.o.DB.Exec(`INSERT INTO cap_jobs (capability, agent, status, input, preview, cost, created) VALUES (?, ?, 'queued', ?, ?, ?, ?)`,
		c.Name, agent, string(input), preview, c.CostUSD, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	b.o.DB.Audit("agent:"+agent, "cap.start", c.Name, fmt.Sprintf("job %d", id))
	b.poke()
	return &JobView{ID: id, Capability: c.Name, Agent: agent, Status: "queued", Preview: preview, CostUSD: c.CostUSD, Created: now}, nil
}

func (b *Broker) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func scanJob(row interface{ Scan(...any) error }, withOutput bool) (*JobView, error) {
	var j JobView
	var out string
	var started, finished sql.NullInt64
	if err := row.Scan(&j.ID, &j.Capability, &j.Agent, &j.Status, &j.Error, &out, &j.Preview, &j.CostUSD, &j.Created, &started, &finished); err != nil {
		return nil, err
	}
	j.Started, j.Finished = nullable(started), nullable(finished)
	if withOutput && out != "" {
		if json.Valid([]byte(out)) {
			j.Output = json.RawMessage(out)
		} else {
			b, _ := json.Marshal(out)
			j.Output = b
		}
	}
	return &j, nil
}

const jobCols = `id, capability, agent, status, error, output, preview, cost, created, started, finished`

// Job is one of agent's jobs, with its output once done.
func (b *Broker) Job(agent string, id int64) (*JobView, error) {
	j, err := scanJob(b.o.DB.QueryRow(`SELECT `+jobCols+` FROM cap_jobs WHERE id = ? AND agent = ?`, id, agent), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, toolErr("no job %d of yours", id)
	}
	return j, err
}

// Jobs are the newest jobs for the owner: previews, never inputs or outputs.
func (b *Broker) Jobs(limit int) ([]JobView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := b.o.DB.Query(`SELECT `+jobCols+` FROM cap_jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobView{}
	for rows.Next() {
		j, err := scanJob(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Run works the queue until stop. Jobs that were running when the core stopped have failed.
func (b *Broker) Run(stop <-chan struct{}) {
	_, _ = b.o.DB.Exec(`UPDATE cap_jobs SET status = 'failed', error = 'the core stopped while it ran', finished = ? WHERE status = 'running'`, b.o.Now().UnixMilli())
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		b.dispatch()
		select {
		case <-stop:
			return
		case <-t.C:
		case <-b.wake:
		}
	}
}

func (b *Broker) dispatch() {
	rows, err := b.o.DB.Query(`SELECT id, capability, agent, input FROM cap_jobs WHERE status = 'queued' ORDER BY id LIMIT 200`)
	if err != nil {
		return
	}
	type queued struct {
		id                int64
		capability, agent string
		input             string
	}
	var qs []queued
	for rows.Next() {
		var q queued
		if rows.Scan(&q.id, &q.capability, &q.agent, &q.input) == nil {
			qs = append(qs, q)
		}
	}
	rows.Close()
	for _, q := range qs {
		c, ok := b.o.Inst.CapabilityNamed(q.capability)
		if !ok {
			b.finish(q.id, nil, errors.New("the capability is no longer declared"))
			continue
		}
		b.mu.Lock()
		if b.running[c.Name] >= c.Concurrency {
			b.mu.Unlock()
			continue
		}
		b.running[c.Name]++
		b.mu.Unlock()
		_, _ = b.o.DB.Exec(`UPDATE cap_jobs SET status = 'running', started = ? WHERE id = ?`, b.o.Now().UnixMilli(), q.id)
		go func(q queued, c instance.Capability) {
			defer func() {
				b.mu.Lock()
				b.running[c.Name]--
				b.mu.Unlock()
				b.poke()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutMinutes)*time.Minute)
			defer cancel()
			out, err := b.exec(ctx, c, q.id, q.agent, []byte(q.input))
			b.finish(q.id, out, err)
		}(q, c)
	}
}

func (b *Broker) finish(id int64, out []byte, err error) {
	status, msg := "done", ""
	if err != nil {
		status, msg = "failed", clip(err.Error(), 500)
	}
	if len(out) > link.MaxOutput {
		status, msg, out = "failed", "the answer is over 1 MB", nil
	}
	if _, e := b.o.DB.Exec(`UPDATE cap_jobs SET status = ?, error = ?, output = ?, finished = ? WHERE id = ?`, status, msg, string(out), b.o.Now().UnixMilli(), id); e != nil {
		log.Printf("capabilities: job %d: %v", id, e)
	}
}

var secretRef = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

func (b *Broker) exec(ctx context.Context, c instance.Capability, id int64, agent string, input []byte) ([]byte, error) {
	switch c.Kind {
	case "http":
		body, _ := json.Marshal(map[string]any{"job": id, "agent": agent, "input": json.RawMessage(input)})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range c.Headers {
			missing := ""
			v = secretRef.ReplaceAllStringFunc(v, func(m string) string {
				name := secretRef.FindStringSubmatch(m)[1]
				s, ok := b.o.Secret(name)
				if !ok {
					missing = name
				}
				return s
			})
			if missing != "" {
				return nil, fmt.Errorf("the core has no %s (secrets.local.yaml shared)", missing)
			}
			req.Header.Set(k, v)
		}
		res, err := b.o.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(io.LimitReader(res.Body, link.MaxOutput+1))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, fmt.Errorf("%s answered %d: %s", c.Name, res.StatusCode, clip(strings.TrimSpace(string(out)), 300))
		}
		return out, nil
	case "container":
		if p, ok := b.o.Inst.PlacementNamed(c.Placement); ok && p.Kind == instance.PlacementRunner {
			raw, err := b.o.Runners.Call(ctx, p.Name, link.TypeOrder, link.Order{Action: "capability", Agent: agent,
				Job: &link.Job{ID: id, Capability: c.Name, Image: c.Image, Command: c.Command, Secrets: c.Secrets, Input: input, TimeoutMinutes: c.TimeoutMinutes}})
			if errors.Is(err, ErrNoLink) {
				return nil, fmt.Errorf("no runner serves %s now", p.Name)
			}
			if err != nil {
				return nil, err
			}
			var r link.OrderResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, err
			}
			return r.Output, nil
		}
		env := map[string]string{}
		for _, n := range c.Secrets {
			if v, ok := b.o.Secret(n); ok {
				env[n] = v
			}
		}
		return b.o.Container(ctx, c.Image, c.Command, env, input)
	}
	return nil, fmt.Errorf("kind %q", c.Kind)
}

// CapabilityView is a capability in the owner's view: how busy, who may use it, what it cost.
type CapabilityView struct {
	instance.Capability
	Queued  int      `json:"queued"`
	Running int      `json:"running"`
	Agents  []string `json:"agents"`
}

// Spend is what an agent's jobs of a capability cost since a time.
type Spend struct {
	Agent      string  `json:"agent"`
	Capability string  `json:"capability"`
	Jobs       int     `json:"jobs"`
	Failed     int     `json:"failed"`
	USD        float64 `json:"usd"`
}

// View is the registry with queue counts, grants and this month's spend (jobs that did not fail).
func (b *Broker) View(agents []string) (map[string]any, error) {
	counts := map[string][2]int{}
	rows, err := b.o.DB.Query(`SELECT capability, status, COUNT(*) FROM cap_jobs WHERE status IN ('queued', 'running') GROUP BY capability, status`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c, st string
		var n int
		if rows.Scan(&c, &st, &n) == nil {
			v := counts[c]
			if st == "queued" {
				v[0] = n
			} else {
				v[1] = n
			}
			counts[c] = v
		}
	}
	rows.Close()
	caps := []CapabilityView{}
	for _, c := range b.o.Inst.Capabilities {
		c.Headers = nil // header templates can name secrets; not for clients
		v := CapabilityView{Capability: c, Queued: counts[c.Name][0], Running: counts[c.Name][1], Agents: []string{}}
		for _, a := range agents {
			if slices.Contains(b.o.Grants(a), c.Name) {
				v.Agents = append(v.Agents, a)
			}
		}
		caps = append(caps, v)
	}
	now := b.o.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	srows, err := b.o.DB.Query(`SELECT agent, capability, COUNT(*), SUM(status = 'failed'), SUM(CASE WHEN status = 'failed' THEN 0 ELSE cost END)
		FROM cap_jobs WHERE created >= ? GROUP BY agent, capability ORDER BY agent, capability`, month)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	spend := []Spend{}
	for srows.Next() {
		var s Spend
		if srows.Scan(&s.Agent, &s.Capability, &s.Jobs, &s.Failed, &s.USD) == nil {
			spend = append(spend, s)
		}
	}
	return map[string]any{"capabilities": caps, "spend": spend, "since": month}, nil
}

// Prune drops jobs older than keep.
func (b *Broker) Prune(keep time.Duration) error {
	_, err := b.o.DB.Exec(`DELETE FROM cap_jobs WHERE created < ? AND status IN ('done', 'failed')`, b.o.Now().Add(-keep).UnixMilli())
	return err
}

// CapabilityTools are the agents' MCP tools for the broker.
func CapabilityTools(b *Broker) []MCPTool {
	return []MCPTool{
		{
			Name:        "cap_list",
			Description: "The heavy or shared capabilities the core runs for you (media renders, scraping…): their names and what they do. Start one with cap_start.",
			InputSchema: schema(nil, map[string]any{}),
			Run: func(c ToolCall) (any, error) {
				out := []map[string]string{}
				for _, cp := range b.Granted(c.Agent) {
					out = append(out, map[string]string{"name": cp.Name, "description": cp.Description})
				}
				return map[string]any{"capabilities": out}, nil
			},
		},
		{
			Name:        "cap_start",
			Description: "Start a job of one of your capabilities (cap_list) with its input. It runs on its own; follow it with cap_status, which gives the output when it is done.",
			InputSchema: schema([]string{"capability"}, map[string]any{
				"capability": str("the capability's name"),
				"input":      map[string]any{"type": "object", "description": "what the capability takes (its description says)"},
			}),
			Run: func(c ToolCall) (any, error) {
				var req struct {
					Capability string          `json:"capability"`
					Input      json.RawMessage `json:"input"`
				}
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				return b.Start(c.Agent, req.Capability, req.Input)
			},
		},
		{
			Name:        "cap_status",
			Description: "A job you started: queued, running, done (with its output) or failed (with why).",
			InputSchema: schema([]string{"job"}, map[string]any{"job": integer("the job number cap_start gave")}),
			Run: func(c ToolCall) (any, error) {
				var req struct {
					Job int64 `json:"job"`
				}
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				return b.Job(c.Agent, req.Job)
			},
		},
	}
}
