package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// prunedNap writes a nap whose single file has body, as NapOnce would, and points latest at it.
func prunedNap(t *testing.T, s FsStore, id, body string, class engine.FileClass, old bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	key := BlobKey("atlas", class, sha)
	if err := s.Put(key, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if old {
		past := time.Now().Add(-2 * blobGrace)
		_ = os.Chtimes(filepath.Join(s.Root, key), past, past)
	}
	n := Nap{ID: id, Agent: "atlas", TakenAt: IsoMillis(time.Now()), Files: []NapFile{{Path: "memories/MEMORY.md", Class: class, Sha256: sha, Size: len(body)}}}
	b, _ := learning.MarshalIndent(n)
	if err := s.Put(NapKey("atlas", id), b); err != nil {
		t.Fatal(err)
	}
	ptr, _ := learning.MarshalCompact(map[string]string{"nap": id})
	_ = s.Put("atlas/latest.json", ptr)
}

func TestPruneKeepsWhatTheDreamHasNotReadAndTheNewest(t *testing.T) {
	root := t.TempDir()
	inst := &instance.Instance{Root: root}
	s := FsStore{Root: filepath.Join(root, "store")}
	for i := 1; i <= 8; i++ {
		prunedNap(t, s, fmt.Sprintf("2026100%dT000000000Z-x", i), fmt.Sprintf("v%d%s", i, string(rune('a'+i))), engine.Learning, true)
	}
	// Never dreamed: nothing goes.
	r, err := Prune(inst, "atlas", s, 0)
	if err != nil || r.Naps != 0 || r.Blobs != 0 || r.Kept != 8 {
		t.Fatalf("undreamed %+v %v", r, err)
	}
	// The dream read up to nap 6.
	last := "20261006T000000000Z-x"
	if err := SaveWatermark(root, "atlas", &Watermark{LastNap: &last}); err != nil {
		t.Fatal(err)
	}
	r, err = Prune(inst, "atlas", s, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Naps 1-5 go (folded, not among the newest 3: 6, 7, 8); their bodies go too.
	if r.Naps != 5 || r.Blobs != 5 || r.Kept != 3 || r.Bytes == 0 {
		t.Fatalf("report %+v", r)
	}
	ids, _ := napIDs(s, "atlas")
	if len(ids) != 3 || ids[0] != last {
		t.Errorf("left %v", ids)
	}
	if n, _ := Latest(s, "atlas"); n == nil || n.ID != "20261008T000000000Z-x" {
		t.Errorf("latest %+v", n)
	}
}

func TestPruneSparesFreshBodiesAndRepairsARacingNap(t *testing.T) {
	root := t.TempDir()
	inst := &instance.Instance{Root: root}
	s := FsStore{Root: filepath.Join(root, "store")}
	for i := 1; i <= 5; i++ {
		prunedNap(t, s, fmt.Sprintf("2026100%dT000000000Z-x", i), fmt.Sprintf("b%d%s", i, string(rune('a'+i))), engine.State, true)
	}
	// A body uploaded a moment ago for a nap whose manifest is not written yet.
	fresh := BlobKey("atlas", engine.Raw, fmt.Sprintf("%064x", 42))
	_ = s.Put(fresh, []byte("in flight"))
	last := "20261005T000000000Z-x"
	_ = SaveWatermark(root, "atlas", &Watermark{LastNap: &last})
	if r, err := Prune(inst, "atlas", s, 1); err != nil || r.Naps != 4 || r.Kept != 1 || r.Blobs != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	if ok, _ := s.Exists(fresh); !ok {
		t.Error("a fresh body was deleted")
	}

	// A nap written during the prune reuses a body the prune deletes: it is dropped and latest moves back.
	gone := BlobKey("atlas", engine.State, fmt.Sprintf("%064x", 1))
	n := Nap{ID: "20261009T000000000Z-x", Agent: "atlas", TakenAt: IsoMillis(time.Now()), Files: []NapFile{{Path: "x", Class: engine.State, Sha256: fmt.Sprintf("%064x", 1)}}}
	b, _ := learning.MarshalIndent(n)
	_ = s.Put(NapKey("atlas", n.ID), b)
	ptr, _ := learning.MarshalCompact(map[string]string{"nap": n.ID})
	_ = s.Put("atlas/latest.json", ptr)
	if ok, _ := s.Exists(gone); ok {
		t.Fatal("fixture")
	}
	rep := &PruneReport{}
	if err := repair(s, "atlas", time.Now().Add(-time.Second), rep); err != nil || rep.Repaired != 1 {
		t.Fatalf("repair %+v %v", rep, err)
	}
	if l, _ := Latest(s, "atlas"); l == nil || l.ID != "20261005T000000000Z-x" {
		t.Errorf("latest moved back to %+v", l)
	}
}

func TestAutoAcceptOnlyWhatNeedsNoJudgement(t *testing.T) {
	root := t.TempDir()
	mk := func(id string, kind learning.Kind, scope manifest.Scope, pii []string, seen int, st learning.Status) *learning.Learning {
		return &learning.Learning{ID: id, Kind: kind, Scope: scope, Agent: "atlas", Unit: "sales", Text: id, PII: pii, Status: st, SeenCount: seen,
			FirstSeen: "2026-10-09T00:00:00.000Z", LastSeen: "2026-10-09T00:00:00.000Z", Instances: []string{}, Naps: []string{}}
	}
	ledger := []*learning.Learning{
		mk("ok0000000001", learning.KindMemory, manifest.ScopeAgent, []string{}, 3, learning.Proposed),
		mk("once00000001", learning.KindMemory, manifest.ScopeAgent, []string{}, 1, learning.Proposed),
		mk("user00000001", learning.KindUser, manifest.ScopeAgent, []string{}, 5, learning.Proposed),
		mk("pii000000001", learning.KindMemory, manifest.ScopeAgent, []string{"email"}, 5, learning.Proposed),
		mk("unit00000001", learning.KindMemory, manifest.ScopeUnit, []string{}, 5, learning.Proposed),
		mk("rej000000001", learning.KindMemory, manifest.ScopeAgent, []string{}, 5, learning.Rejected),
	}
	if err := learning.SaveLedger(root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	ids, err := AutoAccept(root, "atlas", 2)
	if err != nil || len(ids) != 1 || ids[0] != "ok0000000001" {
		t.Fatalf("auto-accepted %v %v", ids, err)
	}
	after, _ := learning.LoadLedger(root, "atlas")
	for _, e := range after {
		want := map[string]learning.Status{"ok0000000001": learning.Accepted, "rej000000001": learning.Rejected}[e.ID]
		if want == "" {
			want = learning.Proposed
		}
		if e.Status != want {
			t.Errorf("%s: %s, want %s", e.ID, e.Status, want)
		}
	}
	if ids, _ := AutoAccept(root, "atlas", 1); len(ids) != 1 || ids[0] != "once00000001" {
		t.Errorf("min_seen 1: %v", ids)
	}
}
