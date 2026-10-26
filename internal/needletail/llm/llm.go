// Package llm is the runtime's model client: OpenAI chat completions over streaming SSE, with the
// fallback chain of docs/needletail.md §4.3 (the core's gateway first when the agent runs on it,
// then the cloud provider). Prompts and completions are never logged.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Endpoint is one chat-completions endpoint with its key resolved.
type Endpoint struct {
	BaseURL string
	Key     string
	Model   string
}

// Message is one chat message as the model sees it.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []wire.ToolCall
	ToolCallID string
}

// Tool is a function the model may call; Parameters is its JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request is one completion.
type Request struct {
	Messages []Message
	Tools    []Tool
	// MaxTokens bounds auxiliary jobs such as proposal-only reflection (0: endpoint default).
	MaxTokens int
}

// Usage is a completion's token counts.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is a finished completion. Tool call ids are the model's own, kept verbatim (the core
// replays encrypted reasoning by them).
type Response struct {
	Content      string
	Reasoning    string
	ToolCalls    []wire.ToolCall
	FinishReason string
	Usage        Usage
	// Endpoint is the index of the endpoint that answered.
	Endpoint int
}

// Client calls the endpoints in order.
type Client struct {
	Endpoints []Endpoint
	// Reasoning is sent as reasoning_effort when set.
	Reasoning string
	HTTP      *http.Client
	// Log gets one line per failed attempt (status and code, never bodies).
	Log func(string)
	// Sleep waits between retries (tests make it instant).
	Sleep func(ctx context.Context, d time.Duration) error
	// MaxWait caps one wait for a rate limit or a busy core.
	MaxWait time.Duration
	// IdleTimeout is the longest silence inside a stream; AttemptTimeout bounds one attempt.
	IdleTimeout    time.Duration
	AttemptTimeout time.Duration
	// DefaultContext is the context length when an endpoint does not say.
	DefaultContext int

	mu      sync.Mutex
	context map[string]contextEntry
}

// contextEntry is a cached context length; a failed lookup (a core not signed in yet) is retried
// after contextRetry instead of keeping the default for the process' life.
type contextEntry struct {
	n  int
	ok bool
	at time.Time
}

const contextRetry = 5 * time.Minute

// Defaults for a zero Client.
const (
	defaultMaxWait        = 60 * time.Second
	defaultIdle           = 5 * time.Minute
	defaultAttempt        = 15 * time.Minute
	defaultContext        = 128_000
	rateLimitRetries      = 2
	busyRetries           = 3
	maxSSELine            = 16 << 20
	maxErrorBody          = 64 << 10
	codeUsageLimitReached = "usage_limit_reached"
	codeCoreBusy          = "core_busy"
)

// Turn is one agent turn's view of the chain: once a turn has moved to a fallback it stays there,
// and the next turn starts from the primary again.
type Turn struct {
	c     *Client
	start int
}

// NewTurn starts a turn on the primary endpoint.
func (c *Client) NewTurn() *Turn { return &Turn{c: c} }

// Endpoint is the index of the endpoint the turn is on.
func (t *Turn) Endpoint() int { return t.start }

// Failure is a failed attempt.
type Failure struct {
	Status     int // 0: no HTTP answer (connection error, broken stream)
	Code       string
	Message    string
	RetryAfter time.Duration
	Endpoint   int
}

func (f *Failure) Error() string {
	s := fmt.Sprintf("model endpoint %d", f.Endpoint)
	if f.Status != 0 {
		s += fmt.Sprintf(": HTTP %d", f.Status)
	}
	if f.Code != "" {
		s += " " + f.Code
	}
	if f.Message != "" {
		s += ": " + f.Message
	}
	return s
}

type action int

const (
	fallback action = iota
	retrySame
	fatal
)

// classify is what a failure leads to (§4.3): rate limits and a busy core wait and retry; a 401,
// a plan limit, a connection error, a 5xx or a broken stream move to the next endpoint; any other
// 4xx is the request's fault, which another endpoint would refuse too.
func classify(f *Failure) action {
	switch {
	case f.Status == 0, f.Status == 401, f.Status == 403:
		return fallback
	case f.Status == 429 && f.Code == codeUsageLimitReached:
		return fallback
	case f.Status == 429:
		return retrySame
	case f.Status == 503 && f.Code == codeCoreBusy:
		return retrySame
	case f.Status >= 500:
		return fallback
	}
	return fatal
}

// ContextExceeded: err is a refusal of a request too long for the model's context window
// (OpenAI's code context_length_exceeded, or a provider's message saying as much).
func ContextExceeded(err error) bool {
	var f *Failure
	if !errors.As(err, &f) || (f.Status != 400 && f.Status != 413) {
		return false
	}
	if f.Code == "context_length_exceeded" {
		return true
	}
	m := strings.ToLower(f.Message)
	return strings.Contains(m, "context length") || strings.Contains(m, "context window") || strings.Contains(m, "maximum context")
}

