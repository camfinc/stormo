// Package commands is the agent's chat commands (docs/needletail.md §4.9, §4.14): /new, /status,
// /memory, /jobs, /stop, /trust and /help, as Slack's slash commands and Telegram's bot commands
// run them. A gateway reads who called, where (the conversation, by the runtime's own records),
// whether the caller is verified and an approver; Run decides what the caller may do and answers
// with text for the caller alone (Slack: ephemeral; Telegram: in the chat it was asked in). The
// commands never start a model turn and never post anywhere else.
package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The commands' keys: their names on Telegram, and on Slack unless renamed (SlackNames).
const (
	New    = "new"
	Status = "status"
	Memory = "memory"
	Jobs   = "jobs"
	Stop   = "stop"
	Trust  = "trust"
	Help   = "help"
)

// Def is one command as a platform's command list shows it.
type Def struct {
	Key, Description, Usage string
}

// Defs are the commands, in the order help lists them.
var Defs = []Def{
	{New, "Start a fresh conversation in this DM (the earlier one is archived)", ""},
	{Status, "The agent's status: model, jobs, approvals, this conversation", ""},
	{Memory, "Search what the agent remembers that you may see", "[words]"},
	{Jobs, "The agent's scheduled jobs", ""},
	{Stop, "Stop the agent's turn running in this conversation", ""},
	{Trust, "Approvers: endorse this conversation (its marked messages no longer hold the agent back)", ""},
	{Help, "The commands", ""},
}

// Keys are the commands' keys.
func Keys() []string {
	out := make([]string, len(Defs))
	for i, d := range Defs {
		out[i] = d.Key
	}
	return out
}

// Def of a key; false for none.
func DefOf(key string) (Def, bool) {
	i := slices.IndexFunc(Defs, func(d Def) bool { return d.Key == key })
	if i < 0 {
		return Def{}, false
	}
	return Defs[i], true
}

// Slack's command names: lowercase letters, digits, - and _, at most 32 characters, none of
// Slack's built-in commands (an app cannot register those: https://slack.com/help/articles/201259356).
var (
	slackNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	slackReserved = map[string]bool{"me": true, "status": true, "away": true, "dnd": true, "shrug": true, "remind": true, "msg": true,
		"feed": true, "who": true, "collapse": true, "expand": true, "leave": true, "join": true, "open": true, "search": true,
		"topic": true, "mute": true, "pro": true, "shortcuts": true, "invite": true, "kick": true, "remove": true, "archive": true,
		"apps": true, "dm": true}
)

// CheckSlackName refuses a name Slack would not register for an app.
func CheckSlackName(name string) error {
	switch {
	case !slackNameRe.MatchString(name):
		return fmt.Errorf("slack command name %q: lowercase letters, digits, - and _, at most 32 characters", name)
	case slackReserved[name]:
		return fmt.Errorf("slack command name %q is one of Slack's own commands", name)
	}
	return nil
}

// SlackNames are the commands' Slack names (key → name, without the slash): prefix + key, or
// prefix + "agent-" + key when that is one of Slack's own commands (status: /agent-status
// without a prefix), unless names renames it. One Slack app per workspace may own a name, so
// agents whose apps share a workspace give their commands a prefix. Every name is checked
// (CheckSlackName) and none may repeat.
func SlackNames(prefix string, names map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k := range names {
		if _, ok := DefOf(k); !ok {
			return nil, fmt.Errorf("slack.commands.names: %q is not a command (%s)", k, strings.Join(Keys(), ", "))
		}
	}
	seen := map[string]string{}
	for _, d := range Defs {
		name, set := names[d.Key]
		if !set {
			name = prefix + d.Key
			if slackReserved[name] {
				name = prefix + "agent-" + d.Key
			}
		}
		name = strings.TrimPrefix(name, "/")
		if err := CheckSlackName(name); err != nil {
			return nil, err
		}
		if other, dup := seen[name]; dup {
			return nil, fmt.Errorf("slack command name %q is given to both %s and %s", name, other, d.Key)
		}
		seen[name], out[d.Key] = d.Key, name
	}
	return out, nil
}

// Call is one command as a gateway heard it: who, where, with what. The gateway fills it from the
// runtime's records (the platform's authenticated user id, the conversation it keys), never from
// the command's text.
type Call struct {
	Key  string
	Args string
	// Platform is slack or telegram; User the caller's id there.
	Platform, User string
	// Conversation is the conversation the command applies to (a DM's, a thread's, a Telegram
	// chat's session id); "" where there is none (a Slack channel outside a thread).
	Conversation string
	// Verified: the caller may instruct the agent (allowed_users, or a full member of the agent's
	// workspace under allow_all_users). Approver: approvals.approvers names them.
	Verified, Approver bool
	// Names are the commands' names on this platform (key → "/name"), for help and hints.
	Names map[string]string
}

