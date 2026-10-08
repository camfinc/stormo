// Package llm is the core's model gateway: an OpenAI-compatible chat-completions endpoint for
// local agents, served from the core's one "Sign in with ChatGPT" connection.
package llm

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenAI chat-completions ⇄ the public Responses API on a ChatGPT plan ("Sign in with ChatGPT",
// developers.openai.com/siwc/token-sharing-open-source). Plan-usage preview rules applied here:
// store:false + stream:true, `input` always an array, no `system` role items (system and developer
// turns go to `instructions`), no temperature/top_p/max_output_tokens/metadata/…, and custom
// function tools grouped in a namespace (calls then carry `namespace` next to the plain `name`).
// Content parts are always typed; encrypted reasoning is replayed without its `id`.

// ---- chat-completions types (the subset we accept) -------------------------------------------

// ChatToolCall is a chat-completions tool call.
type ChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatMessage is one chat-completions message; Content is a string, an array of parts or null.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ChatTool is a chat-completions tool definition.
type ChatTool struct {
	Type     string `json:"type,omitempty"`
	Function *struct {
		Name        string          `json:"name"`
		Description *string         `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function,omitempty"`
}

// ChatRequest is the accepted subset of a chat-completions request.
type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Tools             []ChatTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	ReasoningEffort   *string         `json:"reasoning_effort,omitempty"`
	Reasoning         *struct {
		Effort *string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

// ---- reasoning effort ------------------------------------------------------------------------

var ladder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Efforts the Responses API accepts for gpt-6-tier models (Hermes CODEX_GPT56_EFFORTS).
var Efforts = []string{"none", "low", "medium", "high", "xhigh", "max"}

// ClampEffort is Hermes' clamp: verbatim when supported, else the nearest weaker supported level
// (never escalate cost; never clamp an enabled ask down to "none"), else the weakest. Unknown names
// return "" (the model default applies) rather than a 400, which would not trigger the agent's fallback.
func ClampEffort(effort string, supported []string) string {
	if supported == nil {
		supported = Efforts
	}
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return ""
	}
	if slices.Contains(supported, e) {
		return e
	}
	i := slices.Index(ladder, e)
	if i < 0 {
		return ""
	}
	var candidates []string
	for _, l := range supported {
		if l != "none" && slices.Contains(ladder, l) {
			candidates = append(candidates, l)
		}
	}
	best, bestIdx := "", -1
	for _, l := range candidates {
		if li := slices.Index(ladder, l); li < i && li > bestIdx {
			best, bestIdx = l, li
		}
	}
	if best != "" {
		return best
	}
	lowest, lowIdx := "", len(ladder)
	for _, l := range candidates {
		if li := slices.Index(ladder, l); li < lowIdx {
			lowest, lowIdx = l, li
		}
	}
	return lowest
}

// ---- reasoning replay cache ------------------------------------------------------------------

// ReasoningCache holds encrypted reasoning items keyed by the call_ids they preceded, so the next
// turn (which comes back as plain chat history) can replay them. In memory only: never on disk,
// never logged.
type ReasoningCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order []string
	m     map[string]cacheEntry
}

type cacheEntry struct {
	items []any
	at    time.Time
}

func NewReasoningCache() *ReasoningCache {
	return &ReasoningCache{max: 2000, ttl: time.Hour, m: map[string]cacheEntry{}}
}

func (c *ReasoningCache) Set(callIDs []string, items []any) {
	if len(items) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range callIDs {
		if _, ok := c.m[id]; ok {
			c.order = slices.DeleteFunc(c.order, func(x string) bool { return x == id })
		}
		c.m[id] = cacheEntry{items, time.Now()}
		c.order = append(c.order, id)
	}
	for len(c.order) > c.max {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *ReasoningCache) Get(callID string) []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[callID]
	if !ok {
		return nil
	}
	if time.Since(e.at) > c.ttl {
		delete(c.m, callID)
		c.order = slices.DeleteFunc(c.order, func(x string) bool { return x == callID })
		return nil
	}
	return e.items
}

func (c *ReasoningCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// ---- request ---------------------------------------------------------------------------------

// ClampCallID: the call_id cap is 64 chars; oversize ids get a stable surrogate so a call and its
// output still pair.
func ClampCallID(id string) string {
	if len([]rune(id)) <= 64 {
		return id
	}
	h := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(h[:])[:32]
}

type part struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// contentParts decodes message content: a string (ok=true, s set) or an array of parts/strings.
func contentParts(raw json.RawMessage) (s string, isString bool, parts []any) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if json.Unmarshal(raw, &s) == nil {
		return s, true, nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return "", false, nil
	}
	for _, el := range arr {
		var str string
		if json.Unmarshal(el, &str) == nil {
			parts = append(parts, str)
			continue
		}
		var p part
		if json.Unmarshal(el, &p) == nil {
			parts = append(parts, p)
		}
	}
	return "", false, parts
}

func textOf(raw json.RawMessage) string {
	s, isString, parts := contentParts(raw)
	if isString {
		return s
	}
	var b strings.Builder
	for _, p := range parts {
		switch x := p.(type) {
		case string:
			b.WriteString(x)
		case part:
			if (x.Type == "text" || x.Type == "input_text" || x.Type == "output_text") && x.Text != nil {
				b.WriteString(*x.Text)
			}
		}
	}
	return b.String()
}

func userParts(raw json.RawMessage) []any {
	s, isString, parts := contentParts(raw)
	out := []any{}
	if isString {
		if s != "" {
			out = append(out, map[string]any{"type": "input_text", "text": s})
		}
		return out
	}
	for _, p := range parts {
		switch x := p.(type) {
		case string:
			out = append(out, map[string]any{"type": "input_text", "text": x})
		case part:
			if (x.Type == "text" || x.Type == "input_text") && x.Text != nil && *x.Text != "" {
				out = append(out, map[string]any{"type": "input_text", "text": *x.Text})
			} else if x.Type == "image_url" || x.Type == "input_image" {
				var url string
				var obj struct {
					URL    string `json:"url"`
					Detail string `json:"detail"`
				}
				if json.Unmarshal(x.ImageURL, &url) != nil && json.Unmarshal(x.ImageURL, &obj) == nil {
					url = obj.URL
				}
				if url != "" {
					item := map[string]any{"type": "input_image", "image_url": url}
					if obj.Detail != "" {
						item["detail"] = obj.Detail
					}
					out = append(out, item)
				}
			}
		}
	}
	return out
}

const DefaultInstructions = "You are a helpful assistant."

// ToolNamespace holds every tool the agent offers (plan usage requires namespaced tools).
const ToolNamespace = "agent"

func ModelSlug(model string) string { return strings.TrimPrefix(model, "openai/") }

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Strict      bool            `json:"strict"`
	Parameters  json.RawMessage `json:"parameters"`
}

