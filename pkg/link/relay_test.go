package link

import (
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
