package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Channel turns (docs/needletail.md §4.12a). The core runs the turns of its own channel's
// conversations (WhatsApp: the person's message, and the outcome of a request sent from it) through
// these routes, signed with the agent's core key. A turn is admitted once per execution, durably,
// before it runs: a repeated request answers the same turn, never a second one, even after the core
// took its result (a tombstone stays); every tool call it makes is recorded before it runs; its
// result is published only once it is on disk, and kept until the core takes it.

const (
	channelFile    = "channel-turns.json"
	channelLock    = "channel-turns.lock"
	channelVersion = 1
	// channelKeep is how long an untaken result is kept; channelTombstone how long a taken (or
	// expired) execution is remembered so it never runs again. The core never reuses an execution.
	channelKeep      = 7 * 24 * time.Hour
	channelTombstone = 30 * 24 * time.Hour
	// maxChannelWait is the longest a GET waits for a turn to end, in seconds.
	maxChannelWait = 60
	// channelSaveGap: a result that could not be written is tried again this often (doubling, at
	// most 30 s), the conversation held and the turn unpublished (running) until it is on disk.
	channelSaveGap = 2 * time.Second
)

var execIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type channelEntry struct {
	wire.ChannelTurnStatus
	Hash    string    `json:"hash"`
	Root    string    `json:"root"`
	At      time.Time `json:"at"`
	Updated time.Time `json:"updated"`
}

type channelFileV1 struct {
	V     int                     `json:"v"`
	Turns map[string]channelEntry `json:"turns"`
	// Nonces are the POST signatures' nonces used, kept until their signature expires.
	Nonces map[string]time.Time `json:"nonces,omitempty"`
}

// channelStore is state/needletail/channel-turns.json (Raw: results are client data): the channel
// turns admitted, by execution, their results, and the nonces used. Its process holds
// channel-turns.lock (an exclusive flock) for as long as it runs: a second runtime on the home
// refuses to start rather than admit turns beside it.
type channelStore struct {
	mu      sync.Mutex
	path    string
	tmp     string
	lock    *os.File
	now     func() time.Time
	turns   map[string]channelEntry
	nonces  map[string]time.Time // POSTs': persisted
	seen    map[string]time.Time // GETs' and DELETEs' (idempotent): in memory
	changed chan struct{}        // closed and replaced on every published change
}