// marshalCompact is compact JSON without HTML escaping.
func marshalCompact(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// ToResponsesBody translates a chat-completions request to a Responses body.
func ToResponsesBody(req *ChatRequest, agent string, cache *ReasoningCache) map[string]any {
	// System and developer turns become `instructions` (plan usage rejects `system` role items).
	var sys []string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if t := strings.TrimSpace(textOf(m.Content)); t != "" {
				sys = append(sys, t)
			}
		}
	}
	instructions := strings.Join(sys, "\n\n")
	if instructions == "" {
		instructions = DefaultInstructions
	}

	input := []any{}
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			if parts := userParts(m.Content); len(parts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "user", "content": parts})
			}
		case "assistant":
			var calls []ChatToolCall
			for _, c := range m.ToolCalls {
				if c.Function.Name != "" {
					calls = append(calls, c)
				}
			}
			if len(calls) > 0 && cache != nil {
				if replay := cache.Get(ClampCallID(calls[0].ID)); replay != nil {
					input = append(input, replay...)
				}
			}
			if text := textOf(m.Content); strings.TrimSpace(text) != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			for _, c := range calls {
				args := c.Function.Arguments
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": ClampCallID(c.ID), "namespace": ToolNamespace, "name": c.Function.Name, "arguments": args})
			}
		case "tool":
			if m.ToolCallID == "" {
				continue
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": ClampCallID(m.ToolCallID), "output": textOf(m.Content)})
		}
	}

	tools := []responsesTool{}
	for _, t := range req.Tools {
		f := t.Function
		if f == nil || f.Name == "" {
			continue
		}
		desc := ""
		if f.Description != nil {
			desc = *f.Description
		}
		params := f.Parameters
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, responsesTool{Type: "function", Name: f.Name, Description: desc, Strict: f.Strict != nil && *f.Strict, Parameters: params})
	}

	body := map[string]any{"model": ModelSlug(req.Model), "instructions": instructions, "input": input, "store": false, "stream": true}
	if len(tools) > 0 {
		body["tools"] = []any{map[string]any{"type": "namespace", "name": ToolNamespace, "description": "Tools provided by the calling agent.", "tools": tools}}
		// A forced specific function becomes "required" (a call to some tool): how a namespaced
		// function is named in tool_choice is not documented for plan usage yet.
		choice := "auto"
		var s string
		var obj struct {
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(req.ToolChoice, &s) == nil {
			choice = s
		} else if json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Function != nil && obj.Function.Name != "" {
			choice = "required"
		}
		body["tool_choice"] = choice
		body["parallel_tool_calls"] = req.ParallelToolCalls == nil || *req.ParallelToolCalls
	}

	ask := ""
	if req.ReasoningEffort != nil {
		ask = *req.ReasoningEffort
	} else if req.Reasoning != nil && req.Reasoning.Effort != nil {
		ask = *req.Reasoning.Effort
	}
	effort := ClampEffort(ask, nil)
	if effort == "none" {
		body["reasoning"] = map[string]any{"effort": "none"}
		body["include"] = []any{}
	} else {
		r := map[string]any{"summary": "auto"}
		if effort != "" {
			r["effort"] = effort
		}
		body["reasoning"] = r
		body["include"] = []any{"reasoning.encrypted_content"}
	}

	// Same agent + same system prompt + same tools → same cache key (≤ 64 chars).
	h := sha256.New()
	h.Write([]byte(instructions))
	h.Write([]byte{0})
	h.Write(marshalCompact(tools))
	prefix := agent
	if r := []rune(prefix); len(r) > 24 {
		prefix = string(r[:24])
	}
	body["prompt_cache_key"] = prefix + "-" + hex.EncodeToString(h.Sum(nil))[:32]
	return body
}

