package sched

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMaintenanceKeepsPausedAndDisabledSkillReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	body := `{"jobs":[{"id":"paused","paused":true,"skills":["acme-recovery"]},{"id":"disabled","enabled":false,"skill":["acme-audit"]}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{path: path}
	refs, err := s.SkillReferences()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(refs, []string{"acme-recovery", "acme-audit"}) {
		t.Fatalf("lost protected references: %v", refs)
	}
	if err := os.WriteFile(path, []byte(`{"jobs":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SkillReferences(); err == nil {
		t.Fatal("corrupt schedule removed protections silently")
	}
}
