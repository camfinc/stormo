package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The audiences the tests' runtime knows: two people's DMs, two threads of one channel, a
// scheduled job and a oneshot.
var (
	ana    = Audience{Scope: "dm:UANA", Conv: "slack-DANA-dm"}
	bob    = Audience{Scope: "dm:UBOB", Conv: "slack-DBOB-dm"}
	chan1  = Audience{Scope: "channel:C1", Conv: "slack-C1-100.1"}
	chan1b = Audience{Scope: "channel:C1", Conv: "slack-C1-200.2"}
	chan2  = Audience{Scope: "channel:C2", Conv: "slack-C2-300.3"}
	job    = Audience{}
	owner  = Audience{Operator: true}
)

func scopedStore(t *testing.T, mem string) (*Store, string) {
	t.Helper()
	home := newHome(t, mem, "")
	s := newStore(home)
	s.Where = func(session string) Audience {
		switch {
		case strings.HasPrefix(session, "slack-DANA-dm"):
			return ana
		case strings.HasPrefix(session, "slack-DBOB-dm"):
			return bob
		case strings.HasPrefix(session, "slack-C1-100.1"):
			return chan1
		case strings.HasPrefix(session, "slack-C1-"):
			return Audience{Scope: "channel:C1", Conv: session}
		case strings.HasPrefix(session, "oneshot-"):
			return owner
		}
		return Audience{}
	}
	return s, home
}

func ids(vs []View) []string {
	out := []string{}
	for _, v := range vs {
		out = append(out, v.ID)
	}
	return out
}

// A record saved in one person's DM is that DM's: never recalled, found, fetched or changed by id
// in another person's DM, a channel or a job; the oneshot (the owner) may search every scope and
// reach any record by id.
func TestScopeKeepsADMsRecordsInIt(t *testing.T) {
	s, home := scopedStore(t, "")
	q := "When does the passport of the client expire? Renewal in March"
	r, err := s.Save(SaveOptions{Text: "The client's passport expires in March 2027; renewal booked.", Audience: ana, Src: "chat:slack-DANA-dm"})
	if err != nil || r.Status != StatusCreated || r.Scope != "dm:UANA" {
		t.Fatal(r, err)
	}
	if !strings.Contains(read(t, home, RecordsFile), `"scope":"dm:UANA"`) {
		t.Error("the scope is not in the header")
	}
	if _, got := s.Recall(ana, q, nil, 0, 0); !slices.Equal(got, []string{r.ID}) {
		t.Errorf("Ana's DM recalled %v", got)
	}
	for name, a := range map[string]Audience{"bob": bob, "channel": chan1, "job": job, "owner": owner} {
		if b, got := s.Recall(a, q, nil, 0, 0); len(got) != 0 {
			t.Errorf("%s recalled Ana's record:\n%s", name, b)
		}
		res, err := s.Search(Query{Text: "passport renewal", Audience: a})
		if err != nil || res.Total != 0 {
			t.Errorf("%s found it: %+v %v", name, res, err)
		}
		if a.Operator {
			continue // the owner's own runs reach any record by id
		}
		if res, _ := s.Search(Query{IDs: []string{r.ID}, Audience: a}); len(res.Results) != 0 || !slices.Equal(res.Missing, []string{r.ID}) {
			t.Errorf("%s fetched it by id: %+v", name, res)
		}
		text := "changed"
		if _, err := s.Update(a, r.ID, Change{Text: &text}, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s updated it: %v", name, err)
		}
		if err := s.Delete(a, r.ID, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s deleted it: %v", name, err)
		}
		if _, _, err := s.Pin(a, r.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s pinned it: %v", name, err)
		}
	}
	// every scope: the owner only, never a conversation or a job
	for name, a := range map[string]Audience{"ana": ana, "bob": bob, "channel": chan1, "job": job} {
		if _, err := s.Search(Query{Text: "passport", Audience: a, All: true}); !errors.Is(err, ErrAllScopes) {
			t.Errorf("%s searched every scope: %v", name, err)
		}
	}
	if res, err := s.Search(Query{Text: "passport", Audience: owner, All: true}); err != nil || res.Total != 1 || res.Results[0].Scope != "dm:UANA" {
		t.Errorf("the owner's search of every scope = %+v %v", res, err)
	}
	if res, _ := s.Search(Query{IDs: []string{r.ID}, Audience: owner}); len(res.Results) != 1 {
		t.Errorf("the owner's fetch by id = %+v", res)
	}
	// Bob saving the same text gets a record of his own, not Ana's id
	if b, err := s.Save(SaveOptions{Text: "The client's passport expires in March 2027; renewal booked.", Audience: bob}); err != nil || b.Status != StatusCreated || b.ID == r.ID {
		t.Errorf("Bob's save = %+v %v", b, err)
	}
	// nor does a near text show Ana's record as similar to Bob
	if b, err := s.Save(SaveOptions{Text: "The client's passport expires in March 2027.", Audience: bob}); err != nil || slices.ContainsFunc(b.Similar, func(x Similar) bool { return x.ID == r.ID }) {
		t.Errorf("Bob's near save = %+v %v", b, err)
	}
}

