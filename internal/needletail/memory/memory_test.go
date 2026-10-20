package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "memories"), 0o755)
	// the seed as Compile writes it: entries joined, no trailing newline
	_ = os.WriteFile(filepath.Join(home, "memories", "MEMORY.md"), []byte("Deals close on Fridays.\n§\nCRM is slow at 9am."), 0o644)
	s := &Store{Home: home, Limits: map[string]int{TargetMemory: 80, TargetUser: 30}}
	u, err := s.Add(TargetMemory, "  Quotes need a manager OK.  ")
	if err != nil || u.Entries != 3 {
		t.Fatal(u, err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "memories", "MEMORY.md"))
	if string(raw) != "Deals close on Fridays.\n§\nCRM is slow at 9am.\n§\nQuotes need a manager OK." {
		t.Errorf("file = %q", raw)
	}
	if _, err := s.Add(TargetMemory, "This entry is far too long to fit in what is left of the budget."); err == nil {
		t.Error("over budget accepted")
	} else {
		var ob *OverBudget
		if !errors.As(err, &ob) || ob.Usage.Limit != 80 || !strings.Contains(err.Error(), "of 80") {
			t.Errorf("over budget = %v", err)
		}
	}
	if _, err := s.Replace(TargetMemory, "slow", "CRM is slow before 10am."); err != nil {
		t.Error(err)
	}
	if _, err := s.Remove(TargetMemory, "Fridays"); err != nil {
		t.Error(err)
	}
	if _, err := s.Remove(TargetMemory, "nothing like it"); !errors.Is(err, ErrNoMatch) {
		t.Errorf("no match: %v", err)
	}
	_, _ = s.Add(TargetMemory, "CRM again")
	if _, err := s.Remove(TargetMemory, "CRM"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := s.Add(TargetUser, "a\n§\nb"); !errors.Is(err, ErrDelimiter) {
		t.Errorf("delimiter: %v", err)
	}
	if _, err := s.Add("other", "x"); err == nil {
		t.Error("bad target")
	}
	// the user file did not exist: created
	if _, err := s.Add(TargetUser, "Prefers Spanish — ñ"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Current(TargetUser); u.Used != Units("Prefers Spanish — ñ") || u.String() != "19/30" {
		t.Errorf("usage = %+v %s", u, u)
	}
	if Units("😀") != 2 {
		t.Error("utf16 units")
	}
	if !strings.Contains(s.Block(TargetMemory), "Quotes need") || s.Block("other") != "" {
		t.Error("block")
	}
}