// ---- response (SSE) --------------------------------------------------------------------------

// SseEvent is one server-sent event.
type SseEvent struct {
	Event string
	Data  string
}

// SseReader parses a text/event-stream body into events.
type SseReader struct {
	r       io.Reader
	onChunk func()
	buf     string
	eof     bool
	pending []SseEvent
}

func NewSseReader(r io.Reader, onChunk func()) *SseReader { return &SseReader{r: r, onChunk: onChunk} }

// Next returns the next event; io.EOF at the end of the stream.
func (s *SseReader) Next() (SseEvent, error) {
	chunk := make([]byte, 32*1024)
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.eof {
			return SseEvent{}, io.EOF
		}
		n, err := s.r.Read(chunk)
		if n > 0 {
			if s.onChunk != nil {
				s.onChunk()
			}
			s.buf = strings.ReplaceAll(s.buf+string(chunk[:n]), "\r\n", "\n")
			for {
				i := strings.Index(s.buf, "\n\n")
				if i < 0 {
					break
				}
				block := s.buf[:i]
				s.buf = s.buf[i+2:]
				if ev, ok := parseBlock(block); ok {
					s.pending = append(s.pending, ev)
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return SseEvent{}, err
			}
			s.eof = true
			if ev, ok := parseBlock(strings.TrimSpace(s.buf)); ok {
				s.pending = append(s.pending, ev)
			}
			s.buf = ""
		}
	}
}

func parseBlock(block string) (SseEvent, bool) {
	var ev SseEvent
	var data []string
	sc := bufio.NewScanner(strings.NewReader(block))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			ev.Event = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if len(data) == 0 {
		return ev, false
	}
	ev.Data = strings.Join(data, "\n")
	return ev, true
}

// Usage is the chat-completions usage block.
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// UpstreamFailure is a failed call as the agent will see it. RetryAfterSeconds 0 means unset.
type UpstreamFailure struct {
	Status            int
	Code              string
	Message           string
	RetryAfterSeconds int
}

// StreamMapper turns Responses stream events into chat.completion.chunk objects. Stateful:
// tool-call indexes, whether text or arguments were already streamed, reasoning items for the
// replay cache.
type StreamMapper struct {
	ID        string
	Created   int64
	Model     string
	toolIndex map[string]int
	argsSent  map[int]bool
	textSent  map[string]bool
	roleSent  bool
	CallIDs   []string
	Reasoning []any
	// Everything streamed so far, for the non-streaming response.
	Text      string
	ToolCalls []ChatToolCall
	Usage     *Usage
	Status    string
	Failure   *UpstreamFailure
	// True once an output-bearing event arrived (the point after which the HTTP status is committed).
	SawOutput bool
	Done      bool
}

