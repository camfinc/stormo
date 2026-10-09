// Package out is how CLI commands report: plain text for people, or, with --json, one JSON event per
// line for programs (docs/api.md):
//
//	{"event":"step","msg":"…"}                 progress, the line text mode prints
//	{"event":"auth_url","url":"…"}            a sign-in page the caller should open
//	{"event":"result","data":{…}}             what the command produced (last line on success)
//	{"event":"error","msg":"…","code":"…"}   why it failed (last line on failure)
package out

import (
	"encoding/json"
	"fmt"
	"io"
)

// Writer reports to W, as text or (JSON) as events.
type Writer struct {
	JSON bool
	W    io.Writer
}

// event is one output line; "event" comes first so a reader can dispatch on it.
type event struct {
	Event string `json:"event"`
	Msg   string `json:"msg,omitempty"`
	URL   string `json:"url,omitempty"`
	Code  string `json:"code,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func (o *Writer) emit(e event) {
	enc := json.NewEncoder(o.W) // one line each; URLs keep their & (no HTML escaping)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
}

// Step reports progress: a line of text, or a step event.
func (o *Writer) Step(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if o.JSON {
		o.emit(event{Event: "step", Msg: msg})
		return
	}
	fmt.Fprintln(o.W, msg)
}

// Line is Step for callers that already hold the line (ops.Deps.Log, llm's Print).
func (o *Writer) Line(l string) { o.Step("%s", l) }

// AuthURL reports a sign-in page: an event for programs; people see it in the step lines around it.
func (o *Writer) AuthURL(u string) {
	if o.JSON {
		o.emit(event{Event: "auth_url", URL: u})
	}
}

// Result ends a successful command: the result event with data, or text's output.
func (o *Writer) Result(data any, text func(w io.Writer)) {
	if o.JSON {
		o.emit(event{Event: "result", Data: data})
		return
	}
	if text != nil {
		text(o.W)
	}
}

// Error ends a failed command in JSON mode (text mode leaves errors to the caller's stderr).
func (o *Writer) Error(code, msg string) {
	o.emit(event{Event: "error", Code: code, Msg: msg})
}