// A channel's records are the channel's: every thread of it sees them, a DM or another channel
// does not. An agent-wide record is everyone's.
func TestScopeChannelAndAgent(t *testing.T) {
	s, _ := scopedStore(t, "")
	c, err := s.Save(SaveOptions{Text: "The channel agreed quotes go out on Fridays.", Audience: chan1})
	if err != nil || c.Scope != "channel:C1" {
		t.Fatal(c, err)
	}
	g, err := s.Save(SaveOptions{Text: "The CRM exports invoices nightly at two.", Audience: chan1, Scope: ScopeAgent})
	if err != nil || g.Scope != ScopeAgent {
		t.Fatal(g, err)
	}
	for name, a := range map[string]Audience{"chan1b": chan1b, "chan2": chan2, "ana": ana, "job": job} {
		res, _ := s.Search(Query{Text: "quotes fridays invoices nightly", Audience: a})
		got := ids(res.Results)
		wantC := name == "chan1b"
		if slices.Contains(got, c.ID) != wantC || !slices.Contains(got, g.ID) {
			t.Errorf("%s found %v", name, got)
		}
	}
	// a job's "here" is agent-wide
	j, err := s.Save(SaveOptions{Text: "The nightly sync job runs at three.", Audience: job})
	if err != nil || j.Scope != ScopeAgent {
		t.Errorf("job save = %+v %v", j, err)
	}
}

// Personal data or a person's profile is never agent-wide, unless the owner (a oneshot) says so:
// an agent-wide record is recalled in every conversation.
func TestScopeAgentWideRefusesPersonalData(t *testing.T) {
	s, _ := scopedStore(t, "")
	for _, o := range []SaveOptions{
		{Text: "Ana's email is ana@example.com.", Audience: ana, Scope: ScopeAgent},
		{Text: "Ana prefers short answers.", Kind: KindUser, Audience: ana, Scope: ScopeAgent},
		{Text: "Call Ana at +1 415 555 0123 for renewals.", Audience: job},
		{Text: "Bob is the finance lead.", Kind: KindUser, Audience: job},
	} {
		if _, err := s.Save(o); !errors.Is(err, ErrAgentWide) {
			t.Errorf("%q (%+v): %v", o.Text, o.Audience, err)
		}
	}
	if r, err := s.Save(SaveOptions{Text: "Ana's email is ana@example.com.", Audience: ana}); err != nil || r.Scope != "dm:UANA" {
		t.Errorf("here in a DM = %+v %v", r, err)
	}
	if r, err := s.Save(SaveOptions{Text: "Bob's email is bob@example.com.", Audience: owner, Scope: ScopeAgent}); err != nil || r.Scope != ScopeAgent {
		t.Errorf("the owner's agent-wide save = %+v %v", r, err)
	}
	if _, err := s.Save(SaveOptions{Text: "A note.", Audience: ana, Scope: "everywhere"}); err == nil {
		t.Error("an unknown scope was taken")
	}
	// a DM whose person the runtime does not know: no "here" to save to
	if _, err := s.Save(SaveOptions{Text: "A plain note.", Audience: Audience{Conv: "slack-DXYZ-dm"}}); err == nil {
		t.Error("saved in a conversation of unknown audience")
	}
}

