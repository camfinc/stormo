package loop

import (
	"bytes"
	"sync"
	"testing"
)

func TestFsStorePutIsWholeUnderConcurrentWriters(t *testing.T) {
	s := FsStore{Root: t.TempDir()}
	long, short := bytes.Repeat([]byte("L"), 200_000), bytes.Repeat([]byte("s"), 150_000)
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for _, b := range [][]byte{long, short} {
			wg.Add(1)
			go func(b []byte) { defer wg.Done(); _ = s.Put("naps/x.json", b) }(b)
		}
		wg.Wait()
		got, _ := s.Get("naps/x.json")
		if !bytes.Equal(got, long) && !bytes.Equal(got, short) {
			t.Fatalf("round %d: %d bytes, a mix of two writes", round, len(got))
		}
	}
	keys, _ := s.List("naps")
	if len(keys) != 1 {
		t.Errorf("list shows temporaries: %v", keys)
	}
}
