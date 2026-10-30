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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/capability"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The capabilities broker (docs/remote-core.md §6, docs/core.md "Capabilities"): heavy or shared
// calls an instance declares (stormo.yaml capabilities:) and grants per agent (agent.yaml
// capabilities:). Agents explore them (cap_list, cap_describe, which work while the worker sleeps),
// queue jobs (cap_start, with a notice that wakes them when it is done) or run one and wait a little
// (cap_run), follow (cap_status, cap_jobs) and cancel (cap_cancel). The core queues jobs in core.db,
// keeps each capability to its concurrency, runs them through a worker that is never the core itself
// (an HTTP endpoint, a one-shot container, or a service container booted on demand and put to sleep
// when idle, here or on a runner), hands files over through its disk within the agent's own layers,
// and meters what they cost. The credential a worker needs stays with the core or the runner; agents
// only hold their grant.

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
	// Container runs a one-shot container job on this host (link.RunContainerIn); jobDir is mounted at /job.
	Container func(ctx context.Context, image string, command []string, env map[string]string, input []byte, jobDir string) ([]byte, error)
	// Services runs this host's service capabilities (link.NewServices).
	Services *link.Services
	// Workdir hands job files over (nil: no files).
	Workdir *Workdir
	// Notify leaves an agent the notice that a job is done (Bus.Notify).
	Notify func(agent, subject, body string, attach []string) error
	// Image makes a template's image available on this host (capability.EnsureImage).
	Image func(template string) error
	// JobsDir holds jobs' files while they run (.swarm/capabilities/jobs).
	JobsDir string
	// Unit is an agent's unit when the fleet does not know it yet (its manifest's).
	Unit func(agent string) string
	Now  func() time.Time
}

// Broker runs capability jobs.
type Broker struct {
	o       BrokerOptions
	mu      sync.Mutex
	running map[string]int
	cancels map[int64]context.CancelFunc
	builds  map[string]*sync.Mutex
	wake    chan struct{}
}

