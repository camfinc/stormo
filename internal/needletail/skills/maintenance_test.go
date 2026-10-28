package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceOwnershipAndProtection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *Library)
		action string
	}{
		{"owned", nil, "archive_review"},
		{"missing receipt", func(t *testing.T, l *Library) {
			if err := os.Remove(filepath.Join(l.Home, OriginsFile)); err != nil {
				t.Fatal(err)
			}
		}, "keep"},
		{"repository", func(t *testing.T, l *Library) { put(t, l.Home, BootedFile, `{"skills":{"custom":"hash"}}`) }, "keep"},
		{"generated", func(t *testing.T, l *Library) { l.Generated = []string{"custom"} }, "keep"},
		{"pinned", func(t *testing.T, l *Library) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if err := l.note("custom", func(e usageEntry, _ string) { e["pinned"] = true }); err != nil {
				t.Fatal(err)
			}
		}, "keep"},
		{"patch activity", func(t *testing.T, l *Library) {
			if _, err := l.Edit("custom", "---\nname: custom\ndescription: custom\n---\nUpdated"); err != nil {
				t.Fatal(err)
			}
		}, "keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			l := &Library{Home: t.TempDir(), Now: func() time.Time { return clock }}
			if _, err := l.Create("custom", "", "---\nname: custom\ndescription: custom\n---\n"); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(l.Home, OriginsFile))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("receipt privacy: %v %v", info, err)
			}
			clock = clock.Add(100 * 24 * time.Hour)
			if tc.change != nil {
				tc.change(t, l)
			}
			before, _ := os.ReadFile(l.usagePath())
			plan, err := l.PlanMaintenance(MaintenancePolicy{StaleAfter: 30 * 24 * time.Hour, ArchiveAfter: 90 * 24 * time.Hour})
			if err != nil || len(plan) != 1 || plan[0].Action != tc.action {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			after, _ := os.ReadFile(l.usagePath())
			if string(before) != string(after) {
				t.Fatal("planning mutated usage")
			}
			if _, err := l.Find("custom"); err != nil {
				t.Fatal("planning removed skill")
			}
		})
	}
}

func TestOriginsCapacityDoesNotWriteUnreadableState(t *testing.T) {
	l := library(t)
	o := skillOrigins{Version: 1, Skills: map[string]skillOrigin{}}
	for i := 0; i < 4096; i++ {
		o.Skills[fmt.Sprintf("skill-%d", i)] = skillOrigin{Name: "skill", CreatedAt: "2020-01-01T00:00:00Z", InitialSHA256: "hash"}
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	put(t, l.Home, OriginsFile, string(b))
	if _, err := l.Create("capacity", "", "---\nname: capacity\ndescription: capacity test\n---\n"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(l.Home, OriginsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(b) {
		t.Fatal("full origin store changed")
	}
	if _, err := l.readOrigins(); err != nil {
		t.Fatal("capacity handling corrupted existing origins")
	}
}

func TestMaintenanceHoldsAmbiguousAndInvalidEvidence(t *testing.T) {
	l := library(t)
	for _, file := range []string{OriginsFile, UsageFile} {
		put(t, l.Home, file, "{")
		if _, err := l.PlanMaintenance(MaintenancePolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour}); err == nil {
			t.Fatalf("corrupt %s accepted", file)
		}
		put(t, l.Home, file, `{"version":1,"skills":{}}`)
	}
	if !maintenanceProtected("group/custom", []string{"GROUP"}) || !maintenanceProtected("group", []string{"group/custom"}) {
		t.Fatal("protected reference ancestry ignored")
	}
	for _, v := range []any{"bad", true, "2100-01-01T00:00:00Z"} {
		if _, ok := maintenanceActivity(usageEntry{"created_at": v}, l.now()); ok {
			t.Fatalf("invalid activity accepted: %v", v)
		}
	}
	// The editable Hermes created_by field alone never establishes ownership.
	u := map[string]usageEntry{"solo": {"created_by": "agent", "created_at": "2020-01-01T00:00:00Z"}}
	b, _ := json.Marshal(u)
	put(t, l.Home, UsageFile, string(b))
	p, err := l.PlanMaintenance(MaintenancePolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p {
		if r.Action != "keep" {
			t.Fatalf("legacy metadata inferred ownership: %+v", r)
		}
	}
}
