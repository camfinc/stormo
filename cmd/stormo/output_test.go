package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func capture(t *testing.T, json bool, f func()) string {
	t.Helper()
	var b bytes.Buffer
	old, oldJSON := stdout, *fJSON
	stdout, *fJSON = &b, json
	defer func() { stdout, *fJSON = old, oldJSON }()
	f()
	return b.String()
}

func TestJSONEvents(t *testing.T) {
	got := capture(t, true, func() {
		step("building %s", "atlas")
		result(map[string]int{"n": 1}, func() { t.Error("text form in json mode") })
		emitError(withCode("instance", errors.New("no stormo.yaml")))
		emitError(fmt.Errorf("wrapped: %w", errUsage))
		emitError(errors.New("boom"))
	})
	want := `{"event":"step","msg":"building atlas"}
{"event":"result","data":{"n":1}}
{"event":"error","msg":"no stormo.yaml","code":"instance"}
{"event":"error","msg":"usage: stormo --help","code":"usage"}
{"event":"error","msg":"boom","code":"failed"}
`
	if got != want {
		t.Errorf("got\n%swant\n%s", got, want)
	}
}

func TestTextModeUnchanged(t *testing.T) {
	got := capture(t, false, func() {
		step("building %s", "atlas")
		result(map[string]int{"n": 1}, func() { fmt.Fprintln(stdout, "done") })
	})
	if got != "building atlas\ndone\n" {
		t.Errorf("%q", got)
	}
}
