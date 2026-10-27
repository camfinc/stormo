package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// tierStep is one call of the information-flow table (docs/needletail.md §7) and its tier in a
// tainted run: allow, marked (allowed, its result kept marked), approval, refuse.
type tierStep struct {
	name, tool string
	args       func(conv string) map[string]any
	tainted    string
	// trusted is the tier in an untainted run ("" is allow)
	trusted string
}

func fixed(a map[string]any) func(string) map[string]any {
	return func(string) map[string]any { return a }
}

var tierSteps = []tierStep{
	{"read", "read_file", fixed(map[string]any{"path": "SOUL.md"}), "allow", ""},
	{"followtext", "follow_up", fixed(map[string]any{"action": "create", "in": "2h", "text": "Reminder: the quote."}), "allow", ""},
	{"jobrun", sched.CronJobTool, fixed(map[string]any{"action": "run", "job_id": "tablejob001"}), "allow", ""},
	{"jobpause", sched.CronJobTool, fixed(map[string]any{"action": "pause", "job_id": "tablejob001"}), "allow", ""},
	{"proclist", "process_manage", fixed(map[string]any{"action": "list"}), "allow", ""},
	{"workfile", "write_file", func(conv string) map[string]any {
		return map[string]any{"path": "work/" + conv + "/draft.md", "content": "a draft"}
	}, "allow", ""},
	{"memsave", "memory_manage", fixed(map[string]any{"action": "save", "text": "The client prefers morning calls.", "force": true}), "marked", ""},
	{"memadd", "memory", fixed(map[string]any{"action": "add", "target": "memory", "content": "Quotes go out on Fridays."}), "marked", ""},
	{"terminal", "terminal", fixed(map[string]any{"command": "echo hi"}), "refuse", ""}, // no Landlock in tests: fail closed
	{"sendelse", "send_message", fixed(map[string]any{"target": "slack", "message": "Posted elsewhere."}), "approval", ""},
	{"homewrite", "write_file", fixed(map[string]any{"path": "notes.md", "content": "a note"}), "approval", ""},
	{"skillwrite", "write_file", fixed(map[string]any{"path": "skills/crm/new/SKILL.md", "content": "---\nname: new\ndescription: x\n---\nx\n"}), "approval", ""},
	{"skill", "skill_manage", fixed(map[string]any{"action": "create", "name": "table-skill", "category": "crm", "content": "---\nname: table-skill\ndescription: Do the table.\n---\nDo it.\n"}), "approval", ""},
	{"jobcreate", sched.CronJobTool, fixed(map[string]any{"action": "create", "schedule": "every 2h", "prompt": "Check the quotes.", "deliver": "local"}), "approval", ""},
	{"jobdeliver", sched.CronJobTool, fixed(map[string]any{"action": "update", "job_id": "tablejob001", "deliver": "slack"}), "approval", ""},
	{"jobschedule", sched.CronJobTool, fixed(map[string]any{"action": "update", "job_id": "tablejob001", "schedule": "every 3h"}), "approval", ""},
	{"followprompt", "follow_up", fixed(map[string]any{"action": "create", "in": "2h", "prompt": "Check on the quote."}), "approval", ""},
	{"memremove", "memory", fixed(map[string]any{"action": "remove", "target": "memory", "old_text": "anything"}), "approval", "refuse"},
	{"mempin", "memory_manage", fixed(map[string]any{"action": "pin", "id": "m0000000000a"}), "approval", "refuse"},
	{"broadcast", "send_message", fixed(map[string]any{"target": "slack", "message": "@here look", "broadcast": true}), "refuse", "refuse"},
	{"cronfile", "write_file", fixed(map[string]any{"path": "cron/jobs.json", "content": "{}"}), "refuse", "refuse"},
	{"jobremove", sched.CronJobTool, fixed(map[string]any{"action": "remove", "job_id": "tablejob001"}), "approval", ""},
}

// tierOf reads a tool result's tier from the reply that quotes it.
func tierOf(reply string) string {
	switch {
	case strings.Contains(reply, "pending_approval"):
		return "approval"
	case strings.Contains(reply, `"error"`):
		return "refuse"
	case strings.Contains(reply, `"untrusted":true`):
		return "marked"
	}
	return "allow"
}

