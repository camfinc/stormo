package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Target is where a run's output goes: a platform's home channel (Channel ""), a channel, or a
// thread in one.
type Target struct {
	Platform string `json:"platform"`
	Channel  string `json:"channel,omitempty"`
	Thread   string `json:"thread,omitempty"`
}

func (t Target) String() string {
	s := t.Platform
	if t.Channel == "" {
		return s + " (home)"
	}
	s += ":" + t.Channel
	if t.Thread != "" {
		s += ":" + t.Thread
	}
	return s
}

// Delivery is one run's output for one target.
type Delivery struct {
	JobID, JobName string
	Target         Target
	Text           string
	// Failure: the text is a failure notice.
	Failure bool
	// Session is the run's session ("" for a script-only job).
	Session string
	// Trusted: the job is the repo's (agent.yaml defines it), so its targets are the repo's
	// and the deliverer takes them as they are. Any other job's targets (a job the agent made)
	// go through the messaging policy.
	Trusted bool
}

// DeliveryEvent is metadata from a durably retained failed delivery report or
// its later completion. ID is the stable outbox ID plus :failed or :recovered.
// It deliberately excludes payloads, provider errors, targets and job names.
type DeliveryEvent struct {
	ID, JobID, Session string
	At                 time.Time
	Status             string // failed or recovered; normal successful delivery emits no event
	Failure            bool   // the original execution failed, independent of delivery recovery
}

// Deliverer sends a delivery (the Slack gateway's, from M5).
type Deliverer interface {
	Deliver(ctx context.Context, d Delivery) error
}

// Targets reads a job's deliver (or failure_deliver) value as Hermes does: comma-separated
// targets; "" and "local" deliver nowhere (the run is only recorded); "origin" is the chat the
// job was made from (its origin), else the home channel; "all" and "home" are the home channel;
// "slack" is Slack's home channel, "slack:<channel>[:<thread>]" a channel or thread. Any other
// platform is kept as named, for the deliverer to refuse.
func Targets(lane string, origin json.RawMessage) ([]Target, error) {
	out := []Target{}
	seen := map[Target]bool{}
	add := func(t Target) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, raw := range strings.Split(lane, ",") {
		part := strings.TrimSpace(raw)
		switch strings.ToLower(part) {
		case "", "local":
			continue
		case "all", "home":
			add(Target{Platform: "slack"})
			continue
		case "origin":
			var o struct {
				Platform string `json:"platform"`
				ChatID   any    `json:"chat_id"`
				ThreadID any    `json:"thread_id"`
			}
			if len(origin) > 0 && json.Unmarshal(origin, &o) == nil && o.Platform != "" && o.ChatID != nil {
				t := Target{Platform: strings.ToLower(o.Platform), Channel: fmt.Sprint(o.ChatID)}
				if o.ThreadID != nil {
					t.Thread = fmt.Sprint(o.ThreadID)
				}
				add(t)
			} else {
				add(Target{Platform: "slack"})
			}
			continue
		}
		platform, rest, _ := strings.Cut(part, ":")
		t := Target{Platform: strings.ToLower(platform)}
		if rest != "" {
			t.Channel, t.Thread, _ = strings.Cut(rest, ":")
		}
		if t.Platform == "" {
			return out, fmt.Errorf("bad delivery target %q", part)
		}
		add(t)
	}
	return out, nil
}

// Pending is the deliverer until a chat gateway takes its place: a Slack or Telegram target is
// refused until the gateway is available (the outbox retains the report), and any other
// platform is refused (needletail serves Slack and Telegram only).
type Pending struct{ Log func(string) }

// Deliver implements Deliverer.
func (p Pending) Deliver(_ context.Context, d Delivery) error {
	if d.Target.Platform != "slack" && d.Target.Platform != "telegram" {
		return fmt.Errorf("%w: needletail does not deliver to %s", ErrUnsupportedPlatform, d.Target.Platform)
	}
	if p.Log != nil {
		p.Log(fmt.Sprintf("cron: job %s: delivery to %s waits for its gateway; the output is kept in state/needletail/jobs/", d.JobID, d.Target))
	}
	return fmt.Errorf("the %s gateway is not ready; delivery remains pending", d.Target.Platform)
}
