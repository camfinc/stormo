package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestErrorEvents(t *testing.T) {
	var b bytes.Buffer
	old, oldJSON := stdout, *fJSON
	stdout, *fJSON = &b, true
	defer func() { stdout, *fJSON = old, oldJSON }()
	emitError(withCode("instance", errors.New("no stormo.yaml")))
	emitError(fmt.Errorf("wrapped: %w", errUsage))
	emitError(errors.New("boom"))
	want := `{"event":"error","msg":"no stormo.yaml","code":"instance"}
{"event":"error","msg":"usage: stormo --help","code":"usage"}
{"event":"error","msg":"boom","code":"failed"}
`
	if b.String() != want {
		t.Errorf("got\n%swant\n%s", b.String(), want)
	}
}
