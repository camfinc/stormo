package loop

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
)

// putNap stores a nap holding one file per class, each with its own blob.
func putNap(t *testing.T, s Store, takenAt, reason string, latest bool) string {
	t.Helper()
	id := strings.NewReplacer("-", "", ":", "", ".", "").Replace(takenAt) + "-i"
	files := []NapFile{}
	for _, cls := range []engine.FileClass{engine.Learning, engine.State, engine.Raw} {
		f := NapFile{Path: string(cls) + ".txt", Class: cls, Sha256: string(cls) + "-" + id, Size: 1}
		files = append(files, f)
		if err := s.Put(BlobKey("atlas", cls, f.Sha256), []byte(fmt.Sprintf("%s of %s", cls, id))); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := learning.MarshalCompact(Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "i", TakenAt: takenAt, Reason: reason, Files: files})
	if err := s.Put(NapKey("atlas", id), body); err != nil {
		t.Fatal(err)
	}
	if latest {
		ptr, _ := learning.MarshalCompact(map[string]string{"nap": id})
		if err := s.Put("atlas/latest.json", ptr); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func stores(t *testing.T) (Store, Store) {
	d := t.TempDir()
	return FsStore{filepath.Join(d, "from")}, FsStore{filepath.Join(d, "to")}
}

func has(t *testing.T, s Store, key string) bool {
	ok, err := s.Exists(key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSyncCopiesLatestWholeAndUndreamedLearningOnly(t *testing.T) {
	from, to := stores(t)
	dreamed := putNap(t, from, "2026-10-07T10:00:00.000Z", "interval", true)
	undreamed := putNap(t, from, "2026-10-07T11:00:00.000Z", "interval", true)
	last := putNap(t, from, "2026-10-07T12:00:00.000Z", "shutdown", true)
	r, err := SyncNaps(from, to, "atlas", dreamed, false)
	if err != nil {
		t.Fatal(err)
	}
	if *r.Latest != last || !slices.Equal(r.Naps, []string{undreamed, last}) {
		t.Errorf("report = %+v", r)
	}
	if n, _ := Latest(to, "atlas"); n == nil || n.ID != last {
		t.Errorf("latest in destination = %v", n)
	}
	for _, cls := range []engine.FileClass{engine.Learning, engine.State, engine.Raw} {
		if !has(t, to, BlobKey("atlas", cls, string(cls)+"-"+last)) {
			t.Errorf("latest %s blob missing", cls)
		}
	}
	if !has(t, to, BlobKey("atlas", engine.Learning, "learning-"+undreamed)) {
		t.Error("undreamed learning blob missing")
	}
	if has(t, to, BlobKey("atlas", engine.Raw, "raw-"+undreamed)) {
		t.Error("older raw blob (PII) copied")
	}
	if has(t, to, NapKey("atlas", dreamed)) {
		t.Error("dreamed nap copied")
	}
	if again, _ := SyncNaps(from, to, "atlas", dreamed, false); again.Blobs != 0 {
		t.Errorf("not idempotent: %d blobs", again.Blobs)
	}
}

func TestSyncRefusesToRollBackANewerDestination(t *testing.T) {
	from, to := stores(t)
	old := putNap(t, from, "2026-10-07T10:00:00.000Z", "interval", true)
	putNap(t, to, "2026-10-07T11:00:00.000Z", "interval", true)
	var newer *NewerDestinationError
	if _, err := SyncNaps(from, to, "atlas", "", false); !errors.As(err, &newer) {
		t.Fatalf("err = %v", err)
	}
	if r, err := SyncNaps(from, to, "atlas", "", true); err != nil || *r.Latest != old {
		t.Fatalf("forced: %v %v", r, err)
	}
}

func TestSyncEmptySource(t *testing.T) {
	from, to := stores(t)
	r, err := SyncNaps(from, to, "atlas", "", false)
	if err != nil || r.Latest != nil || len(r.Naps) != 0 || r.Blobs != 0 {
		t.Errorf("r=%+v err=%v", r, err)
	}
}

func TestFinalNapWarning(t *testing.T) {
	from, _ := stores(t)
	stop := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if w, _ := FinalNapWarning(from, "atlas", stop); !strings.Contains(w, "no nap at all") {
		t.Errorf("w = %q", w)
	}
	putNap(t, from, "2026-10-07T11:45:00.000Z", "interval", true)
	if w, _ := FinalNapWarning(from, "atlas", stop); !strings.Contains(w, "~15 min before the stop") {
		t.Errorf("w = %q", w)
	}
	putNap(t, from, "2026-10-07T12:00:30.000Z", "shutdown", true)
	if w, _ := FinalNapWarning(from, "atlas", stop); w != "" {
		t.Errorf("w = %q", w)
	}
}
