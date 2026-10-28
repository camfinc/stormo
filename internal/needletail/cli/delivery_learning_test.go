package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

func TestDeliveryLearningUsesSessionRootAndRespectsQuarantine(t *testing.T) {
	home := t.TempDir()
	store, err := sessions.Open(filepath.Join(home, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(wire.Session{ID: "delivery-root", Source: "cron"}); err != nil {
		t.Fatal(err)
	}
	tip, err := store.Compact("delivery-root", "prior result", nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := &loop.Agent{Store: store, Learning: &runtimelearn.Store{Home: home}}
	logs := []string{}
	observe := deliveryLearning(agent, func(line string) { logs = append(logs, line) })
	event := sched.DeliveryEvent{ID: "outbox:failed", JobID: "job-with-private-name", Session: tip, At: time.Now(), Status: "failed"}
	observe(event)
	observe(event)
	events, err := agent.Learning.Events(runtimelearn.Hash("delivery-root"), "conversation:"+runtimelearn.Hash("delivery-root"))
	if err != nil || len(events) != 1 {
		t.Fatalf("session-root evidence: %v %v", events, err)
	}
	if err = store.MarkUntrusted(tip); err != nil {
		t.Fatal(err)
	}
	event.ID = "outbox:recovered"
	event.Status = "recovered"
	observe(event)
	events, _ = agent.Learning.Events(runtimelearn.Hash("delivery-root"), "conversation:"+runtimelearn.Hash("delivery-root"))
	if len(events) != 1 {
		t.Fatal("quarantined result was retained as trusted evidence")
	}
	if len(logs) != 0 {
		t.Fatalf("normal observations logged private metadata: %v", logs)
	}
}

func TestDeliveryLearningFailureIsBestEffortAndDoesNotLeak(t *testing.T) {
	home := t.TempDir()
	agent := &loop.Agent{Learning: &runtimelearn.Store{Home: home}}
	file := filepath.Join(home, runtimelearn.OutcomesFile)
	os.MkdirAll(filepath.Dir(file), 0700)
	os.WriteFile(file, []byte(`{"version":99,"events":[]}`), 0600)
	logs := []string{}
	observe := deliveryLearning(agent, func(line string) { logs = append(logs, line) })
	observe(sched.DeliveryEvent{ID: "sensitive-report:failed", JobID: "sensitive-job", At: time.Now(), Status: "failed"})
	if len(logs) != 1 || strings.Contains(logs[0], "sensitive") {
		t.Fatalf("unsafe/error callback log: %v", logs)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != `{"version":99,"events":[]}` {
		t.Fatal("future evidence version overwritten")
	}
	deliveryLearning(nil, nil)(sched.DeliveryEvent{})
	deliveryLearning(&loop.Agent{}, nil)(sched.DeliveryEvent{})
}
