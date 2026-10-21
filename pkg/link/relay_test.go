package link

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The relay passes on each engine's own hook headers (the core verifies a delivery with the
// signature header its engine names), and nothing else beyond the fixed set.
func TestRelayKeepsEveryEnginesHookHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/ingest/any", nil)
	for k, v := range map[string]string{
		"Content-Type":           "application/json",
		"Authorization":          "Bearer k",
		"X-Hermes-Signature-256": "abc",
		"X-Hermes-Event":         "pre_tool_call",
		"X-Hermes-Delivery":      "d1",
		"X-Other-Signature":      "sha256=def",
		"X-Forwarded-For":        "10.0.0.1",
		"X-Stormo-Placement":     "vps",
		"Cookie":                 "c=1",
		"X-Big-Signature":        string(make([]byte, maxHookHeader+1)),
	} {
		r.Header.Set(k, v)
	}
	want := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer k",
		"X-Hermes-Signature-256": "abc", "X-Hermes-Event": "pre_tool_call", "X-Hermes-Delivery": "d1",
		"X-Other-Signature": "sha256=def"}
	if got := headersOf(r); !reflect.DeepEqual(got, want) {
		t.Errorf("kept %v", got)
	}
}

// A request with many hook-shaped headers passes at most maxHookHeaders of them, signatures
// first, so a flood neither grows the spool nor pushes an engine's signature out.
func TestRelayCapsHookHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/ingest/any", nil)
	for i := range 40 {
		r.Header.Set(fmt.Sprintf("X-Flood%02d-Event", i), "x")
	}
	r.Header.Set("X-Zeta-Signature", "sha256=z")
	r.Header.Set("Content-Type", "application/json")
	got := headersOf(r)
	hooks := 0
	for k := range got {
		if hookHeader.MatchString(k) {
			hooks++
		}
	}
	if hooks != maxHookHeaders || got["X-Zeta-Signature"] != "sha256=z" || got["Content-Type"] != "application/json" {
		t.Errorf("kept %d hook headers: %v", hooks, got)
	}
}