func loadChannel(dir, tmp string, now func() time.Time) (*channelStore, error) {
	if now == nil {
		now = time.Now
	}
	s := &channelStore{now: now, tmp: tmp, turns: map[string]channelEntry{}, nonces: map[string]time.Time{}, seen: map[string]time.Time{},
		changed: make(chan struct{})}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, channelLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("%s: another runtime holds it (%v): two would admit channel turns beside each other", channelLock, err)
	}
	s.lock = lf
	s.path = filepath.Join(dir, channelFile)
	raw, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		return s, nil
	case err != nil:
		s.close()
		return nil, err
	}
	var f channelFileV1
	if err := json.Unmarshal(raw, &f); err != nil || f.V != channelVersion {
		// never start empty over admissions it cannot read: a turn could run twice
		s.close()
		return nil, fmt.Errorf("%s: unreadable or version %d (this runtime reads %d)", s.path, f.V, channelVersion)
	}
	if f.Turns != nil {
		s.turns = f.Turns
	}
	if f.Nonces != nil {
		s.nonces = f.Nonces
	}
	at := now().UTC()
	for k, e := range s.turns {
		if !e.Terminal() { // a turn never resumes after a restart; what it did (Acted) stays
			e.State, e.Error, e.Updated = wire.ChannelInterrupted, "the runtime restarted during the turn", at
			s.turns[k] = e
		}
	}
	s.prune()
	if err := s.write(s.turns, s.nonces); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *channelStore) close() {
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

// prune turns untaken results past channelKeep into tombstones and forgets tombstones and nonces
// past theirs; the caller holds mu (or owns s alone).
func (s *channelStore) prune() {
	now := s.now()
	for k, e := range s.turns {
		switch {
		case e.Taken && e.Updated.Before(now.Add(-channelTombstone)):
			delete(s.turns, k)
		case !e.Taken && e.Terminal() && e.Updated.Before(now.Add(-channelKeep)):
			s.turns[k] = tombstone(e, now)
		}
	}
	for n, until := range s.nonces {
		if now.After(until) {
			delete(s.nonces, n)
		}
	}
	for n, until := range s.seen {
		if now.After(until) {
			delete(s.seen, n)
		}
	}
}

// tombstone is e with its result forgotten: the execution, its request's hash and how it ended.
func tombstone(e channelEntry, now time.Time) channelEntry {
	st := wire.ChannelTurnStatus{Execution: e.Execution, State: e.State, Acted: e.Acted, Taken: true}
	return channelEntry{ChannelTurnStatus: st, Hash: e.Hash, Root: e.Root, At: e.At, Updated: now.UTC()}
}

// write puts turns and nonces on disk; the caller holds mu (or owns s alone). The in-memory state
// changes only after a write succeeded.
func (s *channelStore) write(turns map[string]channelEntry, nonces map[string]time.Time) error {
	if s.path == "" {
		return nil
	}
	body, err := json.Marshal(channelFileV1{V: channelVersion, Turns: turns, Nonces: nonces})
	if err != nil {
		return err
	}
	tmp := s.tmp
	if tmp == "" {
		tmp = filepath.Dir(s.path)
	}
	return fsx.WriteAtomicIn(s.path, tmp, append(body, '\n'), 0o600)
}

// commit writes turns with e set (and nonce used until its expiry, when given) and, once on disk,
// makes it the state and wakes the waiting GETs; the caller holds mu.
func (s *channelStore) commit(e channelEntry, nonce string, until time.Time) error {
	turns := maps.Clone(s.turns)
	turns[e.Execution] = e
	nonces := s.nonces
	if nonce != "" {
		nonces = maps.Clone(s.nonces)
		nonces[nonce] = until
	}
	if err := s.write(turns, nonces); err != nil {
		return err
	}
	s.turns, s.nonces = turns, nonces
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

// channelPath reports whether id, or its conversation's root, is one of the core's channel
// conversations (wire.ChannelPrefixes, case-folded).
func channelPath(store *sessions.Store, id string) bool {
	is := func(v string) bool {
		low := strings.ToLower(v)
		for _, p := range wire.ChannelPrefixes {
			if strings.HasPrefix(low, p) {
				return true
			}
		}
		return false
	}
	if is(id) {
		return true
	}
	root, ok := store.Root(id)
	return ok && is(root)
}

// reserved is Reserved, plus the core's channel conversations once enforcement is on.
func (s *Server) reserved(id string) bool {
	return Reserved(s.store, id) || s.enforce && channelPath(s.store, id)
}

// capabilities answers what the runtime offers the core.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	c := wire.Capabilities{Enforcing: s.enforce}
	if len(s.channelKey) >= MinKey && s.channel.path != "" {
		c.ChannelTurns = wire.ChannelTurnsV1
	}
	writeJSON(w, http.StatusOK, c)
}

// signed checks the core's signature over the request (its body read here): the body, the nonce
// and how long it stays valid (the signed time plus the skew), or a status and message to answer.
// Idempotent requests (GET, DELETE) are checked against the nonces seen since this start; a POST's
// nonce is checked against the persisted ones by its caller, under the admission's lock.
func (s *Server) signed(r *http.Request) (body []byte, nonce string, until time.Time, status int, msg string) {
	if len(s.channelKey) < MinKey {
		return nil, "", time.Time{}, http.StatusNotImplemented, "channel turns are not configured (no core key)"
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", time.Time{}, http.StatusBadRequest, "unreadable body"
	}
	// the path signed is the request URI: the query (a GET's wait) is part of what was signed
	nonce, at, err := wire.VerifyChannel(s.channelKey, r.Header.Get(wire.ChannelSignatureHeader), r.Method, r.URL.RequestURI(), body, s.now())
	if err != nil {
		return nil, "", time.Time{}, http.StatusUnauthorized, err.Error()
	}
	// kept a second past the window the signature is valid in: never replayable at its edge
	until = at.Add(wire.ChannelSkew + time.Second)
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	if _, used := c.nonces[nonce]; used {
		return nil, "", time.Time{}, http.StatusUnauthorized, "channel signature replayed"
	}
	if r.Method != http.MethodPost {
		if _, used := c.seen[nonce]; used {
			return nil, "", time.Time{}, http.StatusUnauthorized, "channel signature replayed"
		}
		c.seen[nonce] = until
	}
	return body, nonce, until, 0, ""
}

// channelStart admits a channel turn and starts it: 202 with its status. Admission is one critical
// section: a repeat of the same execution answers its status (taken or not), the same execution
// with another request is refused, and a busy conversation admits nothing.
func (s *Server) channelStart(w http.ResponseWriter, r *http.Request) {
	body, nonce, until, status, msg := s.signed(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req wire.ChannelTurn
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not a channel turn")
		return
	}
	switch {
	case !execIDRe.MatchString(req.Execution):
		writeError(w, http.StatusBadRequest, "execution is 8 to 64 letters, digits, _ or -")
		return
	case req.Kind != messaging.ExecInbound && req.Kind != messaging.ExecReturn:
		writeError(w, http.StatusBadRequest, `kind is "inbound" or "return"`)
		return
	case !sessions.ValidID(req.Session) || !channelPath(s.store, req.Session) || Reserved(s.store, req.Session):
		writeError(w, http.StatusForbidden, "session is not a conversation of the core's channels")
		return
	case strings.TrimSpace(req.Message) == "" && len(req.Files) == 0:
		writeError(w, http.StatusBadRequest, "message is empty")
		return
	case len(req.Files) > wire.MaxChatFiles:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files per message", wire.MaxChatFiles))
		return
	case len(req.Files) > 0 && s.files == nil:
		writeError(w, http.StatusBadRequest, "this agent keeps no files")
		return
	case req.AudioFormat != "" && req.AudioFormat != "mp3" && req.AudioFormat != "opus":
		writeError(w, http.StatusBadRequest, "audio_format is mp3 or opus")
		return
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, used := c.nonces[nonce]; used {
		writeError(w, http.StatusUnauthorized, "channel signature replayed")
		return
	}
	if e, ok := c.turns[req.Execution]; ok {
		if e.Hash != hash {
			writeError(w, http.StatusConflict, "this execution was admitted with another request")
			return
		}
		writeJSON(w, http.StatusAccepted, e.ChannelTurnStatus)
		return
	}
	root, err := s.ensure(req.Session, wire.Session{Title: req.Title, Source: SourceAPI})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the session could not be made")
		return
	}
	h, _ := s.store.Get(root)
	if err := s.acquire(root, h.Source, false); err != nil {
		writeError(w, busyStatus(err), err.Error()) // nothing admitted: the core tries again
		return
	}
	now := s.now().UTC()
	e := channelEntry{ChannelTurnStatus: wire.ChannelTurnStatus{Execution: req.Execution, State: wire.ChannelRunning},
		Hash: hash, Root: root, At: now, Updated: now}
	if err := c.commit(e, nonce, until); err != nil {
		s.release(root, false)
		s.logf("channel turn %s: not admitted: %v", req.Execution, err)
		writeError(w, http.StatusInternalServerError, "the turn could not be admitted")
		return
	}
	go s.channelRun(req, root)
	writeJSON(w, http.StatusAccepted, e.ChannelTurnStatus)
}

// channelRun runs an admitted turn and keeps its result; the conversation's slot is held until the
// result is on disk (its voice note taken, its reply spoken), so nothing else runs there first.
func (s *Server) channelRun(req wire.ChannelTurn, root string) {
	defer s.release(root, false)
	c := s.channel
	exec := &messaging.Execution{ID: req.Execution, Kind: req.Kind, Token: req.Token}
	exec.OnAct = func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		e := c.turns[req.Execution]
		e.Acted++
		e.Updated = s.now().UTC()
		return c.commit(e, "", time.Time{})
	}
	message, opts := req.Message, loop.Options{}
	if req.Kind == messaging.ExecReturn {
		opts.InputKind = wire.KindReturn
	}
	if len(req.Files) > 0 {
		block, images := s.takeFiles(req.Session, root, req.Files)
		message, opts.Images = strings.TrimSpace(message+"\n\n"+block), images
	}
	res, err := s.run(messaging.WithExecution(s.base, exec), req.Session, message, opts)
	q, voiced := s.voice.Take(root) // taken whatever the turn did: never left for the next
	out := wire.ChannelTurnStatus{Execution: req.Execution, Acted: exec.Acted()}
	switch {
	case err != nil:
		out.State, out.Error = wire.ChannelFailed, s.redact("turn failed: "+err.Error())
	default:
		out.State, out.Session, out.Text = wire.ChannelOK, res.Session, res.Text
		out.Marked = s.store.Untrusted(res.Session)
		// a return's update is text only; a person's message may be answered with a voice note,
		// spoken without the reply's control marker
		if spoken := withoutMarker(res.Text); req.Kind == messaging.ExecInbound && voiced && s.speaker != nil && strings.TrimSpace(spoken) != "" && spoken != loop.NoAnswer {
			out.Audio = s.speak(spoken, req.AudioFormat, q)
		}
	}
	gap := channelSaveGap
	for try := 1; ; try++ {
		c.mu.Lock()
		e := c.turns[req.Execution]
		e.ChannelTurnStatus, e.Updated = out, s.now().UTC()
		err := c.commit(e, "", time.Time{})
		c.mu.Unlock()
		if err == nil {
			return
		}
		// unpublished (still running) until it is on disk; never run again. Only a shutdown stops
		// trying: the next start reads it as interrupted, with what it did
		s.logf("channel turn %s: its result could not be kept (try %d): %v", req.Execution, try, err)
		select {
		case <-s.base.Done():
			return
		case <-time.After(gap):
		}
		gap = min(2*gap, 30*time.Second)
	}
}

