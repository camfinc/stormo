package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// APIBase is Slack's Web API (a variable so tests can point it at a fake).
var APIBase = "https://slack.com/api/"

// Directory is what one agent's Slack bot can see in its workspace: the people (and other bots)
// to pick for allowed_users, and the channels and DMs to pick for a home channel or a delivery
// target. Read with the agent's own bot token, so it is that bot's view (private channels only
// where it is a member).
type Directory struct {
	Team     Team      `json:"team"`
	Bot      Person    `json:"bot"`
	People   []Person  `json:"people"`
	Channels []Channel `json:"channels"`
	DMs      []DM      `json:"dms"`
	Fetched  time.Time `json:"fetchedAt"`
	Partial  []string  `json:"partial,omitempty"` // what could not be read (a missing scope), so the rest still serves
}

type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Person is a workspace member. Guests (restricted, single-channel) and bots are flagged: under
// needletail only full members of the agent's own workspace (or listed ids) count as verified.
type Person struct {
	ID       string `json:"id"`
	Name     string `json:"name"`               // display name, else real name, else handle
	RealName string `json:"realName,omitempty"` // as in the profile
	Handle   string `json:"handle,omitempty"`   // the legacy @name
	Bot      bool   `json:"bot,omitempty"`
	Guest    bool   `json:"guest,omitempty"`
	Owner    bool   `json:"owner,omitempty"` // workspace owner or admin
	TZ       string `json:"tz,omitempty"`
}

// Channel is a public channel, or a private one the bot is in.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"private,omitempty"`
	Member  bool   `json:"member"` // the bot is in it (it can post there)
}

// DM is a direct message the bot already has with a person.
type DM struct {
	ID   string `json:"id"`
	User string `json:"user"`
}

// FetchDirectory reads the directory with a bot token: auth.test, users.list and
// conversations.list, every page. A list the token lacks the scope for is left empty and named in
// Partial; any other Slack error fails the whole read.
func FetchDirectory(ctx context.Context, client *http.Client, token string) (*Directory, error) {
	var auth struct {
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
	}
	if err := call(ctx, client, token, "auth.test", nil, &auth); err != nil {
		return nil, err
	}
	d := &Directory{Team: Team{ID: auth.TeamID, Name: auth.Team}, Bot: Person{ID: auth.UserID, Name: auth.User, Bot: true},
		People: []Person{}, Channels: []Channel{}, DMs: []DM{}}

	err := pages(ctx, client, token, "users.list", url.Values{"limit": {"200"}}, func(raw json.RawMessage) error {
		var page struct {
			Members []struct {
				ID                string `json:"id"`
				Name              string `json:"name"`
				Deleted           bool   `json:"deleted"`
				IsBot             bool   `json:"is_bot"`
				IsAppUser         bool   `json:"is_app_user"`
				IsRestricted      bool   `json:"is_restricted"`
				IsUltraRestricted bool   `json:"is_ultra_restricted"`
				IsOwner           bool   `json:"is_owner"`
				IsAdmin           bool   `json:"is_admin"`
				TZ                string `json:"tz"`
				Profile           struct {
					RealName    string `json:"real_name"`
					DisplayName string `json:"display_name"`
				} `json:"profile"`
			} `json:"members"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, m := range page.Members {
			if m.Deleted || m.ID == "USLACKBOT" {
				continue
			}
			name := firstNonEmpty(m.Profile.DisplayName, m.Profile.RealName, m.Name)
			d.People = append(d.People, Person{ID: m.ID, Name: name, RealName: m.Profile.RealName, Handle: m.Name,
				Bot: m.IsBot || m.IsAppUser, Guest: m.IsRestricted || m.IsUltraRestricted, Owner: m.IsOwner || m.IsAdmin, TZ: m.TZ})
		}
		return nil
	})
	if err := partial(d, "users", err); err != nil {
		return nil, err
	}

	q := url.Values{"limit": {"200"}, "exclude_archived": {"true"}, "types": {"public_channel,private_channel,im"}}
	err = pages(ctx, client, token, "conversations.list", q, func(raw json.RawMessage) error {
		var page struct {
			Channels []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				IsIM      bool   `json:"is_im"`
				IsPrivate bool   `json:"is_private"`
				IsMember  bool   `json:"is_member"`
				User      string `json:"user"`
			} `json:"channels"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, c := range page.Channels {
			if c.IsIM {
				d.DMs = append(d.DMs, DM{ID: c.ID, User: c.User})
				continue
			}
			d.Channels = append(d.Channels, Channel{ID: c.ID, Name: c.Name, Private: c.IsPrivate, Member: c.IsMember})
		}
		return nil
	})
	if err := partial(d, "channels", err); err != nil {
		return nil, err
	}
	sort.Slice(d.People, func(i, j int) bool {
		if d.People[i].Bot != d.People[j].Bot {
			return !d.People[i].Bot
		}
		return strings.ToLower(d.People[i].Name) < strings.ToLower(d.People[j].Name)
	})
	sort.Slice(d.Channels, func(i, j int) bool { return d.Channels[i].Name < d.Channels[j].Name })
	d.Fetched = time.Now().UTC()
	return d, nil
}

// OpenDM returns the bot's direct message with a person (conversations.open: it opens the DM if
// there is none, and posts nothing), the channel id a home channel or a delivery needs.
func OpenDM(ctx context.Context, client *http.Client, token, user string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := call(ctx, client, token, "conversations.open", url.Values{"users": {user}}, &out); err != nil {
		return "", err
	}
	return out.Channel.ID, nil
}

// Error is a Slack Web API error (`ok: false`).
type Error struct {
	Method, Code, Needed string
	RetryAfter           time.Duration
}

func (e *Error) Error() string {
	s := e.Method + ": " + e.Code
	if e.Needed != "" {
		s += " (needs " + e.Needed + ")"
	}
	return s
}

func partial(d *Directory, what string, err error) error {
	if se, ok := err.(*Error); ok && se.Code == "missing_scope" {
		d.Partial = append(d.Partial, fmt.Sprintf("%s: the bot token lacks %s", what, se.Needed))
		return nil
	}
	return err
}

// pages calls a cursor-paginated method until its last page (at most 50, a bound on a runaway
// cursor).
func pages(ctx context.Context, client *http.Client, token, method string, q url.Values, each func(json.RawMessage) error) error {
	cursor := ""
	for range 50 {
		v := url.Values{}
		for k, vs := range q {
			v[k] = vs
		}
		if cursor != "" {
			v.Set("cursor", cursor)
		}
		var raw json.RawMessage
		if err := call(ctx, client, token, method, v, &raw); err != nil {
			return err
		}
		if err := each(raw); err != nil {
			return err
		}
		var meta struct {
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		_ = json.Unmarshal(raw, &meta)
		if cursor = meta.Meta.Next; cursor == "" {
			return nil
		}
	}
	return fmt.Errorf("%s: more than 50 pages", method)
}

// call POSTs a form to one Web API method and decodes the answer into out.
func call(ctx context.Context, client *http.Client, token, method string, form url.Values, out any) error {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		after, _ := time.ParseDuration(res.Header.Get("Retry-After") + "s")
		return &Error{Method: method, Code: "ratelimited", RetryAfter: after}
	}
	var head struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("%s: HTTP %d", method, res.StatusCode)
	}
	if !head.OK {
		return &Error{Method: method, Code: head.Error, Needed: head.Needed}
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = body
		return nil
	}
	return json.Unmarshal(body, out)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
