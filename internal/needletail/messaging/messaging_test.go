package messaging

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "slack",
		"slack":                              "slack",
		"home":                               "slack",
		"Slack:home":                         "slack",
		"slack:C0DEVCHAN1":                   "slack:C0DEVCHAN1",
		"slack:C0DEVCHAN1:1700000070.000100": "slack:C0DEVCHAN1:1700000070.000100",
		"slack:U0ALLOWED1":                   "slack:U0ALLOWED1",
		"slack:<@U0ALLOWED1>":                "slack:U0ALLOWED1",
		"slack:D0DMCHAN01":                   "slack:D0DMCHAN01",
		"telegram":                           "telegram",
		"telegram:-1001234567890":            "telegram:-1001234567890",
		"telegram:42:7":                      "telegram:42:7",
	} {
		got, err := Parse(in)
		if err != nil || got.String() != want {
			t.Errorf("%q: %v %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"slack:#general", "telegram:abc", "telegram:-100:x", "slack:C0DEVCHAN1:<!channel>", "slack:c0lower", "discord"} {
		if got, err := Parse(bad); err == nil {
			t.Errorf("%q parsed as %v", bad, got)
		}
	}
}

// The default policy: the home channel (and its threads), a channel or thread the agent is in,
// an allowed user's DM; a broadcast only when asked and granted.
func TestPolicy(t *testing.T) {
	engaged := map[string]bool{"C0DEVCHAN1:": true, "C0DEVCHAN1:1700000002.000100": true, "D0DMCHAN01:": true, "D0STRANGE1:": true}
	dms := map[string]string{"D0DMCHAN01": "U0ALLOWED1", "D0STRANGE1": "U0STRANGER", "D0OTHERDM1": "U0STRANGER"}
	p := Policy{Home: "C0HOME0001", AllowedUsers: []string{"U0ALLOWED1", "*"},
		Engaged: func(c, th string) bool { return engaged[c+":"+th] },
		DMUser: func(c string) (string, error) {
			if u, ok := dms[c]; ok {
				return u, nil
			}
			return "", errors.New("channel_not_found")
		}}
	for _, ok := range []string{"slack", "slack:C0HOME0001", "slack:C0HOME0001:1700000009.000100", "slack:C0DEVCHAN1",
		"slack:C0DEVCHAN1:1700000002.000100", "slack:U0ALLOWED1", "slack:D0DMCHAN01"} {
		tg, _ := Parse(ok)
		if _, err := p.Check(tg, false); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// a DM channel is its person's DM: allowed_users decides, a conversation there does not
	for _, bad := range []string{"slack:C0SECRET01", "slack:C0DEVCHAN1:1700000003.000100", "slack:U0STRANGER", "slack:D0OTHERDM1",
		"slack:D0STRANGE1", "slack:D0STRANGE1:1700000004.000100", "slack:D0UNKNOWN1"} {
		tg, _ := Parse(bad)
		if _, err := p.Check(tg, false); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	home, _ := Parse("slack")
	if _, err := p.Check(home, true); !errors.Is(err, ErrRefused) {
		t.Errorf("a broadcast without the grant: %v", err)
	}
	p.Broadcast = true
	if b, err := p.Check(home, true); err != nil || !b {
		t.Errorf("a granted broadcast: %v %v", b, err)
	}
	if b, _ := p.Check(home, false); b {
		t.Error("a broadcast nobody asked for")
	}
	if _, err := (Policy{}).Check(home, false); err == nil {
		t.Error("the home channel with none set")
	}
	dmTarget, _ := Parse("slack:D0DMCHAN01")
	if _, err := (Policy{AllowedUsers: []string{"U0ALLOWED1"}, Engaged: func(string, string) bool { return true }}).Check(dmTarget, false); !errors.Is(err, ErrRefused) {
		t.Errorf("a DM channel whose person cannot be told: %v", err)
	}
}

func TestGrantMatches(t *testing.T) {
	for _, c := range []struct {
		pattern, target string
		want            bool
	}{
		{"slack", "slack", true},
		{"slack", "slack:C0DEVCHAN1", false},
		{"slack:*", "slack", true},
		{"slack:*", "slack:U0ALLOWED1", true},
		{"slack:C0DEVCHAN1:*", "slack:C0DEVCHAN1:1700000002.000100", true},
		{"slack:C0DEVCHAN1:*", "slack:C0DEVCHAN1", true},
		{"slack:C0DEVCHAN1:*", "slack:C0DEVCHAN10", false},
		{"slack:C0DEVCHAN1", "slack:C0DEVCHAN1:1700000002.000100", false},
		{"*", "slack:C0X", true},
	} {
		if got := GrantMatches(c.pattern, c.target); got != c.want {
			t.Errorf("%s ~ %s = %v", c.pattern, c.target, got)
		}
	}
}
