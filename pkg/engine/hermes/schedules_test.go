package hermes

import (
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

func TestMergeSchedules(t *testing.T) {
	got, err := New(&instance.Instance{}).MergeSchedules([]byte(`{"jobs":[{"id":"a","prompt":"repo"}]}`), []byte(`{"jobs":[{"id":"a","prompt":"stale"},{"id":"b","prompt":"agent-made <x>"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"jobs\": [\n    {\n      \"id\": \"a\",\n      \"prompt\": \"repo\"\n    },\n    {\n      \"id\": \"b\",\n      \"prompt\": \"agent-made <x>\"\n    }\n  ]\n}"
	if string(got) != want {
		t.Errorf("got %s", got)
	}
}
