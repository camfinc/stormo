// Package messaging is the runtime's one policy for the messages the agent sends on its own
// (docs/needletail.md §4.9): the send_message tool, the Luau send_message binding and the delivery
// of the scheduled jobs the agent made all go through it, so none of them reaches further than
// the others. A platform's gateway (Slack) applies it with what it knows of the agent's
// conversations; the policy itself reads nothing.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Tool names: send_message (Hermes') posts a message; no_reply lets the agent stay out of a
// conversation with other people. Neither is ever offered to a scheduled run (its reply is what
// is delivered).
const (
	SendTool    = "send_message"
	NoReplyTool = "no_reply"
)

// Target is where a message goes: Slack's home channel (no Channel and no User), a channel or a
// DM channel, a thread in one, or a person's DM (User).
type Target struct {
	Platform string
	Channel  string
	Thread   string
	User     string
}

// Home is whether the target is the home channel.
func (t Target) Home() bool { return t.Channel == "" && t.User == "" }

// String is the target in send_message's grammar, which message:<target> grants match:
// "slack" (the home channel), "slack:<channel>", "slack:<channel>:<thread_ts>", "slack:<user>".
func (t Target) String() string {
	switch {
	case t.User != "":
		return t.Platform + ":" + t.User
	case t.Channel == "":
		return t.Platform
	case t.Thread != "":
		return t.Platform + ":" + t.Channel + ":" + t.Thread
	}
	return t.Platform + ":" + t.Channel
}

var (
	channelRe = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)
	userRe    = regexp.MustCompile(`^<?@?([UW][A-Z0-9]{8,})>?$`)
	threadRe  = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,9}$`)
)

// Parse reads a target as send_message takes it: "slack" or "home" (or "") for the home
// channel, "slack:<channel id>", "slack:<channel id>:<thread ts>", "slack:<user id>" (also as
// <@U…>). Channel names (#general) and other platforms are refused.
func Parse(target string) (Target, error) {
	t := strings.TrimSpace(target)
	platform, rest, _ := strings.Cut(t, ":")
	switch p := strings.ToLower(platform); {
	case t == "" || p == "home" && rest == "":
		return Target{Platform: "slack"}, nil
	case p != "slack":
		return Target{}, fmt.Errorf("%q: only Slack targets are served (slack, slack:<channel>[:<thread_ts>], slack:<user>)", target)
	}
	if rest == "" || strings.EqualFold(rest, "home") {
		return Target{Platform: "slack"}, nil
	}
	if m := userRe.FindStringSubmatch(rest); m != nil {
		return Target{Platform: "slack", User: m[1]}, nil
	}
	channel, thread, _ := strings.Cut(rest, ":")
	if !channelRe.MatchString(channel) {
		return Target{}, fmt.Errorf("could not resolve %q: use a channel or user id (send_message action list shows the targets)", target)
	}
	if thread != "" && !threadRe.MatchString(thread) {
		return Target{}, fmt.Errorf("%q: a thread is a message ts (1700000000.000100)", target)
	}
	return Target{Platform: "slack", Channel: channel, Thread: thread}, nil
}

// Policy is who the agent may message on its own. By default: the home channel (and a thread in
// it), a channel or DM it has a conversation in, a thread it is engaged in (it has the thread's
// session, posted there or was mentioned there), and the DM of a person allowed_users names. A
// broadcast (@channel, @here, @everyone, a user group) only when the call asks for one and the
// config grants it (slack.broadcast).
type Policy struct {
	// Home is the home channel (SLACK_HOME_CHANNEL).
	Home string
	// AllowedUsers are the people the agent may DM (allowed_users, by id; "*" and
	// allow_all_users do not make everyone a target).
	AllowedUsers []string
	// Broadcast is the config's grant (needletail.yaml slack.broadcast).
	Broadcast bool
	// Engaged is whether the agent is in a conversation: a channel (thread "") it has a
	// conversation in, or a thread it is engaged in.
	Engaged func(channel, thread string) bool
}

// ErrRefused wraps every policy refusal.
var ErrRefused = errors.New("not a target you may message")

// Check is whether the agent may send to t, and with a broadcast when it asks for one; the
// answer is whether the text may keep its broadcast tokens.
func (p Policy) Check(t Target, broadcast bool) (bool, error) {
	if t.Platform != "slack" {
		return false, fmt.Errorf("%w: %s (only Slack is served)", ErrRefused, t)
	}
	if broadcast && !p.Broadcast {
		return false, fmt.Errorf("%w: a broadcast (@channel, @here, @everyone, a user group) needs the agent's slack.broadcast grant", ErrRefused)
	}
	engaged := func(channel, thread string) bool { return p.Engaged != nil && p.Engaged(channel, thread) }
	switch {
	case t.User != "":
		if !slices.Contains(p.AllowedUsers, t.User) {
			return false, fmt.Errorf("%w: %s is not in allowed_users", ErrRefused, t.User)
		}
	case t.Channel == "" || t.Channel == p.Home:
		if p.Home == "" {
			return false, fmt.Errorf("%w: no home channel is set; name a channel", ErrRefused)
		}
	case t.Thread == "":
		if !engaged(t.Channel, "") {
			return false, fmt.Errorf("%w: %s is not a channel you have a conversation in (send_message action list shows the targets)", ErrRefused, t.Channel)
		}
	default:
		if !engaged(t.Channel, t.Thread) {
			return false, fmt.Errorf("%w: thread %s in %s is not one you are in (you posted there, were mentioned there or talked there)", ErrRefused, t.Thread, t.Channel)
		}
	}
	return broadcast, nil
}

// Request is one message the agent sends on its own.
type Request struct {
	// Target in send_message's grammar (Parse).
	Target string
	// Text is the model's Markdown.
	Text string
	// Broadcast asks to keep @channel, @here, @everyone and user groups (granted or refused).
	Broadcast bool
}

// Sent is a message posted.
type Sent struct {
	Platform string `json:"platform"`
	Channel  string `json:"chat_id"`
	TS       string `json:"message_id"`
	Home     bool   `json:"home,omitempty"`
}

// Messenger sends a message under the policy: the platform's gateway.
type Messenger interface {
	Send(ctx context.Context, r Request) (Sent, error)
}

// Grant matching for Luau's message:<target> capabilities.

// GrantMatches is whether a message:<pattern> grant (or declaration) covers target (in
// Target.String's form): the target itself, "*" for any, or a prefix ending in ":*"
// ("slack:*" every Slack target, the home channel included; "slack:C0…:*" every thread of a
// channel, and the channel itself).
func GrantMatches(pattern, target string) bool {
	if pattern == "*" || pattern == target {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
		return target == prefix || strings.HasPrefix(target, prefix+":")
	}
	return false
}