// The information-flow table: each call's tier in a tainted Slack thread, and the same calls in a
// trusted DM (unchanged: nothing is put to a person, the protected stay refused).
func TestServeInformationFlowTiers(t *testing.T) {
	steps := map[string]tierStep{}
	for _, s := range tierSteps {
		steps[s.name] = s
	}
	conv := ""
	next := func(msgs []scriptMsg) (string, string, any) {
		user, after := lastUser(msgs)
		if after != nil {
			return "Tool said: " + after.text(), "", nil
		}
		for name, s := range steps {
			if strings.HasSuffix(strings.TrimSpace(user), "STEP "+name) {
				return "Doing " + name + ".", s.tool, s.args(conv)
			}
		}
		return "ok", "", nil
	}
	r := newApprovalRig(t, "approvals:\n    per_conversation: 40\n    per_day: 100\n", next)
	r.taintedThread()
	jobsPath := filepath.Join(r.home, "cron", "jobs.json")
	f, err := sched.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	job := sched.NewJob()
	for k, v := range map[string]any{"id": "tablejob001", "name": "tablejob001", "prompt": "Report.", "skills": []string{}, "no_agent": false, "created_at": "2026-10-01T00:00:00Z",
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true, "state": "scheduled",
		"next_run_at": "2099-01-01T00:00:00Z", "deliver": "local", "repeat": map[string]any{"times": nil, "completed": 0}} {
		job.Set(k, v)
	}
	f.Jobs = append(f.Jobs, job)
	if err := f.Write(jobsPath, filepath.Join(r.home, ".needletail-tmp")); err != nil {
		t.Fatal(err)
	}
	r.start()
	defer r.srv.stop()

	ts := 300
	run := func(channel, thread, user, step string) string {
		t.Helper()
		before := len(r.fake.Posts())
		ts++
		msgTS := fmt.Sprintf("1700000100.%06d", ts)
		ev := slacktest.Event{Channel: channel, ChannelType: "channel", User: user, Text: "STEP " + step, TS: msgTS, ThreadTS: thread}
		if thread == "" {
			ev.ChannelType = "im"
		}
		r.fake.Send("ev"+msgTS, ev)
		var reply string
		waitFor(t, "the reply to "+step, 15*time.Second, func() bool {
			for _, p := range r.fake.Posts()[before:] {
				if p.Channel == channel && strings.HasPrefix(p.Text, "Tool said:") {
					reply = p.Text
					return true
				}
			}
			return false
		})
		return reply
	}
	// tainted: the thread mallory's words are in
	conv = taintedSID
	approvalsWanted := 0
	for _, s := range tierSteps {
		reply := run(devChan, taintedTS, teammate, s.name)
		if got := tierOf(reply); got != s.tainted {
			t.Errorf("tainted %s (%s): tier %s, want %s: %s", s.name, s.tool, got, s.tainted, reply)
		}
		if s.tainted == "approval" {
			approvalsWanted++
		}
	}
	if n := len(r.cards()); n != approvalsWanted {
		t.Errorf("%d approval cards, want %d", n, approvalsWanted)
	}
	if b, err := os.ReadFile(filepath.Join(r.home, "work", taintedSID, "draft.md")); err != nil || string(b) != "a draft" {
		t.Errorf("the work file: %q %v", b, err)
	}
	for _, p := range []string{"notes.md", "skills/crm/new/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(r.home, p)); err == nil {
			t.Errorf("%s written without approval", p)
		}
	}
	recs, _ := os.ReadFile(filepath.Join(r.home, "memories", "RECORDS.md"))
	if strings.Count(string(recs), `"untrusted":true`) != 2 {
		t.Errorf("marked records:\n%s", recs)
	}
	if hot, _ := os.ReadFile(filepath.Join(r.home, "memories", "MEMORY.md")); strings.Contains(string(hot), "Fridays") {
		t.Error("a tainted memory went hot")
	}

	// untainted: a trusted DM, the same calls, nothing put to a person
	cards := len(r.cards())
	conv = "slack-D0ALLOWED1-dm"
	for _, s := range tierSteps {
		if s.name == "jobremove" || s.name == "jobpause" || s.name == "jobrun" || s.name == "jobdeliver" || s.name == "jobschedule" {
			continue // the job's own state moves on; covered in the tainted run
		}
		reply := run("D0ALLOWED1", "", teammate, s.name)
		want := s.trusted
		if want == "" {
			want = "allow"
		}
		if s.name == "terminal" {
			want = "allow" // a trusted command runs (unjailed in tests)
		}
		if got := tierOf(reply); got != want {
			t.Errorf("trusted %s (%s): tier %s, want %s: %s", s.name, s.tool, got, want, reply)
		}
	}
	if n := len(r.cards()); n != cards {
		t.Errorf("a trusted run put %d cards", n-cards)
	}
	if _, err := os.Stat(filepath.Join(r.home, "notes.md")); err != nil {
		t.Errorf("a trusted write: %v", err)
	}
}

