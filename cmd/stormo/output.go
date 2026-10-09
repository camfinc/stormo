package main

import (
	"errors"
	"io"
	"os"
	"slices"

	"github.com/camfinc/stormo/pkg/out"
)

// Machine output (--json): see pkg/out and docs/api.md.

var stdout io.Writer = os.Stdout

// jsonMode is --json; also before parsing finished, so a bad flag is reported as an error event.
func jsonMode() bool { return *fJSON || slices.Contains(os.Args[1:], "--json") }

// output is the writer every command reports through.
func output() *out.Writer { return &out.Writer{JSON: jsonMode(), W: stdout} }

// step reports progress.
func step(format string, a ...any) { output().Step(format, a...) }

// result ends a successful command: data for --json, text otherwise.
func result(data any, text func(w io.Writer)) { output().Result(data, text) }

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
	output().Error(errorCode(err), msg)
}