// State is what /status shows.
type State struct {
	Agent, Model string
	// Jobs: how many, enabled, running now (their names).
	Jobs, Enabled int
	Running       []string
	// Approvals pending in all, and in this conversation.
	Approvals, ApprovalsHere int
	// Turn: a turn runs in this conversation now; Untrusted: it is marked, Taint why (and who).
	Turn      bool
	Untrusted bool
	Taint     []Taint
	// Platforms are the gateways' states (slack: connected, …).
	Platforms map[string]string
}

// Taint is one source of a conversation's mark, as /status shows it.
type Taint struct {
	Why   string
	Users []string
}

// Record is one memory record /memory shows.
type Record struct {
	ID, Kind, Scope, Text string
	// Marked: saved from words the agent may not obey (information, never instructions).
	Marked bool
}

// Job is one scheduled job /jobs shows.
type Job struct {
	Name, Schedule, Next, Last string
	Enabled, Running           bool
}

// Backend is what the commands act on (the runtime's).
type Backend struct {
	// New continues the conversation in a fresh session (sessions.Store.Restart), serialized
	// with its turns: ErrBusy while one runs.
	New func(ctx context.Context, conversation, by string) error
	// Stop cancels the turn running in the conversation: false when none runs.
	Stop func(conversation string) bool
	// Trust endorses the conversation as the API's trust does (audited), serialized with its
	// turns: ErrBusy while one runs.
	Trust func(ctx context.Context, conversation, by string) error
	// Status is the agent's state and the conversation's ("" for none).
	Status func(conversation string) State
	// Search finds the memory records the conversation's audience may see; Hot is the hot memory
	// (MEMORY.md, USER.md entries).
	Search func(conversation, query string) ([]Record, int, error)
	Hot    func() ([]string, error)
	// Jobs are the scheduled jobs.
	Jobs func() ([]Job, error)
}

// ErrBusy is a command that waits for no turn: one runs in the conversation.
var ErrBusy = errors.New("a turn is running in this conversation")

// Run runs c on b and answers the caller (Markdown). Every command but /help needs a verified
// caller; /trust an approver who brought none of the conversation's taint in.
func Run(ctx context.Context, b Backend, c Call) string {
	if c.Key != Help && !c.Verified {
		return "You may talk to me, but not run my commands (you are not on my verified list). " + c.name(Help) + " lists them."
	}
	switch c.Key {
	case Help:
		return help(c)
	case New:
		return c.newConversation(ctx, b)
	case Stop:
		if c.Conversation == "" {
			return c.noConversation()
		}
		if b.Stop != nil && b.Stop(c.Conversation) {
			return "Stopped the turn running here."
		}
		return "No turn is running here."
	case Trust:
		return c.trust(ctx, b)
	case Status:
		return c.status(b)
	case Memory:
		return c.memory(b)
	case Jobs:
		return c.jobs(b)
	}
	return "Unknown command. " + c.name(Help) + " lists mine."
}

// Unknown is the answer to a command this agent does not serve (another app's, an old one).
func Unknown(name string, names map[string]string) string {
	h := "/help"
	if n := names[Help]; n != "" {
		h = n
	}
	return fmt.Sprintf("`%s` is not one of my commands. %s lists them.", oneLine(name, 40), h)
}

func (c Call) name(key string) string {
	if n := c.Names[key]; n != "" {
		return n
	}
	return "/" + key
}

func (c Call) noConversation() string {
	if c.Platform == "slack" {
		return "There is no conversation to apply this to here: in a channel each thread is its own conversation. Use it in a DM with me."
	}
	return "There is no conversation to apply this to here."
}

func help(c Call) string {
	lines := []string{"My commands (only you see the answers):"}
	if c.Platform != "slack" {
		lines[0] = "My commands:"
	}
	for _, d := range Defs {
		usage := ""
		if d.Usage != "" {
			usage = " " + d.Usage
		}
		lines = append(lines, fmt.Sprintf("• %s%s: %s", c.name(d.Key), usage, d.Description))
	}
	return strings.Join(lines, "\n")
}

func (c Call) newConversation(ctx context.Context, b Backend) string {
	if c.Conversation == "" {
		return c.noConversation()
	}
	if b.New == nil {
		return "Not available here."
	}
	switch err := b.New(ctx, c.Conversation, c.Platform+":"+c.User); {
	case errors.Is(err, ErrBusy):
		return "I am answering here right now: " + c.name(Stop) + " stops it, or try again when I am done."
	case err != nil:
		return "Could not start a fresh conversation: " + oneLine(err.Error(), 200)
	}
	return "Started a fresh conversation: I no longer have our earlier messages in mind (they are archived, not deleted). What I saved to memory stays."
}

func (c Call) trust(ctx context.Context, b Backend) string {
	switch {
	case c.Conversation == "":
		return c.noConversation()
	case !c.Approver:
		return "Only an approver may endorse a conversation."
	case b.Trust == nil:
		return "Not available here."
	}
	if b.Status != nil {
		st := b.Status(c.Conversation)
		if !st.Untrusted {
			return "This conversation is not marked: there is nothing to endorse."
		}
		for _, t := range st.Taint {
			if slices.Contains(t.Users, c.User) {
				return "You brought some of this conversation's marked messages in, so you may not endorse it: another approver may."
			}
		}
	}
	switch err := b.Trust(ctx, c.Conversation, c.Platform+":"+c.User); {
	case errors.Is(err, ErrBusy):
		return "I am answering here right now: try again when I am done."
	case err != nil:
		return "Could not endorse it: " + oneLine(err.Error(), 200)
	}
	return "Endorsed: this conversation's messages so far no longer hold me back (recorded). Messages from unverified people after this mark it again."
}

