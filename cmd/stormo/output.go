package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// Machine output (--json, docs/api.md): one JSON object per line on stdout, each with an "event":
//
//	{"event":"step","msg":"…"}                 progress, as the text mode prints it
//	{"event":"result","data":{…}}             what the command produced (last line on success)
//	{"event":"error","code":"…","msg":"…"}   why it failed (last line on failure, exit status 1 or 2)
//
// Without --json the same calls print the text the CLI always printed.

var stdout io.Writer = os.Stdout

// jsonMode is --json; also before parsing finished, so a bad flag is reported as an error event.
func jsonMode() bool { return *fJSON || slices.Contains(os.Args[1:], "--json") }

// event is one output line; "event" comes first so a reader can dispatch on it.
type event struct {
	Event string `json:"event"`
	Msg   string `json:"msg,omitempty"`
	Code  string `json:"code,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func emitLine(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(stdout, string(b))
}

// step reports progress: a line of text, or a step event.
func step(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if jsonMode() {
		emitLine(event{Event: "step", Msg: msg})
		return
	}
	fmt.Fprintln(stdout, msg)
}

// result ends a successful command: the result event with data, or the text form.
func result(data any, text func()) {
	if jsonMode() {
		emitLine(event{Event: "result", Data: data})
		return
	}
	if text != nil {
		text()
	}
}

// cliError carries a stable code for the error event; plain errors get "failed".
type cliError struct {
	code string
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func withCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return &cliError{code, err}
}

func errorCode(err error) string {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	if errors.Is(err, errUsage) {
		return "usage"
	}
	return "failed"
}

func emitError(err error) {
	msg := err.Error()
	if errors.Is(err, errUsage) {
		msg = "usage: stormo --help"
	}
	emitLine(event{Event: "error", Code: errorCode(err), Msg: msg})
}
