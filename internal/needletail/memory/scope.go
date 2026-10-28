package memory

import (
	"errors"
	"fmt"
	"strings"
)

// Scopes (docs/needletail.md §4.6): every record says who may be shown it, and recall,
// memory_search and the changes by id show a turn only the records its audience may see. The hot
// files are not scoped: they are in every conversation's system prompt (Hermes parity), so a hot
// record is anyone's while it is hot, and its scope's again once unpinned.

// ScopeAgent is a record everyone may be shown: every conversation, job and script of the agent.
const ScopeAgent = "agent"

// DMScope is a record saved in person's DM (a Slack user id); ChannelScope one saved in a
// channel's thread (every thread of that channel sees it).
func DMScope(person string) string       { return "dm:" + person }
func ChannelScope(channel string) string { return "channel:" + channel }

// Audience is who hears a turn: the runtime's, from the turn's session (its root and source, the
// Slack gateway's), never from what the model says.
type Audience struct {
	// Scope is the conversation's: DMScope of the DM's person, ChannelScope of a thread's
	// channel; "" for a turn with no conversation (a scheduled job, a script, the API's runs) or
	// a DM whose person the runtime does not know.
	Scope string
	// Conv is the conversation (its root session) of a Slack DM or thread; "" for none.
	Conv string
	// Operator: the owner's own turn (a oneshot, from the command line), which may search every
	// scope and save personal data agent-wide.
	Operator bool
	// Revision, when set, is the revision of the record an approval card showed (RecordRevision):
	// a change by id to a record that is no longer that one is refused (ErrRevision).
	Revision string
}

// Errors of the scopes.
var (
	ErrAllScopes  = errors.New(`every scope (audience "all") is only for the owner's own runs (a oneshot): a conversation or a job sees its own records and the agent-wide ones`)
	ErrAgentWide  = errors.New("an agent-wide record is recalled in every conversation, so it may hold no personal data (an email, a phone, a card) and may not be about a person (kind user): save it here, in the conversation it came from, or without the personal data")
	ErrNoAudience = errors.New("this conversation's audience is not known to the runtime yet (a DM whose person it has not seen): nothing can be saved here; try again after the next message, or save it agent-wide without personal data")
)

// AudienceOf is session's audience, as Where says (nil: none, the agent-wide records only).
func (s *Store) AudienceOf(session string) Audience {
	if s.Where == nil || session == "" {
		return Audience{}
	}
	return s.Where(session)
}

// agentWide: a record of kind, personal or not, may be everyone's in a's turn. Personal data and
// a person's profile never are, but for the owner's own runs: an agent-wide record is recalled
// in every conversation.
func agentWide(a Audience, kind string, personal bool) bool {
	return a.Operator || !personal && kind != KindUser
}

// saveScope is the scope a save asks for (here, the default, or agent) as a's turn makes it.
// Here is the conversation's scope; a turn with no conversation (a job, a script, the API's
// runs) has only the agent-wide one, which personal text may not take (ErrAgentWide): a hot save
// of it gets no scope instead (hot, it is everyone's anyway; unpinned, its src's rule decides).
func saveScope(want string, a Audience, kind string, personal, hot bool) (string, error) {
	scope := ""
	switch strings.TrimSpace(want) {
	case "", "here":
		switch {
		case a.Scope != "":
			return a.Scope, nil
		case a.Conv != "":
			return "", ErrNoAudience
		case hot && !agentWide(a, kind, personal):
			return "", nil
		}
		scope = ScopeAgent
	case ScopeAgent:
		scope = ScopeAgent
	default:
		return "", fmt.Errorf(`scope %q: "here" (this conversation, the default) or "agent" (everyone)`, want)
	}
	if !agentWide(a, kind, personal) {
		return "", ErrAgentWide
	}
	return scope, nil
}

// scopeOf is r's scope and conversation: its own scope, or for a record saved before scopes
// existed (no scope key) the one its src gives. A src naming a Slack DM or thread
// (chat:<session>) is that conversation's (a DM's record is seen in that DM, a thread's in its
// channel); any other (a job's, a script's, the API's, boot, migration) is agent-wide when it
// holds no personal data and is not about a person, else nobody's (only an owner's search of
// every scope finds it). The caller holds the lock.
func (s *Store) scopeOf(r *Record) (scope, conv string) {
	if r.Scope != "" {
		return r.Scope, ""
	}
	if session, ok := strings.CutPrefix(r.Src, "chat:"); ok && session != "" && s.Where != nil {
		a, seen := s.srcCache[session]
		if !seen {
			a = s.Where(session)
			if s.srcCache == nil {
				s.srcCache = map[string]Audience{}
			}
			s.srcCache[session] = a
		}
		if a.Conv != "" {
			return a.Scope, a.Conv
		}
	}
	if !r.personal() && r.Kind != KindUser {
		return ScopeAgent, ""
	}
	return "", ""
}

// personal: r's header flags personal data, or its text has some (a header a hand edit or an
// older runtime wrote without the flags). Computed once per text.
func (r *Record) personal() bool {
	if len(r.PII) > 0 {
		return true
	}
	if r.piiText != r.Text || !r.piiDone {
		r.piiText, r.piiDone, r.piiAny = r.Text, true, len(PII(r.Text)) > 0
	}
	return r.piiAny
}

// visible: a may be shown r. all is an owner's search of every scope (checked by the caller).
func (s *Store) visible(r *Record, a Audience, all bool) bool {
	if all || r.Hot > 0 {
		return true
	}
	scope, conv := s.scopeOf(r)
	return scope == ScopeAgent || scope != "" && scope == a.Scope || conv != "" && conv == a.Conv
}
