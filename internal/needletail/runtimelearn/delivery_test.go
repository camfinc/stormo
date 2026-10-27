package runtimelearn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeliveryEvidenceIsPrivateAndReplaySafe(t *testing.T) {
	home := t.TempDir()
	store := &Store{Home: home}
	at := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	observation := DeliveryObservation{ID: "private-report-id:failed", JobID: "private-job-id", TaskRoot: "private-task-root", At: at, Status: "failed", Failure: true}
	if err := store.RecordDelivery(observation); err != nil {
		t.Fatal(err)
	}
	// Callback replay after restart must not change the first observation timestamp.
	store = &Store{Home: home}
	observation.At = at.Add(time.Minute)
	if err := store.RecordDelivery(observation); err != nil {
		t.Fatal(err)
	}
	observation.ID = "private-report-id:recovered"
	observation.Status = "recovered"
	observation.At = at.Add(2 * time.Minute)
	if err := store.RecordDelivery(observation); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDelivery(observation); err != nil {
		t.Fatal(err)
	}
	raw, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 2 {
		t.Fatalf("callbacks duplicated evidence: %+v", raw.Events)
	}
	first, recovered := raw.Events[0], raw.Events[1]
	if first.At != at.Format(time.RFC3339Nano) || first.TaskOutcome != "failed" || first.ExecutionStatus != "failed" {
		t.Fatalf("incorrect failure evidence %+v", first)
	}
	if first.SubjectKind != "delivery" || first.SubjectID != recovered.SubjectID || first.SubjectRevision != recovered.SubjectRevision || first.TaskRoot != Hash("private-task-root") {
		t.Fatal("delivery revision/root identity was lost")
	}
	if recovered.ExecutionStatus != "succeeded" || recovered.TaskOutcome != "unknown" || !strings.Contains(recovered.EvidenceRef, "original-execution:failed") {
		t.Fatalf("transport recovery implied workload success: %+v", recovered)
	}
	if recovered.Audience != "conversation:"+first.TaskRoot || recovered.SourceType != "runtime" {
		t.Fatal("delivery observation widened audience or authority")
	}
	summary := raw.Summary("delivery", first.SubjectID, first.SubjectRevision, first.Audience)
	if summary.Confirmations != 0 {
		t.Fatal("delivery event confirmed knowledge")
	}
	encoded, _ := json.Marshal(raw)
	for _, secret := range []string{"private-report-id", "private-job-id", "private-task-root"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("raw source identity entered evidence store")
		}
	}
	b, err := os.ReadFile(filepath.Join(home, OutcomesExportFile))
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Events []any `json:"events"`
	}
	if json.Unmarshal(b, &exported) != nil || len(exported.Events) != 0 {
		t.Fatal("private delivery observation exported to dream")
	}
}

func TestDeliveryEvidenceRefusesConflictingIdentity(t *testing.T) {
	store := &Store{Home: t.TempDir()}
	observation := DeliveryObservation{ID: "report:failed", JobID: "job", At: time.Now(), Status: "failed"}
	if err := store.RecordDelivery(observation); err != nil {
		t.Fatal(err)
	}
	observation.Failure = true
	if err := store.RecordDelivery(observation); err == nil {
		t.Fatal("conflicting original execution outcome replaced existing evidence")
	}
	for _, invalid := range []DeliveryObservation{{ID: "report:success", JobID: "job", At: time.Now(), Status: "succeeded"}, {ID: "report", JobID: "job", At: time.Now(), Status: "failed"}, {ID: "report:failed", JobID: "job", Status: "failed"}} {
		if err := store.RecordDelivery(invalid); err == nil {
			t.Fatalf("invalid observation accepted %+v", invalid)
		}
	}
	raw, _ := store.load()
	if len(raw.Events) != 1 {
		t.Fatal("invalid events mutated evidence")
	}
	if raw.Events[0].TaskRoot != Hash("scheduled-delivery:report") {
		t.Fatal("script-only result lacks private report root")
	}
}
