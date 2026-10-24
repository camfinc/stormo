package needletail

import (
	"os"
	"slices"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

func TestParseBench(t *testing.T) {
	body, err := os.ReadFile("testdata/bench.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	n := New(&instance.Instance{})
	r := n.ParseBench(string(body), 0)
	if r.Text != "Three stale deals." || !slices.Equal(r.Tools, []string{"terminal", "crm__lookup"}) || r.ExitCode != 0 || r.Tokens == nil || *r.Tokens != 1834 || r.Error != "" {
		t.Errorf("run = %+v", r)
	}
	// No result line: the streamed text, the process' exit code.
	r = n.ParseBench("{\"type\":\"text\",\"text\":\"partial\"}\n", 2)
	if r.Text != "partial" || r.ExitCode != 2 || r.Tokens != nil {
		t.Errorf("partial = %+v", r)
	}
	if argv := n.BenchArgv("hi"); !slices.Equal(argv, []string{"needletail", "oneshot", "--format", "stream-json", "-q", "hi"}) {
		t.Errorf("argv = %v", argv)
	}
}
