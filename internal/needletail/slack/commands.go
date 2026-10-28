package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/commands"
)

// Slash commands (docs/needletail.md §4.9): Socket Mode delivers a slash_commands envelope for
// each command the app registers; the socket acks it at once (Slack wants it within 3 s) and hands
// it here, off the reader. The caller is the payload's user_id, as the authenticated socket gives
// it; allowed_users and verification gate it as they gate a message, and every answer goes to the
// caller alone (response_url, ephemeral; chat.postEphemeral when that fails), never to the channel.

// Commands are the gateway's chat commands (SetCommands).
type Commands struct {
	// Names are the commands' Slack names (key → name without the slash, commands.SlackNames).
	Names map[string]string
	// Backend is what they act on.
	Backend commands.Backend
	// Approvers are the user ids approvals.approvers names (/trust, /memory's hot list).
	Approvers func() []string
}

// tsRe is a message ts (a thread's).
var tsRe = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,9}$`)

// maxCommands is how many commands are answered at once; past it one is told to try again.
const maxCommands = 8

// slashPayload is a slash command's payload (the fields used here).
type slashPayload struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	UserID      string `json:"user_id"`
	ChannelID   string `json:"channel_id"`
	TeamID      string `json:"team_id"`
	ResponseURL string `json:"response_url"`
	ThreadTS    string `json:"thread_ts"`
}

// SetCommands turns the chat commands on (serve, once the runtime's backend is built); before it,
// a command is answered "try again".
func (g *Gateway) SetCommands(c Commands) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commands = &c
}

// slash takes a slash command from the socket (acked already) and answers it off the reader.
func (g *Gateway) slash(payload json.RawMessage) {
	var p slashPayload
	if json.Unmarshal(payload, &p) != nil || p.UserID == "" || p.ChannelID == "" || p.Command == "" {
		return
	}
	g.mu.Lock()
	c, closing := g.commands, g.closing
	busy := g.slashes >= maxCommands
	if !busy {
		g.slashes++
	}
	g.mu.Unlock()
	g.notes.Add(1)
	go func() {
		defer g.notes.Done()
		if !busy {
			defer func() {
				g.mu.Lock()
				g.slashes--
				g.mu.Unlock()
			}()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var text string
		switch {
		case closing:
			text = "I am restarting: nothing was done. Try again in a minute."
		case c == nil:
			text = "My commands are not running yet (I am starting): nothing was done. Try again in a minute."
		case busy:
			text = "I am answering too many commands at once: nothing was done. Try again in a moment."
		default:
			text = g.runCommand(ctx, c, p)
		}
		g.respond(ctx, p, text)
	}()
}

// runCommand is the answer to one command: its key from its name (an unknown one is named as
// such), the caller admitted by allowed_users and verified as a message's author would be, and the
// conversation it applies to (a DM's; a thread's when Slack gives one; none in a channel).
func (g *Gateway) runCommand(ctx context.Context, c *Commands, p slashPayload) string {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(p.Command)), "/")
	key := ""
	for k, n := range c.Names {
		if n == name {
			key = k
		}
	}
	names := map[string]string{}
	for k, n := range c.Names {
		names[k] = "/" + n
	}
	if !g.admitted(p.UserID, "") {
		g.warnOnce("slash:"+p.UserID, fmt.Sprintf("slack: a command from %s, who is not in allowed_users, is refused", p.UserID))
		return "You may not use my commands."
	}
	if key == "" {
		return commands.Unknown("/"+name, names)
	}
	budget := maxLookups
	call := commands.Call{Key: key, Args: p.Text, Platform: Source, User: p.UserID, Names: names,
		Verified: g.verified(p.UserID, "", "", false, &budget)}
	if c.Approvers != nil {
		call.Approver = slices.Contains(c.Approvers(), p.UserID)
	}
	dm := strings.HasPrefix(p.ChannelID, "D")
	switch {
	case dm:
		g.rememberDM(p.ChannelID, p.UserID) // the DM's person is who writes there
		call.Conversation = DMSessionID(p.ChannelID)
	case tsRe.MatchString(p.ThreadTS) && g.o.Exists(SessionID(p.ChannelID, p.ThreadTS)):
		call.Conversation = SessionID(p.ChannelID, p.ThreadTS)
	}
	g.logf("slack: command /%s from %s in %s", name, p.UserID, p.ChannelID)
	return commands.Run(ctx, c.Backend, call)
}

// respond answers a command's caller alone: through its response_url (ephemeral), else
// chat.postEphemeral; redacted and converted as any message (broadcasts escaped).
func (g *Gateway) respond(ctx context.Context, p slashPayload, text string) {
	parts := Split(Format(g.o.Redact(text), false), MaxMessage)
	if len(parts) > 5 {
		parts = parts[:5] // a response_url takes five answers
	}
	for _, part := range parts {
		err := g.o.Web.Respond(ctx, p.ResponseURL, part)
		if err == nil {
			continue
		}
		g.logf("slack: a command's answer through its response_url failed (%v); sending it as an ephemeral message", err)
		if err := g.o.Web.Ephemeral(ctx, p.ChannelID, p.UserID, part); err != nil {
			g.logf("slack: could not answer %s's command: %v", p.UserID, err)
			return
		}
	}
}

// Respond posts an ephemeral answer to a command's response_url: only to Slack's own https hosts
// (as file downloads, fileHostAllowed; under a test override a loopback address only), with no
// token, following no redirect.
func (w *Web) Respond(ctx context.Context, responseURL, text string) error {
	u, err := url.Parse(responseURL)
	if err != nil || responseURL == "" || !w.fileHostAllowed(u) {
		return errors.New("the response_url is not on Slack's own host")
	}
	body, _ := json.Marshal(map[string]any{"response_type": "ephemeral", "replace_original": false, "text": text})
	hc := &http.Client{Timeout: 15 * time.Second}
	if w.HTTP != nil {
		c := *w.HTTP
		c.Timeout = 15 * time.Second
		hc = &c
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirected: refused") }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("bad response_url")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("post failed: %v", ue.Err) // without the URL
		}
		return errors.New("post failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
