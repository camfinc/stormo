package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/slack"
)

// The owner's view of an agent's Slack workspace, so a client (the app) can offer people and
// channels by name instead of asking for ids: GET /api/agents/<id>/slack (?refresh=1 skips the
// cache), POST /api/agents/<id>/slack/dm {"user": "U…"} for the bot's DM with a person. Read with
// the agent's own bot token as a local run sees it (as SlackMirror posts), whatever its engine;
// people's names are workspace data, so the owner only, cached in memory, never stored.
var slackDirectoryPath = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/slack(/dm)?$`)

// SlackDirectoryTTL is how long a directory is served from memory.
const SlackDirectoryTTL = 5 * time.Minute

var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)

type slackDirectories struct {
	mu     sync.Mutex
	m      map[string]slackDirectoryEntry
	client *http.Client // nil: slack's default
}

type slackDirectoryEntry struct {
	dir *slack.Directory
	at  time.Time
}

func (s *slackDirectories) get(agent string) *slack.Directory {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[agent]; ok && time.Since(e.at) < SlackDirectoryTTL {
		return e.dir
	}
	return nil
}

func (s *slackDirectories) put(agent string, d *slack.Directory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]slackDirectoryEntry{}
	}
	s.m[agent] = slackDirectoryEntry{d, time.Now()}
}

// addDM records a DM just opened in the cached directory, if there is one.
func (s *slackDirectories) addDM(agent string, dm slack.DM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[agent]
	if !ok {
		return
	}
	for _, d := range e.dir.DMs {
		if d.ID == dm.ID {
			return
		}
	}
	cp := *e.dir
	cp.DMs = append(append([]slack.DM{}, e.dir.DMs...), dm)
	s.m[agent] = slackDirectoryEntry{&cp, e.at}
}

// agentSlackToken is the agent's Slack bot token as a local run sees it (the `local:` overlay
// applied); empty when it has none.
func agentSlackToken(inst *instance.Instance, a *manifest.Agent) (string, error) {
	file, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return "", err
	}
	token, _ := secrets.Resolve(file, a, manifest.Local).Values.Get("SLACK_BOT_TOKEN")
	return token, nil
}

func (c *Core) handleSlackDirectory(w http.ResponseWriter, r *http.Request, agent string, dm bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !c.caller(r).Owner {
		writeJSONBody(w, 401, errBody("unauthorized", "the owner token is required"))
		return
	}
	if (dm && r.Method != http.MethodPost) || (!dm && r.Method != http.MethodGet) {
		writeJSONBody(w, 405, errBody("method", "GET …/slack, POST …/slack/dm"))
		return
	}
	a, err := manifest.Load(c.inst.Root, agent, c.inst.Names.Secret)
	if err != nil {
		writeJSONBody(w, 404, errBody("unknown_agent", "no agent "+agent))
		return
	}
	hasSlack := false
	for _, ch := range a.Channels {
		hasSlack = hasSlack || ch.Kind == "slack"
	}
	token, err := agentSlackToken(c.inst, a)
	if err != nil {
		writeJSONBody(w, 500, errBody("core_internal", "could not read the secrets"))
		return
	}
	if !hasSlack || token == "" {
		writeJSONBody(w, 404, errBody("no_slack", fmt.Sprintf("%s has no Slack channel with a bot token for a local run", agent)))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if dm {
		var req struct {
			User string `json:"user"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || !slackUserID.MatchString(req.User) {
			writeJSONBody(w, 400, errBody("bad_request", `send {"user": "U…"}`))
			return
		}
		id, err := slack.OpenDM(ctx, c.slackDirs.client, token, req.User)
		if err != nil {
			slackFailure(w, err)
			return
		}
		c.slackDirs.addDM(agent, slack.DM{ID: id, User: req.User})
		writeJSONBody(w, 200, map[string]string{"channel": id, "user": req.User})
		return
	}
	if d := c.slackDirs.get(agent); d != nil && r.URL.Query().Get("refresh") != "1" {
		writeJSONBody(w, 200, d)
		return
	}
	d, err := slack.FetchDirectory(ctx, c.slackDirs.client, token)
	if err != nil {
		slackFailure(w, err)
		return
	}
	c.slackDirs.put(agent, d)
	writeJSONBody(w, 200, d)
}

// slackFailure answers a Slack error: 429 with Retry-After when rate limited, else 502 with
// Slack's code (invalid_auth, missing_scope, …), so a client can say what to fix.
func slackFailure(w http.ResponseWriter, err error) {
	var se *slack.Error
	if errors.As(err, &se) {
		if se.Code == "ratelimited" {
			if se.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(se.RetryAfter.Seconds())))
			}
			writeJSONBody(w, 429, errBody("slack_ratelimited", se.Error()))
			return
		}
		writeJSONBody(w, 502, errBody("slack_"+se.Code, se.Error()))
		return
	}
	writeJSONBody(w, 502, errBody("slack_unreachable", err.Error()))
}
