// Package loop is the agent loop (docs/needletail.md §4.4): one turn sends the conversation to the
// model, runs the tools it calls, sends the results back, and repeats until the model answers
// without calling a tool or the turn limit is reached. When the conversation nears the model's
// context window, older rows are summarised into a new session (the lineage, see sessions).
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Observer hears a turn's steps (the bench's stream-json, the core's hooks).
type Observer interface {
	// ToolStart is before a tool runs; ToolDone after, with its (redacted) result.
	ToolStart(session string, call wire.ToolCall)
	ToolDone(session string, call wire.ToolCall, result string, failed bool, took time.Duration)
	// Text is an assistant message's text, once the model has finished it.
	Text(session, text string)
}

// TurnObserver is an Observer that also hears a turn begin (on the session it runs on) and end
// (session is then the tip the turn ended on; err how it failed, nil when it answered).
type TurnObserver interface {
	TurnStart(session string)
	TurnEnd(session string, err error)
}

// observers is every observer of one turn, called in order.
type observers []Observer

func (o observers) ToolStart(session string, call wire.ToolCall) {
	for _, x := range o {
		x.ToolStart(session, call)
	}
}

func (o observers) ToolDone(session string, call wire.ToolCall, result string, failed bool, took time.Duration) {
	for _, x := range o {
		x.ToolDone(session, call, result, failed, took)
	}
}

func (o observers) Text(session, text string) {
	for _, x := range o {
		x.Text(session, text)
	}
}

func (o observers) TurnStart(session string) {
	for _, x := range o {
		if t, ok := x.(TurnObserver); ok {
			t.TurnStart(session)
		}
	}
}

func (o observers) TurnEnd(session string, err error) {
	for _, x := range o {
		if t, ok := x.(TurnObserver); ok {
			t.TurnEnd(session, err)
		}
	}
}

// Agent runs turns for one agent home.
type Agent struct {
	Cfg   *config.Config
	LLM   *llm.Client
	Store *sessions.Store
	Tools *tools.Registry
	// Env is the template every tool call gets (Session is set per turn).
	Env tools.Env
	Log func(string)
	Now func() time.Time
	// ContextLength overrides the endpoint's context window (0: ask the endpoint).
	ContextLength int
	// Observer hears every turn (the core's hooks), besides a turn's own Options.Observer.
	Observer Observer
	// Refresh, when set, runs as a turn starts, before the tools are offered: the Luau tools are
	// found again, so a tool the agent wrote last turn is offered in this one.
	Refresh func(ctx context.Context)
}

// Options shape one turn.
type Options struct {
	// Tools limits the tools offered (empty: all).
	Tools    []string
	Observer Observer
}

// Result is a finished turn.
type Result struct {
	Text string
	// Session is the conversation's tip after the turn (a compaction moves it).
	Session string
	// Tokens is the total the model reported over the turn's calls.
	Tokens int
	// Steps is the number of model calls.
	Steps int
}

// Compaction thresholds, as fractions of the context window.
const (
	compactAt   = 0.80
	keepRecent  = 0.30
	summaryIn   = 0.50 // most of the window the summary call may use
	charsPerTok = 4
)

// ErrNoSession is a turn on a session that does not exist.
var ErrNoSession = errors.New("session not found")

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(fmt.Sprintf(format, args...))
	}
}