// markers are the control lines a channel turn's reply may end with (the core strips them).
var markers = []string{"[DONE]", "[UPDATE]", "[SILENT]"}

// withoutMarker is text without a trailing control marker line.
func withoutMarker(text string) string {
	t := strings.TrimRight(text, " \t\r\n")
	for _, m := range markers {
		if strings.HasSuffix(t, m) {
			return strings.TrimRight(strings.TrimSuffix(t, m), " \t\r\n")
		}
	}
	return text
}

// channelGet answers a channel turn's status, waiting up to ?wait= seconds (at most 60) for it to end.
func (s *Server) channelGet(w http.ResponseWriter, r *http.Request) {
	if _, _, _, status, msg := s.signed(r); status != 0 {
		writeError(w, status, msg)
		return
	}
	id := r.PathValue("exec")
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), maxChannelWait) // clamped before it becomes a duration
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	c := s.channel
	for {
		c.mu.Lock()
		e, ok := c.turns[id]
		changed := c.changed
		c.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "no such channel turn")
			return
		}
		left := time.Until(deadline)
		if e.Terminal() || left <= 0 {
			writeJSON(w, http.StatusOK, e.ChannelTurnStatus)
			return
		}
		select {
		case <-changed:
		case <-time.After(left):
		case <-r.Context().Done():
			return
		}
	}
}

