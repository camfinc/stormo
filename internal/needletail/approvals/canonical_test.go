package approvals

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	props := []string{"message", "target", "opts"}
	for _, c := range []struct{ in, want, err string }{
		{`{"target":"slack","message":"hi <b>&"}`, `{"message":"hi <b>&","target":"slack"}`, ""},
		{`{"message":1.50,"opts":{"b":[1,{"x":2}],"a":null}}`, `{"message":1.50,"opts":{"a":null,"b":[1,{"x":2}]}}`, ""},
		{`{"message":"a","message":"b"}`, "", "twice"},
		{`{"message":"a","MESSAGE":"b"}`, "", "differ only in case"},
		{`{"message":"a","meſſage":"b"}`, "", "differ only in case"},
		{`{"Message":"b"}`, "", "not one the tool takes"},
		{`{"opts":{"k":1,"K":2}}`, "", "differ only in case"},
		{`{"opts":[{"k":1,"k":2}]}`, "", "twice"},
		{`[1]`, "", "not a JSON object"},
		{`{"message":"a"} {"x":1}`, "", "more than one"},
	} {
		got, err := Canonical(json.RawMessage(c.in), props)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err %v, want %q", c.in, err, c.err)
		case c.err == "" && (err != nil || string(got) != c.want):
			t.Errorf("%s: %s (%v), want %s", c.in, got, err, c.want)
		}
	}
	if _, err := Canonical(json.RawMessage(`{"anything":1}`), nil); err != nil {
		t.Errorf("no properties: %v", err)
	}
	if p := SchemaProperties(json.RawMessage(`{"type":"object","properties":{"b":{},"a":{}}}`)); strings.Join(p, ",") != "a,b" {
		t.Errorf("properties %v", p)
	}
	if p := SchemaProperties(json.RawMessage(`{"type":"object","properties":{"a":{}},"additionalProperties":true}`)); p != nil {
		t.Errorf("open schema %v", p)
	}
}