// Run is one turn of session (any session of its lineage: the turn goes to the tip). The caller
// makes sure one turn at a time runs per conversation.
func (a *Agent) Run(ctx context.Context, session, input string, opts Options) (res *Result, err error) {
	tip, ok := a.Store.Tip(session)
	if !ok {
		return nil, ErrNoSession
	}
	var obs observers
	for _, o := range []Observer{a.Observer, opts.Observer} {
		if o != nil {
			obs = append(obs, o)
		}
	}
	obs.TurnStart(tip)
	defer func() {
		end := tip
		if res != nil {
			end = res.Session
		}
		obs.TurnEnd(end, err)
	}()
	history, err := a.Store.Messages(tip)
	if err != nil {
		return nil, err
	}
	// A turn that stopped (a crash, a restart) between the model's tool calls and their results
	// left calls without answers: answer them as interrupted, or every request would be refused.
	if calls := unanswered(history); len(calls) > 0 {
		rows := make([]wire.Message, len(calls))
		for i, c := range calls {
			rows[i] = wire.Message{Role: "tool", ToolCallID: c.ID, Content: interruptedResult}
		}
		stored, err := a.Store.Append(tip, rows...)
		if err != nil {
			return nil, err
		}
		history = append(history, stored...)
		a.logf("session=%s: %d tool calls of an interrupted turn answered as interrupted", tip, len(calls))
	}
	stored, err := a.Store.Append(tip, wire.Message{Role: "user", Content: input})
	if err != nil {
		return nil, err
	}
	history = append(history, stored...)
	res = &Result{Session: tip}
	defer func() {
		if err := a.Store.Sync(res.Session); err != nil {
			a.logf("session sync: %v", err)
		}
	}()

	turn := a.LLM.NewTurn()
	system := a.SystemPrompt()
	if a.Refresh != nil {
		a.Refresh(ctx)
	}
	offered := a.Tools.Filter(opts.Tools)
	var specs []llm.Tool
	for _, t := range offered {
		specs = append(specs, llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	allowed := map[string]bool{}
	for _, t := range offered {
		allowed[t.Name] = true
	}
	env := a.Env
	lastTotal, sinceTotal := 0, 0
	shrunk := false // a context-length refusal was answered with a compaction

	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		final := step >= a.Cfg.Limits.Turns
		window := a.window(ctx, turn)
		estimate := max(estimateTokens(system, history, specs), lastTotal+sinceTotal/charsPerTok)
		if float64(estimate) > compactAt*float64(window) {
			newTip, newHistory, err := a.compact(ctx, turn, res.Session, history, window, int(keepRecent*float64(window)))
			if err != nil {
				a.logf("compaction skipped: %v", err)
			} else {
				res.Session, history = newTip, newHistory
				lastTotal, sinceTotal = 0, 0
			}
		}
		req := llm.Request{Messages: toLLM(system, history)}
		if final {
			req.Messages = append(req.Messages, llm.Message{Role: "system", Content: "You have reached this turn's tool-call limit. Answer now with what you have; do not call tools."})
		} else {
			req.Tools = specs
		}
		resp, err := turn.Complete(ctx, req)
		if err != nil {
			// The estimate was low and the model refused the request as too long: compact as
			// much as can be (the request's own size is the window it did not fit) and retry
			// the step, once per turn.
			if !shrunk && llm.ContextExceeded(err) {
				shrunk = true
				newTip, newHistory, cerr := a.compact(ctx, turn, res.Session, history, min(window, estimate), 0)
				if cerr == nil {
					res.Session, history = newTip, newHistory
					lastTotal, sinceTotal = 0, 0
					step--
					continue
				}
				a.logf("compaction after a context-length refusal skipped: %v", cerr)
			}
			return res, err
		}
		res.Steps++
		res.Tokens += resp.Usage.TotalTokens
		lastTotal, sinceTotal = resp.Usage.TotalTokens, 0
		calls := resp.ToolCalls
		if final {
			calls = nil
		}
		msg := wire.Message{Role: "assistant", Content: resp.Content, ToolCalls: calls}
		if msg.Content == "" && len(calls) == 0 {
			msg.Content = "(no answer)"
		}
		stored, err := a.Store.Append(res.Session, msg)
		if err != nil {
			return res, err
		}
		history = append(history, stored...)
		if resp.Content != "" {
			obs.Text(res.Session, resp.Content)
		}
		if len(calls) == 0 {
			res.Text = msg.Content
			return res, nil
		}
		env.Session = res.Session
		for _, c := range calls {
			seen := observed(a.Tools, c)
			obs.ToolStart(res.Session, seen)
			started := time.Now()
			var out string
			var failed bool
			var touched []string
			env.Touched = func(p string) { touched = append(touched, p) }
			if allowed[c.Name] {
				out, failed = a.Tools.Call(ctx, &env, c.Name, c.Arguments)
			} else {
				out, failed = fmt.Sprintf(`{"error":"tool %q is not available in this turn"}`, c.Name), true
			}
			env.Touched = nil
			obs.ToolDone(res.Session, withPaths(seen, touched), out, failed, time.Since(started))
			stored, err := a.Store.Append(res.Session, wire.Message{Role: "tool", ToolCallID: c.ID, Content: out})
			if err != nil {
				return res, err
			}
			history = append(history, stored...)
			sinceTotal += len(out)
		}
	}
}

// observed is the call as observers see it: under the tool's hook name (luau:<tool>).
func observed(reg *tools.Registry, c wire.ToolCall) wire.ToolCall {
	if t, ok := reg.Get(c.Name); ok {
		c.Name = t.ObservedName()
	}
	return c
}

// withPaths names the /shared paths a tool wrote through the core in its arguments, as the hooks
// send them (path, and paths when there are more), so the core attributes the writes. A call
// whose arguments already name a path keeps it.
func withPaths(c wire.ToolCall, touched []string) wire.ToolCall {
	if len(touched) == 0 {
		return c
	}
	args := map[string]any{}
	_ = json.Unmarshal([]byte(c.Arguments), &args)
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args["path"]; !ok {
		args["path"] = touched[0]
	}
	if len(touched) > 1 {
		args["paths"] = touched
	}
	if b, err := json.Marshal(args); err == nil {
		c.Arguments = string(b)
	}
	return c
}

func (a *Agent) window(ctx context.Context, turn *llm.Turn) int {
	if a.ContextLength > 0 {
		return a.ContextLength
	}
	return a.LLM.ContextLength(ctx, turn.Endpoint())
}

// interruptedResult answers a tool call whose turn stopped before its result was stored.
const interruptedResult = `{"error":"interrupted"}`

// unanswered is the tool calls of history's last assistant row that have no result after it, when
// only tool rows follow that row: what a turn that stopped mid-tools leaves.
func unanswered(history []wire.Message) []wire.ToolCall {
	var open []wire.ToolCall
	for _, m := range history {
		switch {
		case m.Role == "tool":
			for i, c := range open {
				if c.ID == m.ToolCallID {
					open = append(open[:i:i], open[i+1:]...)
					break
				}
			}
		case len(m.ToolCalls) > 0:
			open = append([]wire.ToolCall(nil), m.ToolCalls...)
		default:
			open = nil
		}
	}
	return open
}

// toLLM is the request's messages: the system prompt, then history made well formed. Every tool
// call is followed by its result (a call a stopped turn left unanswered, in a session written
// before Run answered them, gets the interrupted result), and a tool row that answers no call
// right before it is left out: providers refuse either.
func toLLM(system string, history []wire.Message) []llm.Message {
	out := []llm.Message{{Role: "system", Content: system}}
	var open []string
	flush := func() {
		for _, id := range open {
			out = append(out, llm.Message{Role: "tool", ToolCallID: id, Content: interruptedResult})
		}
		open = nil
	}
	for _, m := range history {
		if m.Role == "tool" {
			for i, id := range open {
				if id == m.ToolCallID {
					open = append(open[:i:i], open[i+1:]...)
					out = append(out, llm.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID})
					break
				}
			}
			continue
		}
		flush()
		out = append(out, llm.Message{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID})
		for _, c := range m.ToolCalls {
			open = append(open, c.ID)
		}
	}
	flush()
	return out
}

