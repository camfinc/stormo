package slack

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/commands"
)

// cmdLog records what the commands' backend was asked.
type cmdLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *cmdLog) add(s string) { c.mu.Lock(); c.calls = append(c.calls, s); c.mu.Unlock() }
func (c *cmdLog) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.calls...)
}

func cmdBackend(log *cmdLog, slow time.Duration) commands.Backend {
	return commands.Backend{
		New:   func(_ context.Context, conv, by string) error { log.add("new " + conv + " " + by); return nil },
		Stop:  func(conv string) bool { log.add("stop " + conv); return false },
		Trust: func(_ context.Context, conv, by string) error { log.add("trust " + conv + " " + by); return nil },
		Status: func(conv string) commands.State {
			time.Sleep(slow)
			log.add("status " + conv)
			return commands.State{Agent: "Test", Model: "m1", Untrusted: true, Taint: []commands.Taint{{Why: "x", Users: []string{mallet}}}}
		},
	}
}

// Slash commands: acked at once (before the answer, however slow it is), answered to the caller
// alone through the response_url (ephemeral, no token, no post in the channel), gated as messages
// are (allowed_users; approvers for /trust), and scoped to the DM's conversation.
func TestSlashCommands(t *testing.T) {
	owner := "U0OWNER001"
	r := newRig(t, func(o *Options) { o.AllowedUsers = append(o.AllowedUsers, owner) })
	log := &cmdLog{}
	// before serve wires the commands: told to try again, nothing done
	r.fake.Slash("Sl0", "/new", "", alice, dm, nil)
	if res := r.fake.WaitResponses(1, 5*time.Second); !strings.Contains(res[0].Text, "not running yet") {
		t.Fatalf("before SetCommands: %+v", res)
	}
	names, _ := commands.SlackNames("", nil)
	r.g.SetCommands(Commands{Names: names, Backend: cmdBackend(log, 600*time.Millisecond), Approvers: func() []string { return []string{owner} }})

	sent := time.Now()
	r.fake.Slash("Sl1", "/agent-status", "", alice, dm, nil)
	res := r.fake.WaitResponses(2, 5*time.Second)
	ack := r.fake.AckTime("Sl1")
	if ack.IsZero() || ack.Sub(sent) > 500*time.Millisecond || !ack.Before(res[1].At) {
		t.Fatalf("ack at %v (sent %v, answered %v): not acked at once", ack, sent, res[1].At)
	}
	if res[1].ResponseType != "ephemeral" || res[1].Auth != "" || !strings.Contains(res[1].Text, "marked") {
		t.Fatalf("status answer: %+v", res[1])
	}
	if got := log.get(); len(got) != 1 || got[0] != "status "+DMSessionID(dm) {
		t.Fatalf("backend: %v", got)
	}

	// /new in the DM restarts the DM's conversation; in a channel there is none to restart
	r.fake.Slash("Sl2", "/new", "", alice, dm, nil)
	r.fake.WaitResponses(3, 5*time.Second)
	r.fake.Slash("Sl3", "/new", "", alice, dev, nil)
	res = r.fake.WaitResponses(4, 5*time.Second)
	if !strings.Contains(res[3].Text, "own conversation") {
		t.Fatalf("/new in a channel: %q", res[3].Text)
	}
	// /trust: refused to a non-approver, run for an approver
	r.fake.Slash("Sl4", "/trust", "", alice, dm, nil)
	res = r.fake.WaitResponses(5, 5*time.Second)
	if !strings.Contains(res[4].Text, "Only an approver") {
		t.Fatalf("non-approver /trust: %q", res[4].Text)
	}
	r.fake.Slash("Sl5", "/trust", "", owner, "D0OWNERDM1", nil)
	res = r.fake.WaitResponses(6, 5*time.Second)
	if !strings.Contains(res[5].Text, "Endorsed") {
		t.Fatalf("approver /trust: %q", res[5].Text)
	}
	// someone allowed_users does not list: refused, the backend never asked
	r.fake.Slash("Sl6", "/new", "", mallet, "D0MALLET01", nil)
	res = r.fake.WaitResponses(7, 5*time.Second)
	if !strings.Contains(res[6].Text, "may not use my commands") {
		t.Fatalf("stranger: %q", res[6].Text)
	}
	// another app's command (an old Hermes one): named as not mine
	r.fake.Slash("Sl7", "/model", "gpt", alice, dm, nil)
	res = r.fake.WaitResponses(8, 5*time.Second)
	if !strings.Contains(res[7].Text, "is not one of my commands") || !strings.Contains(res[7].Text, "/help") {
		t.Fatalf("unknown: %q", res[7].Text)
	}
	want := []string{"status " + DMSessionID(dm), "new " + DMSessionID(dm) + " slack:" + alice, "status " + DMSessionID("D0OWNERDM1"), "trust " + DMSessionID("D0OWNERDM1") + " slack:" + owner}
	if got := log.get(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("backend calls %v, want %v", got, want)
	}
	// nothing was posted in a channel or DM: every answer is the caller's alone
	if p := r.fake.Posts(); len(p) != 0 {
		t.Fatalf("posts: %+v", p)
	}
	for _, id := range []string{"Sl0", "Sl1", "Sl2", "Sl3", "Sl4", "Sl5", "Sl6", "Sl7"} {
		if r.fake.AckTime(id).IsZero() {
			t.Errorf("%s not acked", id)
		}
	}

	// a response_url that fails: the answer goes as chat.postEphemeral to the caller
	r.fake.SetRespondFail(500)
	r.fake.Slash("Sl8", "/help", "", alice, dm, nil)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.fake.Calls("chat.postEphemeral")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e := r.fake.Calls("chat.postEphemeral"); len(e) != 1 || e[0].Params["user"] != alice || !strings.Contains(e[0].Params["text"], "/agent-status") {
		t.Fatalf("fallback: %+v", e)
	}
	// a response_url off Slack's host is never posted to (the token-free answer still is not sent
	// elsewhere): the fallback answers
	r.fake.SetRespondFail(0)
	before := len(r.fake.Responses())
	r.fake.Slash("Sl9", "/help", "", alice, dm, map[string]string{"response_url": "https://hooks.example.org/commands/x"})
	deadline = time.Now().Add(5 * time.Second)
	for len(r.fake.Calls("chat.postEphemeral")) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.fake.Calls("chat.postEphemeral")) != 2 || len(r.fake.Responses()) != before {
		t.Fatalf("off-host response_url: %d ephemerals, %d responses", len(r.fake.Calls("chat.postEphemeral")), len(r.fake.Responses())-before)
	}
}