// NewBroker builds the broker; Run works the queue.
func NewBroker(o BrokerOptions) *Broker {
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Container == nil {
		o.Container = link.RunContainerIn
	}
	if o.Services == nil {
		o.Services = link.NewServices(o.Inst.Slug)
		o.Services.Logf = log.Printf
	}
	if o.Image == nil {
		o.Image = func(t string) error {
			_, err := capability.EnsureImage(t, func(s string) { log.Printf("capabilities: %s", s) })
			return err
		}
	}
	if o.JobsDir == "" {
		o.JobsDir = filepath.Join(o.Inst.Root, ".swarm", "capabilities", "jobs")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Scrub == nil {
		o.Scrub = func(s string) string { return s }
	}
	if o.Unit == nil {
		o.Unit = func(id string) string {
			a, err := manifest.Load(o.Inst.Root, id, o.Inst.Names.Secret)
			if err != nil {
				return ""
			}
			return a.Unit
		}
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
	return &Broker{o: o, running: map[string]int{}, cancels: map[int64]context.CancelFunc{}, builds: map[string]*sync.Mutex{}, wake: make(chan struct{}, 1)}
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
	// Inputs are the shared files handed to it, Out the folder its files go to, Files the ones it wrote.
	Inputs []string `json:"inputs,omitempty"`
	Out    string   `json:"out,omitempty"`
	Files  []string `json:"files,omitempty"`
	Notify bool     `json:"notify"`
	Note   string   `json:"note,omitempty"`
	// Uploads are the files the agent uploaded to the job; Pending how many it still waits for, at Upload.
	Uploads []string `json:"uploads,omitempty"`
	Pending int      `json:"pending,omitempty"`
	Upload  string   `json:"upload,omitempty"`
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

func (b *Broker) granted(agent, capability string) (instance.Capability, error) {
	c, ok := b.o.Inst.CapabilityNamed(capability)
	if !ok {
		return c, toolErr("no capability %q in this instance (cap_list shows yours)", capability)
	}
	if c.Unavailable != "" {
		return c, toolErr("%s cannot run here: %s", capability, c.Unavailable)
	}
	if !slices.Contains(b.o.Grants(agent), capability) {
		return c, toolErr("%s is not granted to you (agent.yaml capabilities:)", capability)
	}
	return c, nil
}

// StartRequest is a job an agent asks for.
type StartRequest struct {
	Capability string          `json:"capability"`
	Input      json.RawMessage `json:"input"`
	// Files are shared paths (files or folders) handed to the worker; Out the shared folder its files go to.
	Files  []string `json:"files"`
	Out    string   `json:"out"`
	Notify *bool    `json:"notify"`
	// FilesPending: files the agent uploads to the job itself (POST /v1/capabilities/jobs/<id>/files),
	// straight into its folder: the job waits for them before it is queued.
	FilesPending int `json:"files_pending"`
}

// Uploads into a waiting job: how many, how long it waits for them.
const (
	maxPending  = 10
	pendingWait = 15 * time.Minute
)

// Start queues a job for agent.
func (b *Broker) Start(agent string, req StartRequest) (*JobView, error) {
	c, err := b.granted(agent, req.Capability)
	if err != nil {
		return nil, err
	}
	input := req.Input
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	if len(input) > maxJobInput || !json.Valid(input) {
		return nil, toolErr("input is JSON up to %d KB", maxJobInput>>10)
	}
	takes, gives := c.Files == "in" || c.Files == "both", c.Files == "out" || c.Files == "both"
	if len(req.Files) > 0 && !takes {
		return nil, toolErr("%s takes no files (cap_describe says what it takes)", c.Name)
	}
	if req.Out != "" && !gives {
		return nil, toolErr("%s writes no files, so it takes no out folder", c.Name)
	}
	if req.FilesPending < 0 || req.FilesPending > maxPending {
		return nil, toolErr("files_pending is 0 to %d", maxPending)
	}
	if req.FilesPending > 0 && (!takes || c.Kind == "http" || c.Placement != "local") {
		return nil, toolErr("%s takes no uploaded files (only a container or service on the core's host that takes files does)", c.Name)
	}
	if (takes || gives) && b.o.Workdir == nil {
		return nil, toolErr("this core has no shared space to hand files over")
	}
	inputs := []string{}
	if len(req.Files) > 0 {
		if inputs, err = b.o.Workdir.JobInputs(agent, req.Files); err != nil {
			return nil, err
		}
	}
	out := ""
	if req.Out != "" {
		if out, err = b.o.Workdir.JobOut(agent, req.Out); err != nil {
			return nil, err
		}
	}
	unit := ""
	if gives && req.Out == "" {
		// the default folder is in the agent's own unit's layer: never guessed (another unit's
		// agents read group)
		if unit = b.unitOf(agent); unit == "" {
			return nil, toolErr("the core does not know your unit yet: name a folder of yours with out, or try again in a moment")
		}
	}
	notify := req.Notify == nil || *req.Notify
	now := b.o.Now().UnixMilli()
	preview := b.o.Scrub(clip(string(input), 200))
	files, _ := json.Marshal(inputs)
	// One transaction: the dispatcher never sees the job before its default folder is set.
	tx, err := b.o.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	status, waitUntil := "queued", int64(0)
	if req.FilesPending > 0 {
		status, waitUntil = "waiting", b.o.Now().Add(pendingWait).UnixMilli()
	}
	res, err := tx.Exec(`INSERT INTO cap_jobs (capability, agent, status, input, preview, cost, created, files, out, notify, pending, wait_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, agent, status, string(input), preview, c.CostUSD, now, string(files), out, notify, req.FilesPending, waitUntil)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if gives && out == "" {
		out = jobOutDefault(unit, c.Name, id)
		if _, err := tx.Exec(`UPDATE cap_jobs SET out = ? WHERE id = ?`, out, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	b.o.DB.Audit("agent:"+agent, "cap.start", c.Name, fmt.Sprintf("job %d", id))
	b.poke()
	v := &JobView{ID: id, Capability: c.Name, Agent: agent, Status: status, Preview: preview, CostUSD: c.CostUSD, Created: now, Inputs: inputs, Out: out, Notify: notify,
		Pending: req.FilesPending}
	if req.FilesPending > 0 {
		v.Upload = UploadPath(id)
		v.Note = fmt.Sprintf("waiting for %d file(s): upload each to %s?name=<file name> (your core key; cap_attach does it for a file you were sent), within %s",
			req.FilesPending, v.Upload, pendingWait)
		return v, nil
	}
	if c.Kind == "service" && c.Placement == "local" && b.o.Services.State(c.Name).State != "awake" {
		v.Note = "its worker is asleep and boots for this job: the first one takes longer"
	}
	return v, nil
}

func (b *Broker) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func scanJob(row interface{ Scan(...any) error }, withOutput bool) (*JobView, error) {
	var j JobView
	var out, inputs, files, uploads string
	var started, finished sql.NullInt64
	if err := row.Scan(&j.ID, &j.Capability, &j.Agent, &j.Status, &j.Error, &out, &j.Preview, &j.CostUSD, &j.Created, &started, &finished,
		&inputs, &j.Out, &j.Notify, &files, &uploads, &j.Pending); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(uploads), &j.Uploads)
	if j.Status == "waiting" {
		j.Upload = UploadPath(j.ID)
	}
	j.Started, j.Finished = nullable(started), nullable(finished)
	_ = json.Unmarshal([]byte(inputs), &j.Inputs)
	_ = json.Unmarshal([]byte(files), &j.Files)
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

const jobCols = `id, capability, agent, status, error, output, preview, cost, created, started, finished, files, out, notify, outputs, uploads, pending`

// Job is one of agent's jobs, with its output once done.
func (b *Broker) Job(agent string, id int64) (*JobView, error) {
	j, err := scanJob(b.o.DB.QueryRow(`SELECT `+jobCols+` FROM cap_jobs WHERE id = ? AND agent = ?`, id, agent), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, toolErr("no job %d of yours", id)
	}
	return j, err
}

// Jobs are the newest jobs for the owner: previews and file paths scrubbed, never inputs or outputs.
func (b *Broker) Jobs(limit int) ([]JobView, error) {
	jobs, err := b.jobs(`SELECT `+jobCols+` FROM cap_jobs ORDER BY id DESC LIMIT ?`, limit)
	for i := range jobs {
		for _, l := range [][]string{jobs[i].Inputs, jobs[i].Files, jobs[i].Uploads} {
			for k := range l {
				l[k] = b.o.Scrub(l[k])
			}
		}
		jobs[i].Out = b.o.Scrub(jobs[i].Out)
	}
	return jobs, err
}

// AgentJobs are agent's newest jobs, without outputs (cap_status gives one).
func (b *Broker) AgentJobs(agent string, limit int) ([]JobView, error) {
	return b.jobs(`SELECT `+jobCols+` FROM cap_jobs WHERE agent = ? ORDER BY id DESC LIMIT ?`, agent, limit)
}

func (b *Broker) jobs(q string, args ...any) ([]JobView, error) {
	if limit := args[len(args)-1].(int); limit <= 0 || limit > 500 {
		args[len(args)-1] = 100
	}
	rows, err := b.o.DB.Query(q, args...)
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

// Cancel stops one of agent's jobs: a queued one never runs, a running one is interrupted.
func (b *Broker) Cancel(agent string, id int64) (*JobView, error) {
	j, err := b.Job(agent, id)
	if err != nil {
		return nil, err
	}
	switch j.Status {
	case "queued", "waiting":
		res, err := b.o.DB.Exec(`UPDATE cap_jobs SET status = 'failed', error = 'cancelled', finished = ? WHERE id = ? AND status IN ('queued', 'waiting')`, b.o.Now().UnixMilli(), id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return b.Cancel(agent, id) // it just started
		}
		_ = os.RemoveAll(b.jobDir(j.Capability, id))
	case "running":
		b.mu.Lock()
		cancel := b.cancels[id]
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	default:
		return j, nil
	}
	b.o.DB.Audit("agent:"+agent, "cap.cancel", j.Capability, fmt.Sprintf("job %d", id))
	return b.Job(agent, id)
}

// Recover settles what a stopped core left: its running jobs failed, jobs waiting for uploads
// failed (their files went with it), and the job folders removed. StartCore calls it before it
// serves anything, so no job of this run is touched.
func (b *Broker) Recover() {
	now := b.o.Now().UnixMilli()
	_, _ = b.o.DB.Exec(`UPDATE cap_jobs SET status = 'failed', error = 'the core stopped while it ran', finished = ? WHERE status = 'running'`, now)
	_, _ = b.o.DB.Exec(`UPDATE cap_jobs SET status = 'failed', error = 'the core restarted before its files arrived', finished = ? WHERE status = 'waiting'`, now)
	_ = os.RemoveAll(b.o.JobsDir)
}

// Run works the queue until stop (Recover settled the last core's jobs first). Idle services go to
// sleep; all of them do when the core stops.
func (b *Broker) Run(stop <-chan struct{}) {
	for _, c := range b.o.Inst.Capabilities {
		if c.Kind == "service" && c.Placement == "local" {
			b.o.Services.Clean() // containers a stopped core left behind
			go b.o.Services.Run(stop, 30*time.Second)
			break
		}
	}
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

type queued struct {
	id                int64
	capability, agent string
	input             string
	inputs            []string
	uploads           []string
	out               string
}

func (b *Broker) dispatch() {
	b.expireWaiting()
	rows, err := b.o.DB.Query(`SELECT id, capability, agent, input, files, out, uploads FROM cap_jobs WHERE status = 'queued' ORDER BY id LIMIT 200`)
	if err != nil {
		return
	}
	var qs []queued
	for rows.Next() {
		var q queued
		var files, uploads string
		if rows.Scan(&q.id, &q.capability, &q.agent, &q.input, &files, &q.out, &uploads) == nil {
			_ = json.Unmarshal([]byte(files), &q.inputs)
			_ = json.Unmarshal([]byte(uploads), &q.uploads)
			qs = append(qs, q)
		}
	}
	rows.Close()
	for _, q := range qs {
		c, ok := b.o.Inst.CapabilityNamed(q.capability)
		if !ok {
			b.finish(q, nil, nil, errors.New("the capability is no longer declared"))
			continue
		}
		b.mu.Lock()
		if b.running[c.Name] >= c.Concurrency {
			b.mu.Unlock()
			continue
		}
		res, err := b.o.DB.Exec(`UPDATE cap_jobs SET status = 'running', started = ? WHERE id = ? AND status = 'queued'`, b.o.Now().UnixMilli(), q.id)
		if n, _ := res.RowsAffected(); err != nil || n == 0 {
			b.mu.Unlock()
			continue // cancelled meanwhile
		}
		b.running[c.Name]++
		// Cancellable from the start; the job's own minutes start once its worker is ready (exec).
		ctx, cancel := context.WithCancel(context.Background())
		b.cancels[q.id] = cancel
		b.mu.Unlock()
		go func(q queued, c instance.Capability) {
			defer func() {
				cancel()
				b.mu.Lock()
				b.running[c.Name]--
				delete(b.cancels, q.id)
				b.mu.Unlock()
				b.poke()
			}()
			out, files, err := b.exec(ctx, c, q)
			if errors.Is(ctx.Err(), context.Canceled) {
				err = errors.New("cancelled")
			} else if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("it took over its %d minutes", c.TimeoutMinutes)
			}
			b.finish(q, out, files, err)
		}(q, c)
	}
}

func (b *Broker) finish(q queued, out []byte, files []string, err error) {
	status, msg := "done", ""
	if err != nil {
		status, msg = "failed", clip(err.Error(), 1500)
	}
	if len(out) > link.MaxOutput {
		status, msg, out = "failed", "the answer is over 1 MB", nil
	}
	if files == nil {
		files = []string{}
	}
	fb, _ := json.Marshal(files)
	if _, e := b.o.DB.Exec(`UPDATE cap_jobs SET status = ?, error = ?, output = ?, outputs = ?, finished = ? WHERE id = ?`, status, msg, string(out), string(fb), b.o.Now().UnixMilli(), q.id); e != nil {
		log.Printf("capabilities: job %d: %v", q.id, e)
		return
	}
	var notify bool
	if b.o.DB.QueryRow(`SELECT notify FROM cap_jobs WHERE id = ?`, q.id).Scan(&notify) != nil || !notify || b.o.Notify == nil || msg == "cancelled" {
		return
	}
	subject := fmt.Sprintf("%s job %d %s", q.capability, q.id, status)
	body := fmt.Sprintf("Your %s job %d is %s. cap_status(job=%d) gives its answer.", q.capability, q.id, status, q.id)
	if err != nil {
		body = fmt.Sprintf("Your %s job %d failed: %s\ncap_status(job=%d) has the details.", q.capability, q.id, msg, q.id)
	} else if len(files) > 0 {
		body += fmt.Sprintf("\nIt wrote %d file(s) to %s.", len(files), q.out)
	}
	if e := b.o.Notify(q.agent, subject, body, clipList(files, 10)); e != nil {
		log.Printf("capabilities: notice for job %d: %v", q.id, e)
	}
}

func clipList(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var secretRef = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

func (b *Broker) secrets(c instance.Capability) map[string]string {
	env := map[string]string{}
	for _, n := range c.Secrets {
		if v, ok := b.o.Secret(n); ok {
			env[n] = v
		}
	}
	return env
}

// image makes sure a template's image is here (one build at a time per template).
func (b *Broker) image(c instance.Capability) error {
	t, ok := c.TemplateImage()
	if !ok {
		return nil
	}
	b.mu.Lock()
	m := b.builds[t]
	if m == nil {
		m = &sync.Mutex{}
		b.builds[t] = m
	}
	b.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	if err := b.o.Image(t); err != nil {
		return fmt.Errorf("building the %s worker image: %w", t, err)
	}
	return nil
}

// spec is how a local service capability runs.
func (b *Broker) spec(c instance.Capability) link.ServiceSpec {
	env := b.secrets(c)
	for k, v := range c.Env {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	sp := link.ServiceSpec{Name: c.Name, Image: c.Image, Command: c.Command, Port: c.Port, Health: c.Health, Env: env, MemoryMB: c.MemoryMB, ShmMB: c.ShmMB,
		CPUs: c.CPUs, Idle: time.Duration(c.IdleMinutes) * time.Minute, Boot: time.Duration(c.BootMinutes) * time.Minute}
	if c.Files != "none" {
		sp.JobsDir = filepath.Join(b.o.JobsDir, jobDirName(c.Name))
	}
	return sp
}

func jobDirName(capability string) string { return strings.ReplaceAll(capability, ".", "-") }

// exec runs one job. ctx is the job's cancellation; its minutes (timeout_minutes) count from when
// its worker is ready, so building a template's image or booting a sleeping service does not eat them.
func (b *Broker) exec(ctx context.Context, c instance.Capability, q queued) ([]byte, []string, error) {
	input := []byte(q.input)
	clock := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes)*time.Minute)
	}
	switch c.Kind {
	case "http":
		ctx, cancel := clock()
		defer cancel()
		body, _ := json.Marshal(map[string]any{"job": q.id, "agent": q.agent, "input": json.RawMessage(input)})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
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
				return nil, nil, fmt.Errorf("the core has no %s (secrets.local.yaml shared)", missing)
			}
			req.Header.Set(k, v)
		}
		res, err := b.o.HTTP.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(io.LimitReader(res.Body, link.MaxOutput+1))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, nil, fmt.Errorf("%s answered %d: %s", c.Name, res.StatusCode, clip(strings.TrimSpace(string(out)), 300))
		}
		return out, nil, nil
	case "container", "service":
		if p, ok := b.o.Inst.PlacementNamed(c.Placement); ok && p.Kind == instance.PlacementRunner {
			// The runner keeps the job's minutes itself, after building and booting.
			ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes+c.BootMinutes)*time.Minute+30*time.Minute)
			defer cancel()
			job := &link.Job{ID: q.id, Capability: c.Name, Image: c.Image, Command: c.Command, Secrets: c.Secrets, Input: input, TimeoutMinutes: c.TimeoutMinutes}
			if c.Kind == "service" {
				tpl, _ := c.TemplateImage()
				job.Service = &link.ServiceJob{Template: tpl, Port: c.Port, Health: c.Health, Env: c.Env, MemoryMB: c.MemoryMB, ShmMB: c.ShmMB, CPUs: c.CPUs,
					IdleMinutes: c.IdleMinutes, BootMinutes: c.BootMinutes}
			}
			raw, err := b.o.Runners.Call(ctx, p.Name, link.TypeOrder, link.Order{Action: "capability", Agent: q.agent, Job: job})
			if errors.Is(err, ErrNoLink) {
				return nil, nil, fmt.Errorf("no runner serves %s now", p.Name)
			}
			if err != nil {
				return nil, nil, err
			}
			var r link.OrderResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, nil, err
			}
			return r.Output, nil, nil
		}
		if err := b.image(c); err != nil {
			return nil, nil, err
		}
		// The job's folder: in/ holds its inputs, out/ what it writes; gone once delivered.
		dir := ""
		if c.Files != "none" {
			dir = b.jobDir(c.Name, q.id) // in/ may already hold the agent's uploads
			for _, d := range []string{"in", "out"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o777); err != nil {
					return nil, nil, err
				}
				_ = os.Chmod(filepath.Join(dir, d), 0o777) // the worker may run as another user
			}
			defer os.RemoveAll(dir)
			if err := b.o.Workdir.StageIn(q.agent, q.inputs, filepath.Join(dir, "in")); err != nil {
				return nil, nil, err
			}
		}
		var out []byte
		var err error
		if c.Kind == "service" {
			boot, cancel := context.WithTimeout(ctx, time.Duration(c.BootMinutes)*time.Minute+time.Minute)
			_, err = b.o.Services.Ensure(boot, b.spec(c))
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				return nil, nil, fmt.Errorf("the worker did not boot: %w", err)
			}
		}
		ctx, cancel := clock()
		defer cancel()
		if c.Kind == "container" {
			out, err = b.o.Container(ctx, c.Image, c.Command, b.secrets(c), input, dir)
		} else {
			mount := "/jobs/" + strconv.FormatInt(q.id, 10)
			files := []string{}
			for _, p := range q.inputs {
				files = append(files, mount+"/in/"+path.Base(p))
			}
			for _, u := range q.uploads {
				files = append(files, mount+"/in/"+u)
			}
			outDir := ""
			if c.Files == "out" || c.Files == "both" {
				outDir = mount + "/out"
			}
			body, _ := json.Marshal(map[string]any{"job": q.id, "agent": q.agent, "input": json.RawMessage(input), "files": files, "out": outDir})
			out, err = b.o.Services.Call(ctx, b.spec(c), body)
		}
		if err != nil {
			return nil, nil, err
		}
		var delivered []string
		if dir != "" && q.out != "" {
			if delivered, err = b.o.Workdir.Deliver(q.agent, q.out, filepath.Join(dir, "out"), "capability:"+c.Name); err != nil {
				return out, delivered, fmt.Errorf("its files were not all delivered: %w", err)
			}
		}
		return out, delivered, nil
	}
	return nil, nil, fmt.Errorf("kind %q", c.Kind)
}

// UploadPath is where an agent uploads a file to its waiting job.
func UploadPath(id int64) string { return fmt.Sprintf("/v1/capabilities/jobs/%d/files", id) }

func (b *Broker) jobDir(capability string, id int64) string {
	return filepath.Join(b.o.JobsDir, jobDirName(capability), strconv.FormatInt(id, 10))
}

// expireWaiting fails waiting jobs whose files did not all arrive in time.
func (b *Broker) expireWaiting() {
	rows, err := b.o.DB.Query(`SELECT id, capability, agent, out FROM cap_jobs WHERE status = 'waiting' AND wait_until < ?`, b.o.Now().UnixMilli())
	if err != nil {
		return
	}
	var late []queued
	for rows.Next() {
		var q queued
		if rows.Scan(&q.id, &q.capability, &q.agent, &q.out) == nil {
			late = append(late, q)
		}
	}
	rows.Close()
	for _, q := range late {
		res, err := b.o.DB.Exec(`UPDATE cap_jobs SET status = 'failed', error = ?, finished = ? WHERE id = ? AND status = 'waiting'`,
			fmt.Sprintf("its files did not arrive within %s", pendingWait), b.o.Now().UnixMilli(), q.id)
		if n, _ := res.RowsAffected(); err == nil && n == 1 {
			_ = os.RemoveAll(b.jobDir(q.capability, q.id))
		}
	}
}

var uploadName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,119}$`)

// Upload takes one of the files a waiting job of agent's waits for, straight into the job's folder
// (never the shared space): at most the hand-over's size. The last one queues the job.
func (b *Broker) Upload(agent string, id int64, name string, body io.Reader, size int64) (*JobView, error) {
	j, err := b.Job(agent, id)
	if err != nil {
		return nil, err
	}
	if j.Status != "waiting" || j.Pending <= 0 {
		return nil, toolErr("job %d is %s: it waits for no files", id, j.Status)
	}
	name = strings.TrimSpace(name)
	if !uploadName.MatchString(name) || strings.Contains(name, "..") {
		return nil, toolErr("name is a plain file name (letters, digits, spaces, . _ -; up to 120)")
	}
	for _, have := range append(append([]string{}, j.Uploads...), baseNames(j.Inputs)...) {
		if strings.EqualFold(have, name) {
			return nil, toolErr("job %d already has a file named %s", id, name)
		}
	}
	if size > maxStageSize {
		return nil, toolErr("a file is at most %d GB", maxStageSize>>30)
	}
	in := filepath.Join(b.jobDir(j.Capability, id), "in")
	if err := os.MkdirAll(in, 0o777); err != nil {
		return nil, err
	}
	_ = os.Chmod(in, 0o777)
	tmp, err := os.CreateTemp(in, ".upload-*")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(tmp, io.LimitReader(body, maxStageSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxStageSize {
		err = toolErr("a file is at most %d GB", maxStageSize>>30)
	}
	if err == nil && size >= 0 && n != size {
		err = toolErr("the upload ended after %d of %d bytes", n, size)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	b.mu.Lock()
	defer b.mu.Unlock()
	// recorded under the lock: two uploads arriving together never both take the last slot
	cur, err := b.Job(agent, id)
	if err != nil || cur.Status != "waiting" || cur.Pending <= 0 {
		os.Remove(tmp.Name())
		return nil, toolErr("job %d no longer waits for files", id)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(in, name)); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	uploads, _ := json.Marshal(append(cur.Uploads, name))
	status := "waiting"
	if cur.Pending == 1 {
		status = "queued"
	}
	if _, err := b.o.DB.Exec(`UPDATE cap_jobs SET uploads = ?, pending = pending - 1, status = ? WHERE id = ?`, string(uploads), status, id); err != nil {
		return nil, err
	}
	b.o.DB.Audit("agent:"+agent, "cap.upload", j.Capability, fmt.Sprintf("job %d: %d bytes", id, n))
	if status == "queued" {
		b.poke()
	}
	return b.Job(agent, id)
}

func baseNames(paths []string) []string {
	out := []string{}
	for _, p := range paths {
		out = append(out, path.Base(p))
	}
	return out
}

// Wake boots a local service capability now (the owner's), in the background.
func (b *Broker) Wake(name string) (link.ServiceState, error) {
	c, ok := b.o.Inst.CapabilityNamed(name)
	if !ok {
		return link.ServiceState{}, fmt.Errorf("no capability %q", name)
	}
	if c.Kind != "service" || c.Placement != "local" {
		return link.ServiceState{}, fmt.Errorf("%s is not a service on this host: its worker starts with each job", name)
	}
	if c.Unavailable != "" {
		return link.ServiceState{}, fmt.Errorf("%s cannot run here: %s", name, c.Unavailable)
	}
	go func() {
		if err := b.image(c); err != nil {
			log.Printf("capabilities: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.BootMinutes)*time.Minute+time.Minute)
		defer cancel()
		if _, err := b.o.Services.Ensure(ctx, b.spec(c)); err != nil {
			log.Printf("capabilities: waking %s: %v", name, err)
		}
	}()
	return b.o.Services.State(name), nil
}

// Sleep stops a local service capability now (the owner's); refused while jobs run in it.
func (b *Broker) Sleep(name string) (link.ServiceState, error) {
	c, ok := b.o.Inst.CapabilityNamed(name)
	if !ok {
		return link.ServiceState{}, fmt.Errorf("no capability %q", name)
	}
	if c.Kind != "service" || c.Placement != "local" {
		return link.ServiceState{}, fmt.Errorf("%s is not a service on this host", name)
	}
	if err := b.o.Services.Sleep(name); err != nil {
		return b.o.Services.State(name), err
	}
	return b.o.Services.State(name), nil
}

// workerState is how a capability's worker is now: asleep | booting | awake for a service on this
// host, "runner" for one on a runner placement, "" when each job starts its own.
func (b *Broker) workerState(c instance.Capability) string {
	switch {
	case c.Kind != "service":
		return ""
	case c.Placement != "local":
		return "runner"
	}
	return b.o.Services.State(c.Name).State
}

// CapabilityView is a capability in the owner's view: how busy, its worker, who may use it.
type CapabilityView struct {
	instance.Capability
	Queued  int      `json:"queued"`
	Running int      `json:"running"`
	Agents  []string `json:"agents"`
	// Worker: asleep | booting | awake (a service here), runner, or "" (a worker per job).
	Worker  string             `json:"worker,omitempty"`
	Service *link.ServiceState `json:"service,omitempty"`
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
		v := CapabilityView{Capability: c, Queued: counts[c.Name][0], Running: counts[c.Name][1], Agents: []string{}, Worker: b.workerState(c)}
		if v.Worker != "" && v.Worker != "runner" {
			st := b.o.Services.State(c.Name)
			v.Service = &st
		}
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

// capabilitySummary is a capability as cap_list shows it.
func (b *Broker) capabilitySummary(c instance.Capability) map[string]any {
	m := map[string]any{"name": c.Name, "description": c.Description, "files": c.Files}
	if st := b.workerState(c); st != "" {
		m["worker"] = st
	}
	return m
}

// Describe is everything an agent needs to call one of its capabilities.
func (b *Broker) Describe(agent, capability string) (map[string]any, error) {
	c, err := b.granted(agent, capability)
	if err != nil {
		return nil, err
	}
	m := b.capabilitySummary(c)
	if c.Docs != "" {
		m["docs"] = c.Docs
	}
	if c.InputSchema != nil {
		m["input_schema"] = c.InputSchema
	}
	if len(c.Examples) > 0 {
		m["examples"] = c.Examples
	}
	m["concurrency"] = c.Concurrency
	m["timeout_minutes"] = c.TimeoutMinutes
	if c.CostUSD > 0 {
		m["cost_usd_per_job"] = c.CostUSD
	}
	if c.Kind == "service" {
		m["idle_minutes"] = c.IdleMinutes
	}
	var queued int
	_ = b.o.DB.QueryRow(`SELECT COUNT(*) FROM cap_jobs WHERE capability = ? AND status IN ('queued', 'running')`, c.Name).Scan(&queued)
	m["jobs_ahead"] = queued
	how := "cap_start {capability, input} queues a job and a notice wakes you when it is done; cap_run waits for it up to wait_seconds."
	switch c.Files {
	case "in":
		how += " Hand it files with files: [\"/shared/…\"] (yours: group and your unit), or upload ones you were sent (files_pending, then cap_attach)."
	case "out":
		how += fmt.Sprintf(" Its files go to out (a shared folder of yours; default %s).", b.outDefaultHint(agent, c.Name))
	case "both":
		how += fmt.Sprintf(" Hand it files with files: [\"/shared/…\"] (yours: group and your unit), or upload ones you were sent (files_pending, then cap_attach); its files go to out (a shared folder of yours; default %s).", b.outDefaultHint(agent, c.Name))
	}
	if st := b.workerState(c); st == "asleep" {
		how += " Its worker is asleep: the next job boots it, which takes longer."
	}
	m["how"] = how
	return m, nil
}

func (b *Broker) outDefaultHint(agent, capability string) string {
	unit := b.unitOf(agent)
	if unit == "" {
		unit = "<your unit>"
	}
	return jobOutBase(unit, capability) + "/job-<n>"
}

// unitOf is agent's unit: the fleet's, else its manifest's ("" when neither says).
func (b *Broker) unitOf(agent string) string {
	if b.o.Workdir != nil {
		if a, ok := b.o.Workdir.agent(agent); ok && a.Unit != "" {
			return a.Unit
		}
	}
	return b.o.Unit(agent)
}

// Await waits up to d for one of agent's jobs; one still running gets its notice when it ends.
func (b *Broker) Await(agent string, id int64, d time.Duration) (*JobView, error) {
	deadline := b.o.Now().Add(d)
	for {
		j, err := b.Job(agent, id)
		if err != nil || j.Status == "done" || j.Status == "failed" {
			return j, err
		}
		if !b.o.Now().Before(deadline) {
			res, err := b.o.DB.Exec(`UPDATE cap_jobs SET notify = 1 WHERE id = ? AND status IN ('queued', 'running')`, id)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return b.Job(agent, id) // it just ended
			}
			j.Notify = true
			j.Note = "still " + j.Status + ": a notice wakes you when it ends (or follow it with cap_status)"
			return j, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// maxAwait stays under the 60 seconds engines give one core tool call.
const maxAwait = 45 * time.Second

// CapabilityTools are the agents' MCP tools for the broker.
func CapabilityTools(b *Broker) []MCPTool {
	jobArgs := map[string]any{
		"capability": str("the capability's name (cap_list)"),
		"input":      map[string]any{"type": "object", "description": "what it takes: cap_describe gives its input schema and examples"},
		"files":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "shared paths (files or folders) to hand it, for those that take files"},
		"out":        str("shared folder its files go to, for those that write files (default: one per job in your unit's capabilities/ folder)"),
	}
	startArgs := map[string]any{"notify": boolean("leave you a notice that wakes you when it is done (default true)"),
		"files_pending": integer("files you will upload to the job yourself (a file you were sent, with cap_attach): it waits for them, up to 15 minutes, before it runs")}
	for k, v := range jobArgs {
		startArgs[k] = v
	}
	runArgs := map[string]any{"wait_seconds": integer("how long to wait for it, up to 45 (default 30); past that it keeps running and its notice wakes you")}
	for k, v := range jobArgs {
		runArgs[k] = v
	}
	jobOf := func(c ToolCall) (int64, error) {
		var req struct {
			Job int64 `json:"job"`
		}
		err := decodeArgs(c, &req)
		return req.Job, err
	}
	return []MCPTool{
		{
			Name: "cap_list",
			Description: "The capabilities the core runs for you (transcription, a browser, video renders…): their names, what they do, whether they take or give files " +
				"and whether their worker is awake. cap_describe one before its first use.",
			InputSchema: schema(nil, map[string]any{}),
			Run: func(c ToolCall) (any, error) {
				out := []map[string]any{}
				for _, cp := range b.Granted(c.Agent) {
					out = append(out, b.capabilitySummary(cp))
				}
				return map[string]any{"capabilities": out}, nil
			},
		},
		{
			Name:        "cap_describe",
			Description: "How to use one of your capabilities: its docs, input schema, examples, files, limits and how busy it is. Reading it never wakes its worker.",
			InputSchema: schema([]string{"capability"}, map[string]any{"capability": str("the capability's name")}),
			Run: func(c ToolCall) (any, error) {
				var req struct {
					Capability string `json:"capability"`
				}
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				return b.Describe(c.Agent, req.Capability)
			},
		},
		{
			Name: "cap_start",
			Description: "Queue a job of one of your capabilities. It runs on its own (its worker boots if asleep); a notice wakes you when it is done, " +
				"and cap_status gives its answer and files. Start several and carry on with other work.",
			InputSchema: schema([]string{"capability"}, startArgs),
			Run: func(c ToolCall) (any, error) {
				var req StartRequest
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				return b.Start(c.Agent, req)
			},
		},
		{
			Name: "cap_run",
			Description: "Run a job of one of your capabilities and wait for it (up to 45 seconds): its answer when it ends in time, " +
				"else the job, still running, with a notice to wake you when it ends. For quick jobs; long ones go through cap_start.",
			InputSchema: schema([]string{"capability"}, runArgs),
			Run: func(c ToolCall) (any, error) {
				var req struct {
					StartRequest
					Wait int `json:"wait_seconds"`
				}
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				if req.FilesPending > 0 {
					return nil, toolErr("a job that waits for uploads starts with cap_start (cap_run cannot wait for them)")
				}
				off := false
				req.Notify = &off
				j, err := b.Start(c.Agent, req.StartRequest)
				if err != nil {
					return nil, err
				}
				wait := time.Duration(req.Wait) * time.Second
				if wait <= 0 {
					wait = 30 * time.Second
				}
				return b.Await(c.Agent, j.ID, min(wait, maxAwait))
			},
		},
		{
			Name:        "cap_status",
			Description: "A job you started: waiting (for the files you upload), queued, running, done (with its answer and the files it wrote) or failed (with why).",
			InputSchema: schema([]string{"job"}, map[string]any{"job": integer("the job number cap_start or cap_run gave")}),
			Run: func(c ToolCall) (any, error) {
				id, err := jobOf(c)
				if err != nil {
					return nil, err
				}
				return b.Job(c.Agent, id)
			},
		},
		{
			Name:        "cap_jobs",
			Description: "Your latest capability jobs and their status (answers through cap_status).",
			InputSchema: schema(nil, map[string]any{"limit": integer("how many (default 20)")}),
			Run: func(c ToolCall) (any, error) {
				var req struct {
					Limit int `json:"limit"`
				}
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				if req.Limit <= 0 {
					req.Limit = 20
				}
				jobs, err := b.AgentJobs(c.Agent, req.Limit)
				return map[string]any{"jobs": jobs}, err
			},
		},
		{
			Name:        "cap_cancel",
			Description: "Cancel a job of yours: a queued one never runs, a running one is stopped.",
			InputSchema: schema([]string{"job"}, map[string]any{"job": integer("the job number")}),
			Run: func(c ToolCall) (any, error) {
				id, err := jobOf(c)
				if err != nil {
					return nil, err
				}
				return b.Cancel(c.Agent, id)
			},
		},
	}
}