// estimateTokens is a character count over four: rough, and only used against a threshold.
func estimateTokens(system string, history []wire.Message, specs []llm.Tool) int {
	n := len(system)
	for _, m := range history {
		n += rowChars(m)
	}
	for _, s := range specs {
		n += len(s.Name) + len(s.Description) + len(s.Parameters)
	}
	return n / charsPerTok
}

func rowChars(m wire.Message) int {
	n := len(m.Content) + 16
	for _, c := range m.ToolCalls {
		n += len(c.Name) + len(c.Arguments) + len(c.ID)
	}
	return n
}

// SystemPrompt is, in order: SOUL.md, the MEMORY block, the USER block, the skills index, the
// shared-space instructions and the current time (§4.4).
func (a *Agent) SystemPrompt() string {
	var b strings.Builder
	soul, err := os.ReadFile(filepath.Join(a.Cfg.Home, "SOUL.md"))
	if err == nil {
		b.WriteString(strings.TrimSpace(string(soul)))
		b.WriteString("\n\n")
	}
	if a.Env.Memory != nil {
		for _, t := range []struct{ target, title, about string }{
			{memory.TargetMemory, "MEMORY", "your notes about the work"},
			{memory.TargetUser, "USER PROFILE", "what you know about the people you work with"},
		} {
			u, _ := a.Env.Memory.Current(t.target)
			fmt.Fprintf(&b, "══ %s (%s; %s used; change it with the memory tool) ══\n", t.title, t.about, u)
			if block := a.Env.Memory.Block(t.target); block != "" {
				b.WriteString(block)
			} else {
				b.WriteString("(empty)")
			}
			b.WriteString("\n\n")
		}
	}
	if idx := skills.Index(a.Cfg.Home); len(idx) > 0 {
		b.WriteString("## Skills\nBefore a task a skill covers, load it with skill_view and follow it. " +
			"When you work out how to do something well that no skill covers, or a skill was wrong or incomplete, " +
			"save it with skill_manage so it is there next time.\n")
		b.WriteString(skills.PromptIndex(idx, skills.PromptIndexBytes))
		b.WriteString("\n")
	}
	if roots := a.Cfg.SharedRoots(); len(roots) > 0 {
		b.WriteString("## Shared documents\n")
		fmt.Fprintf(&b, "Files other agents and people share live in %s (everyone)", roots[0])
		if len(roots) > 1 {
			fmt.Fprintf(&b, " and %s (your unit)", roots[1])
		}
		b.WriteString(". Read and write them with the file tools; never use another unit's directory.\n\n")
	}
	loc := a.Cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	now := a.now().In(loc)
	fmt.Fprintf(&b, "## Now\nThe current time is %s (%s, %s).\n", now.Format(time.RFC3339), now.Weekday(), loc)
	return b.String()
}

