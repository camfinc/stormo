// The decide worker's adapter: typed decisions (yes/no, a choice, a score) about a state, from a
// structured decision model on OpenRouter's Decisions API, for the capability contract
// (docs/capabilities.md): GET /health, POST /run {job, agent, input}. One state per job, or a batch
// of states that share the questions, asked in parallel. The key comes from the environment
// (OPENROUTER_API_KEY, the capability's secrets:); the agent never holds it. Standard library
// only: the image builds it with no module.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultModel is pinned, never an alias: probabilities shift between versions, so callers tune
// their thresholds to one.
const defaultModel = "typesafe/jev-1.14"

const (
	maxQuestions  = 32
	maxStates     = 200
	maxStateBytes = 128 << 10
)

type job struct {
	Job   int64 `json:"job"`
	Input input `json:"input"`
}

type input struct {
	Model     string                     `json:"model"`
	Questions map[string]json.RawMessage `json:"questions"`
	State     json.RawMessage            `json:"state"`
	States    map[string]json.RawMessage `json:"states"`
}

type jobError struct {
	code int
	msg  string
}

func (e jobError) Error() string { return e.msg }

func bad(format string, a ...any) error { return jobError{400, fmt.Sprintf(format, a...)} }

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var j job
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&j); err != nil {
			http.Error(w, "body: "+err.Error(), 400)
			return
		}
		out, err := run(r.Context(), j.Input)
		if err != nil {
			code := 422
			var je jobError
			if errors.As(err, &je) {
				code = je.code
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	log.Printf("decide: listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

var questionName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// check refuses what the API would refuse anyway, with a reason an agent can act on.
func check(in input) error {
	if len(in.Questions) == 0 {
		return bad("questions: at least one, by name ({\"is_bug\": {\"type\": \"noul\", ...}})")
	}
	if len(in.Questions) > maxQuestions {
		return bad("questions: %d, %d at most", len(in.Questions), maxQuestions)
	}
	for name, raw := range in.Questions {
		if !questionName.MatchString(name) {
			return bad("question %q: a name of letters, digits, _ . - (64 at most)", name)
		}
		var q struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return bad("question %s: an object with type, instructions and criteria", name)
		}
		switch q.Type {
		case "noul":
			var c map[string]string
			if len(q.Criteria) > 0 && json.Unmarshal(q.Criteria, &c) != nil {
				return bad("question %s: noul criteria are {\"true\": \"…\", \"false\": \"…\"}", name)
			}
		case "choice":
			var c map[string]string
			if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 {
				return bad("question %s: choice criteria are two or more options, {\"option\": \"when to pick it\"}", name)
			}
		case "score":
			var c []string
			if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 {
				return bad("question %s: score criteria are two or more levels, lowest first", name)
			}
		case "box":
			return bad("question %s: box needs an image, which this capability does not hand over", name)
		default:
			return bad("question %s: type is noul, choice or score", name)
		}
	}
	if (len(in.State) > 0) == (len(in.States) > 0) {
		return bad("give state (one decision) or states (a batch sharing the questions), not both")
	}
	if len(in.States) > maxStates {
		return bad("states: %d, %d at most a job", len(in.States), maxStates)
	}
	states := in.States
	if len(in.State) > 0 {
		states = map[string]json.RawMessage{"": in.State}
	}
	for id, s := range states {
		if len(s) > maxStateBytes {
			return bad("state %s: %d bytes, %d at most (past ~32 KB the model reads slower and less accurately)", id, len(s), maxStateBytes)
		}
		if t := strings.TrimSpace(string(s)); t == "null" || t == `""` || t == "{}" || t == "[]" {
			return bad("state %s: empty", id)
		}
	}
	return nil
}

type decision struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens int      `json:"input_tokens"`
		Cost        *float64 `json:"cost"`
	} `json:"usage"`
}

func run(ctx context.Context, in input) (map[string]any, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil, bad("no OPENROUTER_API_KEY: add secrets: [OPENROUTER_API_KEY] to this capability in stormo.yaml (the value comes from the secrets file)")
	}
	if err := check(in); err != nil {
		return nil, err
	}
	model := in.Model
	if model == "" {
		model = defaultModel
	}
	if len(in.State) > 0 {
		d, err := ask(ctx, key, model, in.State, in.Questions)
		if err != nil {
			return nil, err
		}
		return map[string]any{"model": d.Model, "answers": d.Answers, "input_tokens": d.Usage.InputTokens, "cost_usd": d.Usage.Cost}, nil
	}

	ids := make([]string, 0, len(in.States))
	for id := range in.States {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := make(map[string]any, len(ids))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		cost     float64
		tokens   int
		failed   int
		firstErr error
		snapshot string
	)
	slots := make(chan struct{}, parallel())
	for _, id := range ids {
		wg.Add(1)
		slots <- struct{}{}
		go func(id string) {
			defer func() { <-slots; wg.Done() }()
			d, err := ask(ctx, key, model, in.States[id], in.Questions)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				results[id] = map[string]any{"error": err.Error()}
				return
			}
			snapshot = d.Model
			tokens += d.Usage.InputTokens
			if d.Usage.Cost != nil {
				cost += *d.Usage.Cost
			}
			results[id] = map[string]any{"answers": d.Answers}
		}(id)
	}
	wg.Wait()
	if failed == len(ids) {
		return nil, firstErr // nothing decided: the job fails with why
	}
	return map[string]any{"model": snapshot, "results": results, "decided": len(ids) - failed, "failed": failed,
		"input_tokens": tokens, "cost_usd": cost}, nil
}

// parallel is how many states of a batch are asked at once (DECIDE_PARALLEL, default 8).
func parallel() int {
	if n, err := strconv.Atoi(os.Getenv("DECIDE_PARALLEL")); err == nil && n > 0 && n <= 32 {
		return n
	}
	return 8
}

// retried are the answers worth asking again: rate limits, the API's or the model's own trouble.
var retried = map[int]bool{429: true, 500: true, 502: true, 503: true, 524: true, 529: true}

// backoff is the wait before each retry (two of them).
var backoff = []time.Duration{time.Second, 3 * time.Second}

// ask puts one state's questions to the Decisions API.
func ask(ctx context.Context, key, model string, state json.RawMessage, questions map[string]json.RawMessage) (decision, error) {
	raw, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": questions})
	url := os.Getenv("DECISIONS_API_URL")
	if url == "" {
		url = "https://openrouter.ai/api/alpha/decisions"
	}
	var last error
	for try := 0; ; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return decision{}, ctx.Err()
			case <-time.After(backoff[try-1]):
			}
		}
		d, status, err := post(ctx, url, key, raw)
		if err == nil {
			return d, nil
		}
		last = err
		if ctx.Err() != nil || (status != 0 && !retried[status]) || try == len(backoff) {
			return decision{}, last
		}
	}
}

// post is one call; status is 0 when no answer came (the network), else the HTTP status.
func post(ctx context.Context, url, key string, raw []byte) (decision, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return decision{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return decision{}, 0, fmt.Errorf("the Decisions API is not reachable: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return decision{}, 0, err
	}
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(answer))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return decision{}, resp.StatusCode, jobError{502, fmt.Sprintf("the Decisions API answered %d: %s", resp.StatusCode, msg)}
	}
	var d decision
	if err := json.Unmarshal(answer, &d); err != nil || len(d.Answers) == 0 || d.Answers[0] != '{' {
		return decision{}, resp.StatusCode, jobError{502, "the Decisions API answered no answers"}
	}
	return d, 200, nil
}