// Records saved before scopes existed: in their source conversation when src names a Slack one
// (a DM's records in that DM, a thread's in its channel), else agent-wide when they hold no
// personal data and are not about a person, else only in an owner's search of every scope.
func TestScopeOfOlderRecords(t *testing.T) {
	s, home := scopedStore(t, "")
	rec := func(id, kind, src, text string) string {
		return `<!-- {"id":"` + id + `","kind":"` + kind + `","tags":[],"imp":3,"hot":0,"src":"` + src + `","created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z"} -->` + "\n" + text
	}
	body := versionLine + Delimiter +
		rec("m00000000001", "fact", "chat:slack-DANA-dm", "Lisbon cruise deposit paid by card ending 4242.") + Delimiter +
		rec("m00000000002", "fact", "chat:slack-C1-500.5", "Lisbon cruise quotes go through the channel lead.") + Delimiter +
		rec("m00000000003", "fact", "boot", "Lisbon cruise season opens in April.") + Delimiter +
		rec("m00000000004", "fact", "luau:crm__sync", "Lisbon cruise contact lisbon@example.com.") + Delimiter +
		rec("m00000000005", "user", "boot", "Lisbon cruise lead is Marta.") + Delimiter +
		rec("m00000000006", "fact", "chat:api-session", "Lisbon cruise prices rise in May.") + "\n"
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(body), 0o640)
	cases := map[string]struct {
		a    Audience
		want []string
	}{
		"ana":    {ana, []string{"m00000000001", "m00000000003", "m00000000006"}},
		"bob":    {bob, []string{"m00000000003", "m00000000006"}},
		"chan1b": {chan1b, []string{"m00000000002", "m00000000003", "m00000000006"}},
		"chan2":  {chan2, []string{"m00000000003", "m00000000006"}},
		"job":    {job, []string{"m00000000003", "m00000000006"}},
	}
	for name, c := range cases {
		res, err := s.Search(Query{Text: "lisbon cruise", Audience: c.a, Limit: 20})
		got := ids(res.Results)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v (want %v) %v", name, got, c.want, err)
		}
	}
	if res, _ := s.Search(Query{Text: "lisbon cruise", Audience: owner, All: true, Limit: 20}); res.Total != 6 {
		t.Errorf("the owner's search of every scope: %d", res.Total)
	}
	if strings.Contains(read(t, home, RecordsFile), `"scope"`) {
		t.Error("an older record was given a scope on disk")
	}
}

// Hot records are in every conversation's system prompt (the hot files are global, as in
// Hermes): found anywhere while hot; unpinned, the record is its scope's again.
func TestScopeHotIsGlobal(t *testing.T) {
	s, _ := scopedStore(t, "")
	r, err := s.Save(SaveOptions{Text: "Ana's renewal desk closes at five on Fridays.", Audience: ana, Hot: true})
	if err != nil || r.Hot != 1 {
		t.Fatal(r, err)
	}
	if res, _ := s.Search(Query{Text: "renewal desk fridays", Audience: bob}); res.Total != 1 {
		t.Errorf("hot record not found from Bob's DM: %+v", res)
	}
	if _, _, err := s.Unpin(ana, r.ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Search(Query{Text: "renewal desk fridays", Audience: bob}); res.Total != 0 {
		t.Errorf("unpinned, still Bob's to find: %+v", res)
	}
	// the memory tool's entries carry the conversation's scope too
	if _, err := s.AddAs(bob, "chat:slack-DBOB-dm", TargetMemory, "Bob's invoices go to the Madrid office."); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search(Query{Text: "madrid office invoices", Audience: bob})
	if len(res.Results) != 1 || res.Results[0].Scope != "dm:UBOB" {
		t.Errorf("memory tool add = %+v", res)
	}
}
