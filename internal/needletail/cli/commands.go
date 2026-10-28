package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/commands"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sched"
)

// restartNote is the one row a /new leaves in the fresh session (a system row: the app does not
// show it; the model reads it).
const restartNote = "[A person started a fresh conversation (/new). The earlier messages of this conversation are archived and not in your context; " +
	"session_search finds them if the person asks about them.]"

// trustConversation is the owner's (or an approver's) endorsement of a conversation, as the API's
// trust route and /trust make it: its rows so far no longer taint it, with an audit record.
func (rt *runtime) trustConversation(conv, by string) error {
	if err := rt.store.Trust(conv, by); err != nil {
		return err
	}
	if svc := rt.approvals.Load(); svc != nil {
		svc.Audit("trusted", conv, by)
	} else {
		approvals.AppendAudit(filepath.Join(rt.cfg.StateDir(), "approvals-audit.jsonl"), map[string]string{"at": time.Now().UTC().Format(time.RFC3339Nano), "event": "trusted", "conversation": conv, "by": by}, nil)
	}
	return nil
}

// commandBackend is what the chat commands act on (docs/needletail.md §4.9): the sessions, the
// API's turns, the scheduler, the approvals and the memory, every one in the conversation the
// gateway named (its root), never one the command's text names. platforms are the gateways'
// states.
func (rt *runtime) commandBackend(srv *api.Server, platforms func() map[string]string) commands.Backend {
	root := func(conv string) (string, bool) { return rt.store.Root(conv) }
	hold := func(conv string) (func(), error) {
		release, err := srv.Hold(conv)
		if errors.Is(err, api.ErrSessionBusy) {
			return nil, commands.ErrBusy
		}
		return release, err
	}
	return commands.Backend{
		New: func(_ context.Context, conv, by string) error {
			r, ok := root(conv)
			if !ok {
				return nil // never talked here: already fresh
			}
			release, err := hold(r)
			if err != nil {
				return err
			}
			defer release()
			if _, err := rt.store.Restart(r, restartNote); err != nil {
				return err
			}
			// the conversation's background processes go with its context
			rt.agent.Env.Processes.EndConversation(r, "new")
			rt.logger.Printf("commands: %s started a fresh conversation in %s (/new)", by, r)
			return nil
		},
		Stop: func(conv string) bool {
			stopped := srv.Cancel(conv)
			if stopped {
				rt.logger.Printf("commands: the turn in %s was stopped (/stop)", conv)
			}
			return stopped
		},
		Trust: func(_ context.Context, conv, by string) error {
			r, ok := root(conv)
			if !ok {
				return errors.New("no such conversation")
			}
			release, err := hold(r)
			if err != nil {
				return err
			}
			defer release()
			return rt.trustConversation(r, by+" (/trust)")
		},
		Status: func(conv string) commands.State {
			st := commands.State{Agent: rt.cfg.Agent, Model: rt.cfg.Model.Primary.Model, Platforms: platforms()}
			st.Running, _ = rt.sched.Running()
			if f, err := sched.ReadFile(filepath.Join(rt.cfg.Home, "cron", "jobs.json")); err == nil {
				for _, j := range f.Jobs {
					st.Jobs++
					if j.Enabled() {
						st.Enabled++
					}
				}
			}
			r, ok := root(conv)
			if svc := rt.approvals.Load(); svc != nil {
				for _, req := range svc.List() {
					if req.Pending() {
						st.Approvals++
						if ok && req.Conversation == r {
							st.ApprovalsHere++
						}
					}
				}
			}
			if !ok {
				return st
			}
			st.Turn = srv.Running(r)
			st.Untrusted = rt.store.Untrusted(r)
			for _, t := range rt.store.Taint(r) {
				st.Taint = append(st.Taint, commands.Taint{Why: t.Why, Users: t.Users})
			}
			return st
		},
		Search: func(conv, q string) ([]commands.Record, int, error) {
			m := rt.agent.Env.Memory
			if m == nil {
				return nil, 0, errors.New("no memory")
			}
			a := memory.Audience{}
			if m.Where != nil && conv != "" {
				a = m.Where(conv)
			}
			a.Operator = false // a person in a chat is never the owner's oneshot
			res, err := m.Search(memory.Query{Text: q, Limit: 8, Audience: a})
			if err != nil {
				return nil, 0, err
			}
			out := []commands.Record{}
			for _, v := range res.Results {
				out = append(out, commands.Record{ID: v.ID, Kind: v.Kind, Scope: v.Scope, Text: v.Text, Marked: v.Untrusted})
			}
			return out, res.Total, nil
		},
		Hot: func() ([]string, error) {
			m := rt.agent.Env.Memory
			if m == nil {
				return nil, errors.New("no memory")
			}
			out := []string{}
			for _, target := range []string{"memory", "user"} {
				entries, err := m.Entries(target)
				if err != nil {
					return nil, err
				}
				out = append(out, entries...)
			}
			return out, nil
		},
		Jobs: func() ([]commands.Job, error) {
			f, err := sched.ReadFile(filepath.Join(rt.cfg.Home, "cron", "jobs.json"))
			if err != nil {
				return nil, fmt.Errorf("could not read the jobs file")
			}
			running, _ := rt.sched.Running()
			out := []commands.Job{}
			for _, j := range f.Jobs {
				s := j.Schedule()
				display := s.Display
				if display == "" {
					display = s.Expr
				}
				out = append(out, commands.Job{Name: j.Name(), Schedule: display, Enabled: j.Enabled(), Running: slices.Contains(running, j.Name()),
					Next: commands.When(j.Str("next_run_at"), rt.cfg.Location), Last: j.Str("last_status")})
			}
			return out, nil
		},
	}
}
