package attach

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// cap_attach sends a conversation's own upload to a waiting job through the core (never the shared
// space); consume drops it; a marked upload marks the conversation, and a tainted run asks a person.
func TestCapAttach(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	video := strings.Repeat("v", 3<<20)
	if _, err := s.Save("convA", Entry{ID: "F1", Name: "clip one.mp4", MIME: "video/mp4", Kind: KindVideo}, []byte(video)); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Save("convA", Entry{ID: "F2", Name: "guest.mp4", MIME: "video/mp4", Kind: KindVideo, Marked: true, Why: "a guest"}, []byte("g"))
	_, _ = s.Save("convB", Entry{ID: "F3", Name: "other.mp4", MIME: "video/mp4", Kind: KindVideo}, []byte("b"))
	type sent struct {
		job  int64
		name string
		n    int
	}
	var got []sent
	tool := s.AttachTool(func(_ context.Context, job int64, name string, body []byte) (json.RawMessage, error) {
		got = append(got, sent{job, name, len(body)})
		return json.RawMessage(`{"status":"queued"}`), nil
	})
	env := &tools.Env{Session: "convA"}
	marked := ""
	ctx := messaging.WithQuarantine(context.Background(), func(why string) error { marked = why; return nil })
	call := func(ctx context.Context, args string) (map[string]any, error) {
		out, err := tool.Run(ctx, env, json.RawMessage(args))
		if err != nil {
			return nil, err
		}
		m, _ := out.(map[string]any)
		return m, nil
	}

	out, err := call(ctx, `{"job":7,"file":"F1","consume":true}`)
	if err != nil || out["consumed"] != true || len(got) != 1 || got[0] != (sent{7, "clip_one.mp4", 3 << 20}) {
		t.Fatalf("attach: %v %v %v", out, err, got)
	}
	if _, err := s.Find("convA", "F1"); err == nil {
		t.Error("consumed, still kept")
	}
	if _, err := call(ctx, `{"job":7,"file":"F3"}`); err == nil {
		t.Error("another conversation's upload sent")
	}
	// a marked upload marks the conversation before anything goes, and the call then waits for a person
	_, err = call(ctx, `{"job":8,"file":"F2"}`)
	var need *messaging.ApprovalNeeded
	if marked == "" || !errors.As(err, &need) {
		t.Errorf("a marked upload: marked %q, %v", marked, err)
	}
	// an already tainted run: a person approves the exact call, nothing goes before
	tainted := messaging.WithUntrusted(messaging.WithQuarantine(context.Background(), func(string) error { return nil }), "read a guest's words")
	s.Save("convA", Entry{ID: "F4", Name: "clean.mp4", MIME: "video/mp4", Kind: KindVideo}, []byte("c"))
	if _, err := call(tainted, `{"job":9,"file":"F4"}`); !errors.As(err, &need) {
		t.Errorf("tainted: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("sent without approval: %v", got)
	}
	// the approved call runs (endorsed)
	if _, err := call(messaging.WithEndorsed(tainted, "A1"), `{"job":9,"file":"F4"}`); err != nil || len(got) != 2 {
		t.Errorf("approved: %v %v", err, got)
	}
}
