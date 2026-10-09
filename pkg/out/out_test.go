package out

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

func TestJSONEvents(t *testing.T) {
	var b bytes.Buffer
	o := &Writer{JSON: true, W: &b}
	o.Step("building %s", "atlas")
	o.Line("50% done")
	o.AuthURL("https://auth.example/x?a=1&b=2")
	o.Result(map[string]int{"n": 1}, func(io.Writer) { t.Error("text form in json mode") })
	o.Error("instance", "no stormo.yaml")
	want := `{"event":"step","msg":"building atlas"}
{"event":"step","msg":"50% done"}
{"event":"auth_url","url":"https://auth.example/x?a=1&b=2"}
{"event":"result","data":{"n":1}}
{"event":"error","msg":"no stormo.yaml","code":"instance"}
`
	if b.String() != want {
		t.Errorf("got\n%swant\n%s", b.String(), want)
	}
}

func TestTextMode(t *testing.T) {
	var b bytes.Buffer
	o := &Writer{W: &b}
	o.Step("building %s", "atlas")
	o.AuthURL("https://auth.example/x")
	o.Result(map[string]int{"n": 1}, func(w io.Writer) { fmt.Fprintln(w, "done") })
	if b.String() != "building atlas\ndone\n" {
		t.Errorf("%q", b.String())
	}
}
