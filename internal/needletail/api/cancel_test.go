package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// /stop: Cancel ends the turn running in a conversation (found by any of its sessions), and only
// that one; with none running it says so.
func TestCancelStopsTheConversationsTurn(t *testing.T) {
	s, _, _ := newServer(t, t.TempDir())
	if s.Cancel("slack-D0DM000001-dm") {
		t.Fatal("cancelled with nothing running")
	}
	res, err := s.Reserve("slack-D0DM000001-dm", wire.Session{Source: "slack"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Reserve("slack-D0DM000002-dm", wire.Session{Source: "slack"})
	if err != nil {
		t.Fatal(err)
	}
	done, otherDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := res.Turn(context.Background(), "block", loop.Options{})
		res.Release()
		done <- err
	}()
	go func() {
		_, err := other.Turn(context.Background(), "block", loop.Options{})
		other.Release()
		otherDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !s.Cancel("slack-D0DM000001-dm") {
		if time.Now().After(deadline) {
			t.Fatal("the turn never became cancellable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("turn: %v, want cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not stop")
	}
	select {
	case err := <-otherDone:
		t.Fatalf("the other conversation's turn stopped too: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !s.Running("slack-D0DM000002-dm") || s.Running("slack-D0DM000001-dm") {
		t.Fatal("running state wrong")
	}
	if !s.Cancel("slack-D0DM000002-dm") {
		t.Fatal("second cancel")
	}
	<-otherDone
}
