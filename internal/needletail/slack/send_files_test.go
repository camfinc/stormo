package slack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// send_message carries files: read under read_file's rules, posted with the message as their
// comment, to "here" (the conversation it was called from) or any target the policy allows; an
// approved send runs only on the files as the card showed them.
func TestSendMessageFiles(t *testing.T) {
	const thread = "1700000070.000100"
	r := newRig(t, nil)
	r.turns.mu.Lock()
	r.turns.sessions[SessionID(dev, thread)] = wire.Session{Source: "slack"}
	r.turns.mu.Unlock()
	home, group := t.TempDir(), t.TempDir()
	write := func(root, rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(home, "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "\xff\xd8\xff cover bytes")
	write(home, "files/conv/upload.mp4", "someone's upload")
	write(home, "state/secret.db", "private")
	write(home, "work/slack-C0OTHER001-1/notes.txt", "another conversation's")
	write(group, "renders/clip.mp4", "clip bytes")
	env := &tools.Env{Session: SessionID(dev, thread), Home: home, Sandbox: sandbox.Sandbox{Home: home, Shared: []string{group}}}
	reg := tools.NewRegistry(r.g.SendTool())
	call := func(ctx context.Context, args map[string]any) (map[string]any, string, bool) {
		b, _ := json.Marshal(args)
		out, failed := reg.Call(ctx, env, "send_message", string(b))
		v := map[string]any{}
		_ = json.Unmarshal([]byte(out), &v)
		return v, out, failed
	}

	// "here", a file of the home and one of the shared space, with a comment
	v, out, failed := call(context.Background(), map[string]any{"target": "here", "message": "Cover ready", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", filepath.Join(group, "renders/clip.mp4")}})
	if failed || v["files"] != float64(2) {
		t.Fatalf("send: %s", out)
	}
	sh := r.fake.Shares()
	if len(sh) != 1 || sh[0].Channel != dev || sh[0].Thread != thread || sh[0].Comment != "Cover ready" ||
		string(sh[0].Files["cover.jpg"]) != "\xff\xd8\xff cover bytes" || string(sh[0].Files["clip.mp4"]) != "clip bytes" {
		t.Fatalf("shared: %+v", sh)
	}
	// files alone: no text needed
	if _, out, failed := call(context.Background(), map[string]any{"target": "here", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}}); failed {
		t.Errorf("files alone: %s", out)
	}

	// read_file's rules: the store of uploads and private runtime data are refused, as is anything
	// outside the sandbox; too many files; files to Telegram
	for _, bad := range []map[string]any{
		{"target": "here", "files": []string{"files/conv/upload.mp4"}},
		{"target": "here", "files": []string{"state/secret.db"}},
		{"target": "here", "files": []string{"/etc/hosts"}},
		{"target": "here", "files": []string{"work/slack-C0OTHER001-1/notes.txt"}},
		{"target": "here", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}},
		{"target": "telegram:1", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}},
		{"target": "slack:C0SECRET01", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}},
	} {
		if _, out, failed := call(context.Background(), bad); !failed {
			t.Errorf("%v accepted: %s", bad, out)
		}
	}
	if n := len(r.fake.Shares()); n != 2 {
		t.Errorf("a refused send uploaded something: %d shares", n)
	}

	// an approved send replays only on the bound content: the same file passes, a changed one is refused
	files, err := SendFiles(context.Background(), env, []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	// as the approvals service runs it: endorsed by the approval, with the card's bindings
	bound := messaging.WithBindings(messaging.WithEndorsed(context.Background(), "A1"), map[string]string{BindFile + "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg": files[0].SHA256})
	if _, out, failed := call(bound, map[string]any{"target": "here", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}}); failed {
		t.Errorf("approved, unchanged: %s", out)
	}
	write(home, "work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg", "swapped after the card")
	if _, out, failed := call(bound, map[string]any{"target": "here", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}}); !failed || !strings.Contains(out, "changed after the owner approved") {
		t.Errorf("approved, changed: %s", out)
	}

	// "here" outside a Slack conversation
	env.Session = "api-1"
	if _, out, failed := call(context.Background(), map[string]any{"target": "here", "files": []string{"work/slack-C0DEVCHAN1-1700000070.000100/cover.jpg"}}); !failed || !strings.Contains(out, "not") {
		t.Errorf("here from the API: %s", out)
	}
}

// The upload URL Slack hands out is the only place a file's bytes go.
func TestUploadURLMustBeSlacks(t *testing.T) {
	w := &Web{}
	if err := w.checkUploadURL("https://files.slack.com/upload/v1/abc"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"https://evil.example/upload", "http://files.slack.com/upload", "http://127.0.0.1:9/upload"} {
		if err := w.checkUploadURL(bad); err == nil {
			t.Errorf("%s accepted with Slack's own API", bad)
		}
	}
	if err := (&Web{BaseURL: "http://127.0.0.1:1234/api"}).checkUploadURL("http://127.0.0.1:1234/upload/F1"); err != nil {
		t.Errorf("loopback test server: %v", err)
	}
}
