package link

import (
	"errors"
	"fmt"
	"testing"
)

func TestSpoolKeepsOrderAndDropsOldestPastItsBound(t *testing.T) {
	s := &Spool{Dir: t.TempDir(), MaxItems: 3}
	for i := 0; i < 5; i++ {
		if err := s.Put(Item{Path: fmt.Sprintf("/ingest/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("len %d", s.Len())
	}
	var sent []string
	// The core takes two, then fails: the third stays for later.
	n := s.Drain(func(it Item) error {
		if len(sent) == 2 {
			return errors.New("away")
		}
		sent = append(sent, it.Path)
		return nil
	})
	if n != 2 || fmt.Sprint(sent) != "[/ingest/2 /ingest/3]" || s.Len() != 1 {
		t.Errorf("sent %d %v, left %d", n, sent, s.Len())
	}
}

func TestWSURL(t *testing.T) {
	for in, want := range map[string]string{"http://h:1": "ws://h:1/link", "https://core.example": "wss://core.example/link"} {
		if got := wsURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