// compact summarises the older part of history into a new session (sessions.Store.Compact) and
// returns the new tip with its rows. The cut is at a user or assistant row, never inside a block
// of tool results, so the model never sees a call without its answer or an answer without its
// call.
func (a *Agent) compact(ctx context.Context, turn *llm.Turn, tip string, history []wire.Message, window, keep int) (string, []wire.Message, error) {
	cut := cutPoint(history, keep)
	if cut <= 0 {
		return "", nil, errors.New("no point to cut the conversation at")
	}
	summary := a.summarise(ctx, turn, history[:cut], window)
	newTip, err := a.Store.Compact(tip, summary, history[cut:])
	if err != nil {
		return "", nil, err
	}
	if a.Env.Skills != nil {
		a.Env.Skills.Moved(tip, newTip) // the terminal's SKILL_DIR follows the conversation
	}
	rows, err := a.Store.Messages(newTip)
	if err != nil {
		return "", nil, err
	}
	a.logf("compacted session=%s into=%s rows=%d kept=%d", tip, newTip, cut, len(history)-cut)
	return newTip, rows, nil
}

// cutPoint is the earliest valid cut whose kept tail fits keep tokens, else the latest valid cut
// (0 when there is none). A valid cut starts at a user or assistant row and leaves something to
// summarise. Tool results directly follow their calls, so every call before such a row has its
// result before it too, or never will (a stopped turn's: toLLM answers it as interrupted); a call
// left open that way must not block every later cut.
func cutPoint(history []wire.Message, keep int) int {
	valid := []int{}
	summarisable := false
	for i, m := range history {
		if i > 0 && summarisable && (m.Role == "user" || m.Role == "assistant") {
			valid = append(valid, i)
		}
		if m.Role != "system" {
			summarisable = true
		}
	}
	if len(valid) == 0 {
		return 0
	}
	tail := make([]int, len(history)+1)
	for i := len(history) - 1; i >= 0; i-- {
		tail[i] = tail[i+1] + rowChars(history[i])/charsPerTok
	}
	for _, i := range valid {
		if tail[i] <= keep {
			return i
		}
	}
	return valid[len(valid)-1]
}

const summaryPrompt = `You compact a conversation between an agent and the people it works with, so the agent can carry on without the full history. Write a concise summary in the conversation's language covering: what was asked, decisions and facts established (names, numbers, ids, file paths), what the agent did and found (tools run and outcomes), what is still open or promised, and anything the people said about how they want things done. Do not invent anything. Do not include secrets or credentials. Output only the summary.`

// summarise asks the model for a summary of rows; when it cannot, a plain note keeps the
// conversation going (the rows themselves stay in the ended session).
func (a *Agent) summarise(ctx context.Context, turn *llm.Turn, rows []wire.Message, window int) string {
	text := render(rows, int(summaryIn*float64(window))*charsPerTok)
	resp, err := turn.Complete(ctx, llm.Request{Messages: []llm.Message{
		{Role: "system", Content: summaryPrompt},
		{Role: "user", Content: "Conversation to summarise:\n\n" + text},
	}})
	if err == nil && strings.TrimSpace(resp.Content) != "" {
		return "Summary of the earlier part of this conversation (compacted):\n\n" + strings.TrimSpace(resp.Content)
	}
	a.logf("summary failed: %v", err)
	return fmt.Sprintf("The earlier part of this conversation (%d messages) was compacted and no summary could be made. Ask the person to restate anything you need from it.", len(rows))
}

// render is rows as plain text for the summary call, keeping the most recent part within limit.
func render(rows []wire.Message, limit int) string {
	parts := make([]string, 0, len(rows))
	for _, m := range rows {
		var s string
		switch m.Role {
		case "tool":
			s = "TOOL RESULT: " + clip(m.Content, 2000)
		case "assistant":
			s = "ASSISTANT: " + m.Content
			for _, c := range m.ToolCalls {
				s += fmt.Sprintf("\n[called %s %s]", c.Name, clip(c.Arguments, 500))
			}
		case "system":
			s = "EARLIER SUMMARY: " + m.Content
		default:
			s = strings.ToUpper(m.Role) + ": " + m.Content
		}
		parts = append(parts, s)
	}
	out := strings.Join(parts, "\n\n")
	if limit > 0 && len(out) > limit {
		out = "…\n" + out[len(out)-limit:]
	}
	return out
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