// channelTake is the core having kept a turn's result: the result is forgotten, the execution kept
// as a tombstone (it can never run again). A turn still running stays.
func (s *Server) channelTake(w http.ResponseWriter, r *http.Request) {
	if _, _, _, status, msg := s.signed(r); status != 0 {
		writeError(w, status, msg)
		return
	}
	id := r.PathValue("exec")
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.turns[id]
	switch {
	case !ok || e.Taken:
		writeJSON(w, http.StatusOK, map[string]any{"taken": id})
	case !e.Terminal():
		writeError(w, http.StatusConflict, "the turn is still running")
	default:
		if err := c.commit(tombstone(e, s.now()), "", time.Time{}); err != nil {
			s.logf("channel turn %s: not taken: %v", id, err)
			writeError(w, http.StatusInternalServerError, "the result could not be forgotten")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"taken": id})
	}
}

// ChannelTurn is a channel turn's status (tests and the CLI).
func (s *Server) ChannelTurn(exec string) (wire.ChannelTurnStatus, bool) {
	s.channel.mu.Lock()
	defer s.channel.mu.Unlock()
	e, ok := s.channel.turns[exec]
	if !ok {
		return wire.ChannelTurnStatus{}, false
	}
	return e.ChannelTurnStatus, true
}

// closeChannel releases the channel-turns lock: only once no turn is left to write (Shutdown,
// when every turn stopped; otherwise the process's exit releases it).
func (s *Server) closeChannel() { s.channel.mu.Lock(); s.channel.close(); s.channel.mu.Unlock() }
