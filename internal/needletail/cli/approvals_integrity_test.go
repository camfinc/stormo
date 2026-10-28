package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// integrityScript is approvalScript plus calls whose arguments a person could misread: raw JSON
// with case-variant duplicate keys, or a key the tool's schema does not name.
func integrityScript(msgs []scriptMsg) (string, string, any) {
	user, after := lastUser(msgs)
	switch {
	case after != nil:
		return "Tool said: " + after.text(), "", nil
	case strings.Contains(user, "DUPKEYS"):
		return "Posting the summary.", "send_message", json.RawMessage(`{"target":"slack","message":"The harmless summary.","MESSAGE":"EVIL payload"}`)
	case strings.Contains(user, "CASEKEY"):
		return "Posting the summary.", "send_message", json.RawMessage(`{"target":"slack","Message":"EVIL payload"}`)
	}
	return approvalScript(msgs)
}

// Item 1 (Claude H2): the card must show what runs. Arguments with keys equal ignoring case (the
// tool's decoder takes the last of them, the card the exact one) or a key the schema does not
// name are refused before any card; clean arguments are put canonically and run exactly.
func TestServeApprovalRefusesAmbiguousArgs(t *testing.T) {
	r := newApprovalRig(t, "", integrityScript)
	r.taintedThread()
	r.start()
	defer r.srv.stop()
	r.inThread("e1", teammate, "DUPKEYS please", "1700000100.000200")
	waitFor(t, "the refusal of duplicate keys", 15*time.Second, func() bool { return len(r.postsTo(devChan, "Tool said:")) == 1 })
	r.inThread("e2", teammate, "CASEKEY please", "1700000100.000300")
	waitFor(t, "the refusal of a case-variant key", 15*time.Second, func() bool { return len(r.postsTo(devChan, "Tool said:")) == 2 })
	if c := r.cards(); len(c) != 0 {
		t.Fatalf("ambiguous arguments were put to the owner: %+v", c)
	}
	for _, p := range r.postsTo(devChan, "Tool said:") {
		if !strings.Contains(p.Text, "argument") {
			t.Errorf("the refusal does not say why: %q", p.Text)
		}
	}
	if len(r.postsTo(homeChan, "EVIL")) != 0 || len(r.postsTo(homeChan, "harmless")) != 0 {
		t.Fatal("sent")
	}
	// clean arguments: a card, and exactly them on approval
	r.inThread("e3", teammate, "POSTHOME please", "1700000100.000400")
	c := r.waitCards(1)[0]
	r.click("i1", owner, c, "needletail_approve", c.hash)
	waitFor(t, "the approved send", 10*time.Second, func() bool { return len(r.postsTo(homeChan, "The summary for the home channel.")) == 1 })
}