// Complete runs one completion along the chain.
func (t *Turn) Complete(ctx context.Context, req Request) (*Response, error) {
	c := t.c
	if len(c.Endpoints) == 0 {
		return nil, errors.New("no model endpoint configured")
	}
	var last error
	for i := t.start; i < len(c.Endpoints); i++ {
		ep := c.Endpoints[i]
		limited, busy := 0, 0
		for {
			resp, f := c.attempt(ctx, i, ep, req)
			if f == nil {
				t.start = i
				resp.Endpoint = i
				return resp, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = f
			c.logf("llm endpoint=%d status=%d code=%s", i, f.Status, f.Code)
			act := classify(f)
			if act == retrySame {
				wait := f.RetryAfter
				if f.Status == 429 {
					limited++
					if limited > rateLimitRetries {
						act = fallback
					}
				} else {
					busy++
					if busy > busyRetries {
						act = fallback
					}
					if wait == 0 {
						wait = time.Duration(1<<busy) * time.Second
					}
				}
				if act == retrySame {
					if wait <= 0 {
						wait = time.Second
					}
					if m := c.maxWait(); wait > m {
						wait = m
					}
					if err := c.sleep(ctx, wait); err != nil {
						return nil, err
					}
					continue
				}
			}
			if act == fatal {
				return nil, f
			}
			break // fallback
		}
	}
	return nil, last
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

func (c *Client) maxWait() time.Duration {
	if c.MaxWait > 0 {
		return c.MaxWait
	}
	return defaultMaxWait
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// ---- wire ------------------------------------------------------------------------------------

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Tools           []chatTool    `json:"tools,omitempty"`
	Stream          bool          `json:"stream"`
	StreamOptions   any           `json:"stream_options,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

func (c *Client) body(ep Endpoint, req Request) ([]byte, error) {
	out := chatRequest{Model: ep.Model, Stream: true, StreamOptions: map[string]bool{"include_usage": true}, ReasoningEffort: c.Reasoning, MaxTokens: req.MaxTokens}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		content := m.Content
		if content != "" || len(m.ToolCalls) == 0 {
			cm.Content = &content
		}
		for _, tc := range m.ToolCalls {
			ct := chatToolCall{ID: tc.ID, Type: "function"}
			ct.Function.Name, ct.Function.Arguments = tc.Name, tc.Arguments
			if ct.Function.Arguments == "" {
				ct.Function.Arguments = "{}"
			}
			cm.ToolCalls = append(cm.ToolCalls, ct)
		}
		out.Messages = append(out.Messages, cm)
	}
	for _, t := range req.Tools {
		ct := chatTool{Type: "function"}
		ct.Function.Name, ct.Function.Description, ct.Function.Parameters = t.Name, t.Description, t.Parameters
		if len(ct.Function.Parameters) == 0 {
			ct.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, ct)
	}
	return json.Marshal(out)
}

// attempt is one streamed call to one endpoint.
func (c *Client) attempt(parent context.Context, idx int, ep Endpoint, req Request) (*Response, *Failure) {
	body, err := c.body(ep, req)
	if err != nil {
		return nil, &Failure{Endpoint: idx, Status: 400, Code: "encode", Message: err.Error()}
	}
	total := c.AttemptTimeout
	if total <= 0 {
		total = defaultAttempt
	}
	ctx, cancel := context.WithTimeoutCause(parent, total, errors.New("attempt timeout"))
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Failure{Endpoint: idx, Code: "bad_endpoint", Message: err.Error()}
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	if ep.Key != "" {
		hr.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	res, err := c.http().Do(hr)
	if err != nil {
		return nil, &Failure{Endpoint: idx, Code: "connection", Message: errName(err)}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		f := failureFrom(res)
		f.Endpoint = idx
		return nil, f
	}
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = defaultIdle
	}
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()
	resp, f := readStream(res.Body, func() { timer.Reset(idle) })
	if f != nil {
		f.Endpoint = idx
		if ctx.Err() != nil && parent.Err() == nil {
			f.Code, f.Message = "timeout", "stream stalled or ran too long"
		}
		return nil, f
	}
	return resp, nil
}

// errName names a transport error without its URL (which can carry nothing secret here, but keeps
// logs short).
func errName(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

// failureFrom reads an error answer: {"error":{"code","message","resets_in_seconds"}} and the
// Retry-After header, as the core gateway and OpenAI write them.
func failureFrom(res *http.Response) *Failure {
	f := &Failure{Status: res.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	var e struct {
		Error struct {
			Code            any    `json:"code"`
			Type            string `json:"type"`
			Message         string `json:"message"`
			ResetsInSeconds int    `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if s, ok := e.Error.Code.(string); ok {
			f.Code = s
		} else if e.Error.Type != "" {
			f.Code = e.Error.Type
		}
		f.Message = truncate(e.Error.Message, 300)
		if e.Error.ResetsInSeconds > 0 {
			f.RetryAfter = time.Duration(e.Error.ResetsInSeconds) * time.Second
		}
	}
	if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s > 0 {
		f.RetryAfter = time.Duration(s) * time.Second
	}
	return f
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// readStream reads an SSE body to [DONE]. An in-band error frame or a stream that ends without
// [DONE] is a failed attempt; whatever arrived before is discarded.
func readStream(body io.Reader, alive func()) (*Response, *Failure) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	resp := &Response{}
	var content, reasoning strings.Builder
	calls := map[int]*wire.ToolCall{}
	order := []int{}
	last, synth := 0, 0 // the call the latest fragment went to; keys for calls without an index (negative)
	var data []string
	done := false
	flush := func() *Failure {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if strings.TrimSpace(payload) == "[DONE]" {
			done = true
			return nil
		}
		var ch chunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			return &Failure{Code: "bad_chunk", Message: "unparseable stream chunk"}
		}
		if ch.Error != nil {
			f := &Failure{Code: fmt.Sprint(ch.Error.Code), Message: truncate(ch.Error.Message, 300)}
			if f.Code == "<nil>" {
				f.Code = "stream_error"
			}
			return f
		}
		if ch.Usage != nil {
			resp.Usage = *ch.Usage
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			content.WriteString(d.Content)
			reasoning.WriteString(d.ReasoningContent)
			reasoning.WriteString(d.Reasoning)
			for j, tc := range d.ToolCalls {
				// Without an index, a fragment continues the call being streamed unless it
				// brings another id, or is not the delta's first (a delta never carries two
				// fragments of one call).
				var i int
				switch prev := calls[last]; {
				case tc.Index != nil:
					i = *tc.Index
				case j == 0 && prev != nil && (tc.ID == "" || prev.ID == "" || tc.ID == prev.ID):
					i = last
				default:
					synth--
					i = synth
				}
				c, ok := calls[i]
				if !ok {
					c = &wire.ToolCall{}
					calls[i] = c
					order = append(order, i)
				}
				last = i
				if tc.ID != "" {
					c.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.Name = tc.Function.Name
				}
				c.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.FinishReason = *choice.FinishReason
			}
		}
		return nil
	}
	for sc.Scan() {
		alive()
		line := sc.Text()
		switch {
		case line == "":
			if f := flush(); f != nil {
				return nil, f
			}
		case strings.HasPrefix(line, ":"): // comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if done {
			break
		}
	}
	if !done {
		if f := flush(); f != nil {
			return nil, f
		}
	}
	if !done {
		msg := "stream ended without [DONE]"
		if err := sc.Err(); err != nil {
			msg = "stream read: " + errName(err)
		}
		return nil, &Failure{Code: "incomplete_stream", Message: msg}
	}
	resp.Content, resp.Reasoning = content.String(), reasoning.String()
	prefix := ""
	for n, i := range order {
		c := calls[i]
		if c.ID == "" {
			// The core keys call spans and live calls by id: one unique to this response.
			if prefix == "" {
				prefix = randomID()
			}
			c.ID = fmt.Sprintf("call_%s_%d", prefix, n)
		}
		resp.ToolCalls = append(resp.ToolCalls, *c)
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	return resp, nil
}

// randomID is 12 random hex digits.
func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ContextLength is the endpoint's context window for its model, from GET {base}/models
// (context_length, as the core and OpenRouter list it), cached per process.
func (c *Client) ContextLength(ctx context.Context, idx int) int {
	def := c.DefaultContext
	if def <= 0 {
		def = defaultContext
	}
	if idx < 0 || idx >= len(c.Endpoints) {
		return def
	}
	ep := c.Endpoints[idx]
	key := ep.BaseURL + "\x00" + ep.Model
	c.mu.Lock()
	if e, ok := c.context[key]; ok && (e.ok || time.Since(e.at) < contextRetry) {
		c.mu.Unlock()
		return e.n
	}
	c.mu.Unlock()
	n := c.fetchContext(ctx, ep)
	e := contextEntry{n: n, ok: n > 0, at: time.Now()}
	if !e.ok {
		e.n = def
	}
	c.mu.Lock()
	if c.context == nil {
		c.context = map[string]contextEntry{}
	}
	c.context[key] = e
	c.mu.Unlock()
	return e.n
}

func (c *Client) fetchContext(parent context.Context, ep Endpoint) int {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.BaseURL, "/")+"/models", nil)
	if err != nil {
		return 0
	}
	if ep.Key != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return 0
	}
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&list) != nil {
		return 0
	}
	for _, m := range list.Data {
		if m.ID == ep.Model {
			return m.ContextLength
		}
	}
	return 0
}
