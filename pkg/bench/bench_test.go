package bench

import (
	"reflect"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
)

func TestParseAndCheck(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	out := `{"type":"system","subtype":"init"}
{"type":"tool_use","name":"skill_view"}
{"type":"text","text":"Hello"}
{"type":"result","exit_code":0,"text":"Confirmed: 90 days.","tokens":{"total":42}}`
	run := hermes.New(inst).ParseBench(out, 0)
	if run.Text != "Confirmed: 90 days." || !reflect.DeepEqual(run.Tools, []string{"skill_view"}) || run.ExitCode != 0 || run.Tokens == nil || *run.Tokens != 42 {
		t.Fatalf("%+v", run)
	}
	if f := Check(Scenario{ID: "x", Expect: Expectation{Matches: "90 days", NoTools: []string{"terminal"}}}, run); len(f) != 0 {
		t.Fatal(f)
	}
	f := Check(Scenario{ID: "x", Expect: Expectation{Silent: true, Tools: []string{"terminal"}}}, run)
	if !reflect.DeepEqual(f, []string{"tool terminal not called", "expected no reply"}) {
		t.Fatal(f)
	}
}

func TestExampleScenariosPassOnTheirMocks(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := LoadScenarios(inst.Root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != "credentials-env-only" {
		t.Fatalf("%+v", ss)
	}
	rs, err := Run(ss, MockRunner)
	if err != nil {
		t.Fatal(err)
	}
	if !rs[0].OK {
		t.Fatal(rs[0].Failures)
	}
	if run, _ := MockRunner(Scenario{ID: "none"}); run.ExitCode != 1 || run.Error == "" {
		t.Fatalf("%+v", run)
	}
}