func NewStreamMapper(model string) *StreamMapper {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return &StreamMapper{ID: "chatcmpl-" + hex.EncodeToString(b), Created: time.Now().Unix(), Model: model,
		toolIndex: map[string]int{}, argsSent: map[int]bool{}, textSent: map[string]bool{}, ToolCalls: []ChatToolCall{}}
}

func (m *StreamMapper) FinishReason() string {
	if len(m.ToolCalls) > 0 {
		return "tool_calls"
	}
	if m.Status == "incomplete" {
		return "length"
	}
	return "stop"
}

func (m *StreamMapper) chunk(delta map[string]any, finish any) map[string]any {
	if !m.roleSent {
		delta["role"] = "assistant"
		m.roleSent = true
	}
	return map[string]any{"id": m.ID, "object": "chat.completion.chunk", "created": m.Created, "model": m.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func (m *StreamMapper) text(delta, itemID string) map[string]any {
	if itemID != "" {
		m.textSent[itemID] = true
	}
	m.Text += delta
	return m.chunk(map[string]any{"content": delta}, nil)
}

func (m *StreamMapper) args(index int, delta string) map[string]any {
	m.argsSent[index] = true
	m.ToolCalls[index].Function.Arguments += delta
	return m.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": delta}}}}, nil)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Map maps one upstream event and returns the chunks to send.
func (m *StreamMapper) Map(ev SseEvent) []any {
	var d map[string]any
	if json.Unmarshal([]byte(ev.Data), &d) != nil || d == nil {
		return nil
	}
	typ := str(d["type"])
	if _, has := d["type"]; !has {
		typ = ev.Event
	}
	switch typ {
	case "response.output_item.added":
		m.SawOutput = true
		item := obj(d["item"])
		if str(item["type"]) != "function_call" {
			return nil
		}
		index := len(m.ToolCalls)
		key := str(item["id"])
		if key == "" {
			key = str(item["call_id"])
		}
		m.toolIndex[key] = index
		var tc ChatToolCall
		tc.ID, tc.Type, tc.Function.Name = str(item["call_id"]), "function", str(item["name"])
		m.ToolCalls = append(m.ToolCalls, tc)
		m.CallIDs = append(m.CallIDs, tc.ID)
		out := []any{m.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": ""}}}}, nil)}
		if a := str(item["arguments"]); a != "" {
			out = append(out, m.args(index, a))
		}
		return out
	case "response.output_text.delta":
		m.SawOutput = true
		if dl := str(d["delta"]); dl != "" {
			return []any{m.text(dl, str(d["item_id"]))}
		}
		return nil
	case "response.reasoning_summary_text.delta":
		m.SawOutput = true
		if dl := str(d["delta"]); dl != "" {
			return []any{m.chunk(map[string]any{"reasoning_content": dl}, nil)}
		}
		return nil
	case "response.function_call_arguments.delta":
		index, ok := m.toolIndex[str(d["item_id"])]
		if dl := str(d["delta"]); ok && dl != "" {
			return []any{m.args(index, dl)}
		}
		return nil
	case "response.output_item.done":
		item := obj(d["item"])
		switch str(item["type"]) {
		case "reasoning":
			if enc := item["encrypted_content"]; enc != nil && enc != "" && enc != false {
				summary := item["summary"]
				if summary == nil {
					summary = []any{}
				}
				m.Reasoning = append(m.Reasoning, map[string]any{"type": "reasoning", "summary": summary, "encrypted_content": enc})
			}
		case "function_call":
			key := str(item["id"])
			if key == "" {
				key = str(item["call_id"])
			}
			if index, ok := m.toolIndex[key]; ok && !m.argsSent[index] && str(item["arguments"]) != "" {
				return []any{m.args(index, str(item["arguments"]))}
			}
		case "message":
			if id := str(item["id"]); id != "" && !m.textSent[id] {
				var b strings.Builder
				parts, _ := item["content"].([]any)
				for _, p := range parts {
					if po := obj(p); str(po["type"]) == "output_text" {
						b.WriteString(str(po["text"]))
					}
				}
				if t := b.String(); t != "" {
					return []any{m.text(t, id)}
				}
			}
		}
		return nil
	case "response.completed", "response.incomplete":
		m.Done = true
		resp := obj(d["response"])
		m.Status = str(resp["status"])
		if m.Status == "" {
			m.Status = map[bool]string{true: "incomplete", false: "completed"}[typ == "response.incomplete"]
		}
		m.Usage = mapUsage(resp["usage"])
		return nil
	case "response.failed":
		m.Done = true
		f := failureFromError(obj(obj(d["response"])["error"]), 502)
		m.Failure = &f
		return nil
	case "error":
		m.Done = true
		e := d
		if inner, ok := d["error"].(map[string]any); ok {
			e = inner
		}
		f := failureFromError(e, 502)
		m.Failure = &f
		return nil
	}
	return nil
}

// FinalChunks are the finish chunk and, when known, the usage chunk.
func (m *StreamMapper) FinalChunks() []any {
	out := []any{m.chunk(map[string]any{}, m.FinishReason())}
	if m.Usage != nil {
		out = append(out, map[string]any{"id": m.ID, "object": "chat.completion.chunk", "created": m.Created, "model": m.Model, "choices": []any{}, "usage": m.Usage})
	}
	return out
}

// Completion is the non-streaming chat.completion.
func (m *StreamMapper) Completion() map[string]any {
	msg := map[string]any{"role": "assistant", "content": nil}
	if m.Text != "" {
		msg["content"] = m.Text
	}
	if len(m.ToolCalls) > 0 {
		msg["tool_calls"] = m.ToolCalls
	}
	out := map[string]any{"id": m.ID, "object": "chat.completion", "created": m.Created, "model": m.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": m.FinishReason()}}}
	if m.Usage != nil {
		out["usage"] = m.Usage
	}
	return out
}

// num reads a number or a numeric string; ok is false when it is neither.
func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x)
	case int:
		return float64(x), true
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

func numOr(v any, def float64) float64 {
	if v == nil {
		return def
	}
	f, ok := num(v)
	if !ok {
		return 0
	}
	return f
}

func mapUsage(v any) *Usage {
	u, ok := v.(map[string]any)
	if !ok || u == nil {
		return nil
	}
	prompt := int(numOr(u["input_tokens"], 0))
	completion := int(numOr(u["output_tokens"], 0))
	out := &Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: int(numOr(u["total_tokens"], float64(prompt+completion)))}
	out.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{int(numOr(obj(u["input_tokens_details"])["cached_tokens"], 0))}
	out.CompletionTokensDetails = &struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	}{int(numOr(obj(u["output_tokens_details"])["reasoning_tokens"], 0))}
	return out
}

// ---- errors ----------------------------------------------------------------------------------

var (
	usageLimitCode = regexp.MustCompile(`(?i)usage_limit_reached|usage_limit_exceeded|usage_not_included`)
	usageLimitMsg  = regexp.MustCompile(`(?i)usage limit`)
	rateLimitCode  = regexp.MustCompile(`(?i)rate_limit`)
)

func isUsageLimit(code, message string) bool {
	return usageLimitCode.MatchString(code) || usageLimitMsg.MatchString(message)
}

// Plan-usage error codes (errors-and-recovery) → the status the agent sees. None carries a reset
// time; the gateway applies its own hold to a usage limit.
var planCodes = map[string]UpstreamFailure{
	"subscription_sharing_usage_limit_exceeded": {Status: 429, Code: "usage_limit_reached"},
	"subscription_sharing_usage_unavailable":    {Status: 503, Code: "plan_usage_unavailable", RetryAfterSeconds: 30},
	"subscription_sharing_user_not_eligible":    {Status: 403, Code: "plan_user_not_eligible"},
	"subscription_sharing_route_not_supported":  {Status: 403, Code: "plan_route_not_supported"},
	"subscription_sharing_invalid_user":         {Status: 401, Code: "plan_invalid_user"},
	// Our translation sent something plan usage does not support: a core bug, never retried as is.
	"subscription_sharing_unsupported_capability": {Status: 400, Code: "plan_unsupported_capability"},
}

// jsString is String(v ?? fallback).
func jsString(v any, fallback string) string {
	switch x := v.(type) {
	case nil:
		return fallback
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func firstDefined(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func failureFromError(e map[string]any, fallbackStatus int) UpstreamFailure {
	code := jsString(firstDefined(e["code"], e["type"]), "upstream_error")
	message := truncate(jsString(e["message"], "upstream error"), 500)
	if plan, ok := planCodes[code]; ok {
		plan.Message = message
		return plan
	}
	if isUsageLimit(code, message) || rateLimitCode.MatchString(code) {
		c := "rate_limit_exceeded"
		if isUsageLimit(code, message) {
			c = "usage_limit_reached"
		}
		return UpstreamFailure{Status: 429, Code: c, Message: message, RetryAfterSeconds: resetSeconds(e, "")}
	}
	return UpstreamFailure{Status: fallbackStatus, Code: code, Message: message}
}

func resetSeconds(e map[string]any, retryAfterHeader string) int {
	if v, ok := num(e["resets_in_seconds"]); ok && v > 0 && !math.IsInf(v, 0) {
		return int(math.Ceil(v))
	}
	if v, ok := num(e["resets_at"]); ok && v > 0 && !math.IsInf(v, 0) {
		return max(1, int(math.Ceil(v-float64(time.Now().UnixMilli())/1000)))
	}
	if v, ok := num(retryAfterHeader); ok && v > 0 && !math.IsInf(v, 0) {
		return int(math.Ceil(v))
	}
	return 0
}

// FailureFromResponse classifies a non-2xx upstream response. Bodies are read for codes only and
// never logged.
func FailureFromResponse(res *http.Response) UpstreamFailure {
	e := map[string]any{}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var parsed any
	if json.Unmarshal(body, &parsed) == nil {
		var v any = parsed
		if m, ok := parsed.(map[string]any); ok {
			v = firstDefined(m["error"], m["detail"], m)
		}
		switch x := v.(type) {
		case string:
			e = map[string]any{"message": x}
		case map[string]any:
			e = x
		}
	}
	code := jsString(firstDefined(e["code"], e["type"]), fmt.Sprintf("http_%d", res.StatusCode))
	message := truncate(jsString(e["message"], fmt.Sprintf("upstream HTTP %d", res.StatusCode)), 500)
	if _, ok := planCodes[code]; ok {
		cp := map[string]any{}
		for k, v := range e {
			cp[k] = v
		}
		cp["code"], cp["message"] = code, message
		return failureFromError(cp, 502)
	}
	retryAfter := resetSeconds(e, res.Header.Get("Retry-After"))
	or := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	switch res.StatusCode {
	case 429:
		// A plan limit without a reset hint gets the gateway's hold (RetryAfterSeconds left unset).
		if isUsageLimit(code, message) {
			return UpstreamFailure{Status: 429, Code: "usage_limit_reached", Message: message, RetryAfterSeconds: retryAfter}
		}
		return UpstreamFailure{Status: 429, Code: "rate_limit_exceeded", Message: message, RetryAfterSeconds: or(retryAfter, 60)}
	case 401:
		return UpstreamFailure{Status: 401, Code: "upstream_unauthorized", Message: message}
	case 403:
		return UpstreamFailure{Status: 403, Code: "upstream_forbidden", Message: message}
	case 503:
		return UpstreamFailure{Status: 503, Code: "upstream_unavailable", Message: message, RetryAfterSeconds: or(retryAfter, 30)}
	case 400, 404, 413, 422:
		return UpstreamFailure{Status: res.StatusCode, Code: code, Message: message}
	}
	return UpstreamFailure{Status: 502, Code: code, Message: message}
}

func humanWait(s int) string {
	if s >= 3600 {
		return fmt.Sprintf("%dh %dm", s/3600, int(math.Ceil(float64(s%3600)/60)))
	}
	return fmt.Sprintf("%dm", int(math.Ceil(float64(s)/60)))
}

// WriteError writes an OpenAI-shaped error response. For plan limits it carries what Hermes
// parses to hold its fallback: `usage_limit_reached` in type/code, "usage limit has been reached"
// in the message, `resets_in_seconds`, and a Retry-After header.
func WriteError(w http.ResponseWriter, f UpstreamFailure) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	e := map[string]any{"type": f.Code, "code": f.Code, "message": f.Message}
	if f.Status == 429 {
		s := max(1, f.RetryAfterSeconds)
		if f.RetryAfterSeconds == 0 {
			s = 60
		}
		h.Set("Retry-After", strconv.Itoa(s))
		e["resets_in_seconds"] = s
		if f.Code == "usage_limit_reached" {
			e["message"] = fmt.Sprintf("The ChatGPT plan usage limit has been reached; resets in %s (resets_in_seconds: %d). Manage usage: https://chatgpt.com/settings/usage", humanWait(s), s)
		} else {
			e["message"] = fmt.Sprintf("Rate limited upstream; retry after %d seconds.", s)
		}
	}
	if f.Status == 503 {
		s := f.RetryAfterSeconds
		if s == 0 {
			s = 30
		}
		h.Set("Retry-After", strconv.Itoa(max(1, s)))
	}
	if f.Status == 401 {
		e["type"] = "authentication_error"
	}
	w.WriteHeader(f.Status)
	_, _ = w.Write(marshalCompact(map[string]any{"error": e}))
}