// Taint from data: a record a tainted run saved is marked; a trusted DM that finds it with
// memory_search is tainted from then on (its next send elsewhere needs approval), until the
// owner trusts the conversation (POST /v1/sessions/{id}/trust, audited): then it sends again.
func TestServeTaintFromDataAndTrust(t *testing.T) {
	next := func(msgs []scriptMsg) (string, string, any) {
		user, after := lastUser(msgs)
		switch {
		case after != nil:
			return "Tool said: " + after.text(), "", nil
		case strings.Contains(user, "REMEMBER"):
			return "Saving.", "memory_manage", map[string]any{"action": "save", "text": "The supplier's new portal is quotes-portal.", "scope": "agent", "force": true}
		case strings.Contains(user, "RECALL"):
			return "Looking.", "memory_search", map[string]any{"query": "supplier portal"}
		}
		return approvalScript(msgs)
	}
	r := newApprovalRig(t, "", next)
	r.taintedThread()
	r.start()
	defer r.srv.stop()
	r.inThread("e1", teammate, "REMEMBER this", "1700000100.000200")
	waitFor(t, "the save", 10*time.Second, func() bool {
		return len(r.postsTo(devChan, `\"untrusted\":true`)) == 1 || len(r.postsTo(devChan, `"untrusted":true`)) == 1
	})
	dmSession := "slack-D0ALLOWED1-dm"
	dmSend := func(id, text, ts string) {
		r.fake.Send(id, slacktest.Event{Channel: "D0ALLOWED1", ChannelType: "im", User: teammate, Text: text, TS: ts})
	}
	header := func() string {
		b, _ := os.ReadFile(filepath.Join(r.home, "sessions", dmSession+".jsonl"))
		return strings.SplitN(string(b), "\n", 2)[0]
	}
	dmSend("d1", "RECALL it", "1700000300.000100")
	waitFor(t, "the search", 10*time.Second, func() bool { return len(r.postsTo("D0ALLOWED1", "quotes-portal")) == 1 })
	if !strings.Contains(header(), `"untrusted":true`) {
		t.Fatalf("the reader is not marked: %s", header())
	}
	dmSend("d2", "POSTHOME now", "1700000300.000200")
	r.waitCards(1)
	waitFor(t, "the pending reply", 10*time.Second, func() bool { return len(r.postsTo("D0ALLOWED1", "pending approval")) == 1 })
	if len(r.postsTo(homeChan, "The summary")) != 0 {
		t.Fatal("a tainted DM sent elsewhere without approval")
	}
	// the owner trusts the DM from the app (the API)
	var code int
	var body []byte
	var err error
	waitFor(t, "the trust (once the turn ended)", 10*time.Second, func() bool {
		code, body, err = r.srv.call(t.Context(), "POST", "/v1/sessions/"+dmSession+"/trust", nil)
		return err != nil || code != 409
	})
	if err != nil || code != 200 {
		t.Fatalf("trust: %d %s %v", code, body, err)
	}
	if h := header(); strings.Contains(h, `"untrusted":true`) || !strings.Contains(h, `"endorsed_by":"owner (API)"`) {
		t.Fatalf("not endorsed: %s", h)
	}
	audit, _ := os.ReadFile(filepath.Join(r.home, "state", "needletail", "approvals-audit.jsonl"))
	if !strings.Contains(string(audit), `"event":"trusted"`) || !strings.Contains(string(audit), dmSession) {
		t.Errorf("no audit record:\n%s", audit)
	}
	dmSend("d3", "POSTHOME again", "1700000300.000300")
	waitFor(t, "the direct send", 10*time.Second, func() bool { return len(r.postsTo(homeChan, "The summary for the home channel.")) == 1 })
	if len(r.cards()) != 1 {
		t.Errorf("a trusted send needed approval: %d cards", len(r.cards()))
	}
}

// serve empties the commands' .tmp at boot: a scratch file one run left is no channel to another.
func TestServeSweepsTmpAtBoot(t *testing.T) {
	h := home(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:1/v1")
	setenv(t, h)
	if err := os.MkdirAll(filepath.Join(h, ".tmp", "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".tmp/left.txt", ".tmp/dir/deep.txt", "work/kept/draft.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(h, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := startServe(t)
	s.stop()
	entries, _ := os.ReadDir(filepath.Join(h, ".tmp"))
	if len(entries) != 0 {
		t.Errorf(".tmp kept %v", entries)
	}
	if _, err := os.Stat(filepath.Join(h, "work", "kept", "draft.md")); err != nil {
		t.Errorf("a work directory was swept: %v", err)
	}
}
