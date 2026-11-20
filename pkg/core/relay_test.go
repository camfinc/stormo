package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	r := NewRelay(dir, func() time.Time { return now })
	write := func(s string) func(*os.File) error {
		return func(f *os.File) error { _, err := f.WriteString(s); return err }
	}

	f, err := r.Keep("atlas", RelayFile{Name: " promo.mp4 ", MIME: "video/mp4", Marked: true}, write("video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := RelayRef(f.Ref()); !ok || id != f.ID || f.Size != 11 || f.Name != "promo.mp4" || !f.Marked {
		t.Fatalf("kept %+v", f)
	}
	if !strings.HasPrefix(f.Describe(), f.Ref()+" (promo.mp4, video/mp4, 11 bytes, kept until Nov 27 16:00 UTC)") {
		t.Errorf("describe %q", f.Describe())
	}
	got, err := r.Get("atlas", f.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := r.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(data)
	data.Close()
	if string(b) != "video bytes" {
		t.Fatalf("bytes %q", b)
	}

	// Another agent may use it only once it is granted, by an agent that holds it.
	if _, err := r.Get("nova", f.ID); !errors.Is(err, ErrRelayGone) {
		t.Fatalf("not granted: %v", err)
	}
	if _, err := r.Grant("nova", f.ID, []string{"lyra"}); !errors.Is(err, ErrRelayGone) {
		t.Fatalf("granted by one that does not hold it: %v", err)
	}
	if _, err := r.Grant("atlas", f.ID, []string{"nova", "nova"}); err != nil {
		t.Fatal(err)
	}
	if g, err := r.Get("nova", f.ID); err != nil || strings.Join(g.Grants, ",") != "atlas,nova" {
		t.Fatalf("granted %+v %v", g, err)
	}

	for _, ref := range []string{"relay:../x", "relay:" + strings.ToUpper(f.ID), f.ID, "relay:"} {
		if _, ok := RelayRef(ref); ok {
			t.Errorf("%q read as a reference", ref)
		}
	}
	if _, err := r.Get("atlas", "0123456789abcdef01234567"); !errors.Is(err, ErrRelayGone) {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := r.Keep("atlas", RelayFile{Name: "empty"}, write("")); err == nil {
		t.Error("an empty file kept")
	}
	if _, err := r.Keep("atlas", RelayFile{Name: "x"}, func(*os.File) error { return errors.New("download failed") }); err == nil {
		t.Error("a failed fill kept")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %d entries", len(entries))
	}

	// Past RelayKeep it is gone, and the next prune removes it.
	now = now.Add(RelayKeep + time.Minute)
	if _, err := r.Get("atlas", f.ID); !errors.Is(err, ErrRelayGone) {
		t.Fatalf("expired: %v", err)
	}
	NewRelay(dir, func() time.Time { return now })
	if _, err := os.Stat(filepath.Join(dir, f.ID)); !os.IsNotExist(err) {
		t.Errorf("expired file not pruned: %v", err)
	}
}