func (c Call) status(b Backend) string {
	if b.Status == nil {
		return "Not available here."
	}
	st := b.Status(c.Conversation)
	lines := []string{}
	head := "Status"
	if st.Agent != "" {
		head = st.Agent + ": status"
	}
	lines = append(lines, "**"+head+"**")
	if st.Model != "" {
		lines = append(lines, "• Model: "+st.Model)
	}
	jobs := fmt.Sprintf("• Jobs: %d (%d enabled)", st.Jobs, st.Enabled)
	if len(st.Running) > 0 {
		jobs += "; running now: " + strings.Join(st.Running, ", ")
	}
	lines = append(lines, jobs)
	lines = append(lines, fmt.Sprintf("• Approvals waiting: %d (%d from this conversation)", st.Approvals, st.ApprovalsHere))
	for _, p := range sortedKeys(st.Platforms) {
		lines = append(lines, "• "+p+": "+st.Platforms[p])
	}
	if c.Conversation == "" {
		return strings.Join(lines, "\n")
	}
	if st.Turn {
		lines = append(lines, "• Here: I am answering now ("+c.name(Stop)+" stops it)")
	}
	if !st.Untrusted {
		lines = append(lines, "• This conversation: trusted")
		return strings.Join(lines, "\n")
	}
	why := []string{}
	for _, t := range st.Taint {
		w := oneLine(t.Why, 120)
		if len(t.Users) > 0 {
			w += " (" + strings.Join(t.Users, ", ") + ")"
		}
		why = append(why, w)
	}
	line := "• This conversation: marked (it took in words I may not obey: sends elsewhere, skills, jobs and memory changes wait for an approver)"
	if len(why) > 0 {
		line += ": " + strings.Join(why, "; ")
	}
	lines = append(lines, line, "  "+c.name(New)+" starts a fresh one; an approver's "+c.name(Trust)+" endorses this one.")
	return strings.Join(lines, "\n")
}

func (c Call) memory(b Backend) string {
	q := strings.TrimSpace(c.Args)
	if q == "" {
		if !c.Approver || b.Hot == nil {
			return "Give words to search for: " + c.name(Memory) + " <words> searches what I remember that you may see here."
		}
		hot, err := b.Hot()
		if err != nil {
			return "Could not read my memory: " + oneLine(err.Error(), 200)
		}
		if len(hot) == 0 {
			return "My hot memory is empty. " + c.name(Memory) + " <words> searches the archive."
		}
		lines := []string{"**Hot memory** (in every conversation's context):"}
		for _, e := range hot {
			lines = append(lines, "• "+oneLine(e, 300))
		}
		return strings.Join(lines, "\n")
	}
	if b.Search == nil {
		return "Not available here."
	}
	recs, total, err := b.Search(c.Conversation, q)
	if err != nil {
		return "Could not search my memory: " + oneLine(err.Error(), 200)
	}
	if len(recs) == 0 {
		return "Nothing found for that among what you may see here."
	}
	lines := []string{fmt.Sprintf("**Memory** (%d of %d found, what you may see here):", len(recs), total)}
	for _, r := range recs {
		tag := r.Kind
		if r.Scope != "" {
			tag += ", " + r.Scope
		}
		mark := ""
		if r.Marked {
			mark = " [marked: saved from words I may not obey]"
		}
		lines = append(lines, fmt.Sprintf("• `%s` (%s)%s: %s", r.ID, tag, mark, oneLine(r.Text, 300)))
	}
	return strings.Join(lines, "\n")
}

func (c Call) jobs(b Backend) string {
	if b.Jobs == nil {
		return "Not available here."
	}
	jobs, err := b.Jobs()
	if err != nil {
		return "Could not read my jobs: " + oneLine(err.Error(), 200)
	}
	if len(jobs) == 0 {
		return "I have no scheduled jobs."
	}
	lines := []string{fmt.Sprintf("**Jobs** (%d):", len(jobs))}
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "paused"
		}
		if j.Running {
			state += ", running now"
		}
		l := fmt.Sprintf("• %s: %s (%s)", oneLine(j.Name, 80), oneLine(orDefault(j.Schedule, "?"), 60), state)
		if j.Next != "" && j.Enabled {
			l += "; next " + j.Next
		}
		if j.Last != "" {
			l += "; last " + j.Last
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// When is a time as /jobs shows it, on the agent's wall clock.
func When(stamp string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	if loc != nil {
		t = t.In(loc)
	}
	return t.Format("Mon 2 Jan 15:04 MST")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// oneLine is s on one line, cut to n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
