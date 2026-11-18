package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Channel turns (docs/needletail.md, channel turns). The core runs the turns of its own channel's
// conversations (WhatsApp: the person's message, and the outcome of a request sent from it) through
// these routes, signed with the agent's core key. A turn is admitted once per execution, durably,
// before it runs: a repeated request answers the same turn, never a second one, and its result is
// kept until the core takes it, so a core that timed out or restarted still gets what the turn did.

const (
	channelFile    = "channel-turns.json"
	channelVersion = 1
	// channelKeep is how long an untaken result is kept.
	channelKeep = 7 * 24 * time.Hour
	// maxChannelWait is the longest a GET waits for a turn to end.
	maxChannelWait = 60 * time.Second
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
	V     int                      `json:"v"`
	Turns map[string]*channelEntry `json:"turns"`
}

// channelStore is state/needletail/channel-turns.json (Raw: results are client data): the channel
// turns admitted, by execution, and their results. Only serve writes it.
type channelStore struct {
	mu      sync.Mutex
	path    string
	tmp     string
	now     func() time.Time
	turns   map[string]*channelEntry
	nonces  map[string]time.Time
	changed chan struct{} // closed and replaced on every change
}

func loadChannel(dir, tmp string, now func() time.Time) (*channelStore, error) {
	if now == nil {
		now = time.Now
	}
	s := &channelStore{now: now, tmp: tmp, turns: map[string]*channelEntry{}, nonces: map[string]time.Time{}, changed: make(chan struct{})}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, channelFile)
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f channelFileV1
	if err := json.Unmarshal(raw, &f); err != nil || f.V != channelVersion {
		// never start empty over admissions it cannot read: a turn could run twice
		return nil, fmt.Errorf("%s: unreadable or version %d (this runtime reads %d)", s.path, f.V, channelVersion)
	}
	if f.Turns != nil {
		s.turns = f.Turns
	}
	at := now().UTC()
	for _, e := range s.turns {
		if !e.Terminal() { // a turn never resumes after a restart
			e.State, e.Error, e.Updated = wire.ChannelInterrupted, "the runtime restarted during the turn", at
		}
	}
	s.prune()
	return s, s.save()
}

func (s *channelStore) prune() {
	cut := s.now().Add(-channelKeep)
	for k, e := range s.turns {
		if e.Updated.Before(cut) {
			delete(s.turns, k)
		}
	}
	ncut := s.now().Add(-wire.ChannelSkew)
	for n, at := range s.nonces {
		if at.Before(ncut) {
			delete(s.nonces, n)
		}
	}
}

// save writes the file; the caller holds mu.
func (s *channelStore) save() error {
	if s.path == "" {
		return nil
	}
	body, err := json.Marshal(channelFileV1{V: channelVersion, Turns: s.turns})
	if err != nil {
		return err
	}
	tmp := s.tmp
	if tmp == "" {
		tmp = filepath.Dir(s.path)
	}
	return fsx.WriteAtomicIn(s.path, tmp, append(body, '\n'), 0o600)
}

// notify wakes the GETs waiting for a change; the caller holds mu.
func (s *channelStore) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
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
	if len(s.channelKey) >= MinKey {
		c.ChannelTurns = wire.ChannelTurnsV1
	}
	writeJSON(w, http.StatusOK, c)
}

// signed checks the core's signature over the request (its body read here) and that its nonce is
// new: the body, or a status and message to answer.
func (s *Server) signed(r *http.Request) ([]byte, int, string) {
	if len(s.channelKey) < MinKey {
		return nil, http.StatusNotImplemented, "channel turns are not configured (no core key)"
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, http.StatusBadRequest, "unreadable body"
	}
	// the path signed is the request URI: the query (a GET's wait) is part of what was signed
	nonce, err := wire.VerifyChannel(s.channelKey, r.Header.Get(wire.ChannelSignatureHeader), r.Method, r.URL.RequestURI(), body, s.now())
	if err != nil {
		return nil, http.StatusUnauthorized, err.Error()
	}
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	if _, seen := c.nonces[nonce]; seen {
		return nil, http.StatusUnauthorized, "channel signature replayed"
	}
	c.nonces[nonce] = s.now()
	return body, 0, ""
}

// channelStart admits a channel turn and starts it: 202 with its status. A repeat of the same
// execution answers its status (a different request under the same execution is refused).
func (s *Server) channelStart(w http.ResponseWriter, r *http.Request) {
	body, status, msg := s.signed(r)
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
	if e, ok := c.turns[req.Execution]; ok {
		c.mu.Unlock()
		if e.Hash != hash {
			writeError(w, http.StatusConflict, "this execution was admitted with another request")
			return
		}
		writeJSON(w, http.StatusAccepted, e.ChannelTurnStatus)
		return
	}
	c.mu.Unlock()
	root, err := s.ensure(req.Session, wire.Session{Title: req.Title, Source: SourceAPI})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h, _ := s.store.Get(root)
	if err := s.acquire(root, h.Source, false); err != nil {
		writeError(w, busyStatus(err), err.Error()) // nothing admitted: the core tries again
		return
	}
	now := s.now().UTC()
	e := &channelEntry{ChannelTurnStatus: wire.ChannelTurnStatus{Execution: req.Execution, State: wire.ChannelRunning},
		Hash: hash, Root: root, At: now, Updated: now}
	c.mu.Lock()
	if _, ok := c.turns[req.Execution]; ok { // admitted by a concurrent repeat meanwhile
		c.mu.Unlock()
		s.release(root, false)
		writeJSON(w, http.StatusAccepted, wire.ChannelTurnStatus{Execution: req.Execution, State: wire.ChannelRunning})
		return
	}
	c.turns[req.Execution] = e
	if err := c.save(); err != nil {
		delete(c.turns, req.Execution)
		c.mu.Unlock()
		s.release(root, false)
		writeError(w, http.StatusInternalServerError, "the turn could not be admitted: "+err.Error())
		return
	}
	accepted := e.ChannelTurnStatus
	c.notify()
	c.mu.Unlock()
	go s.channelRun(req, root, e)
	writeJSON(w, http.StatusAccepted, accepted)
}

// channelRun runs an admitted turn and keeps its result.
func (s *Server) channelRun(req wire.ChannelTurn, root string, e *channelEntry) {
	exec := &messaging.Execution{ID: req.Execution, Kind: req.Kind}
	message, opts := req.Message, loop.Options{}
	if req.Kind == messaging.ExecReturn {
		opts.InputKind = wire.KindReturn
	}
	res, err := func() (*loop.Result, error) {
		defer s.release(root, false)
		if len(req.Files) > 0 {
			block, images := s.takeFiles(req.Session, root, req.Files)
			message, opts.Images = strings.TrimSpace(message+"\n\n"+block), images
		}
		return s.run(messaging.WithExecution(s.base, exec), req.Session, message, opts)
	}()
	q, voiced := s.voice.Take(root) // taken whatever the turn did: never left for the next
	out := wire.ChannelTurnStatus{Execution: req.Execution, Acted: exec.Acted()}
	switch {
	case err != nil:
		out.State, out.Error = wire.ChannelFailed, s.redact("turn failed: "+err.Error())
	default:
		out.State, out.Session, out.Text = wire.ChannelOK, res.Session, res.Text
		// a return's update is text only; a person's message may be answered with a voice note
		if req.Kind == messaging.ExecInbound && voiced && s.speaker != nil && strings.TrimSpace(res.Text) != "" && res.Text != loop.NoAnswer {
			out.Audio = s.speak(res.Text, req.AudioFormat, q)
		}
	}
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	e.ChannelTurnStatus, e.Updated = out, s.now().UTC()
	if err := c.save(); err != nil {
		s.logf("channel turn %s: its result could not be kept: %v", req.Execution, err)
	}
	c.notify()
}

// channelGet answers a channel turn's status, waiting up to ?wait= seconds (at most 60) for it to end.
func (s *Server) channelGet(w http.ResponseWriter, r *http.Request) {
	if _, status, msg := s.signed(r); status != 0 {
		writeError(w, status, msg)
		return
	}
	id := r.PathValue("exec")
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(min(time.Duration(max(wait, 0))*time.Second, maxChannelWait))
	c := s.channel
	for {
		c.mu.Lock()
		e, ok := c.turns[id]
		changed := c.changed
		var st wire.ChannelTurnStatus
		if ok {
			st = e.ChannelTurnStatus
		}
		c.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "no such channel turn")
			return
		}
		left := time.Until(deadline)
		if st.Terminal() || left <= 0 {
			writeJSON(w, http.StatusOK, st)
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

// channelTake is the core having kept a turn's result: it is forgotten. A turn still running stays.
func (s *Server) channelTake(w http.ResponseWriter, r *http.Request) {
	if _, status, msg := s.signed(r); status != 0 {
		writeError(w, status, msg)
		return
	}
	id := r.PathValue("exec")
	c := s.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.turns[id]
	switch {
	case !ok:
		writeJSON(w, http.StatusOK, map[string]any{"taken": id})
	case !e.Terminal():
		writeError(w, http.StatusConflict, "the turn is still running")
	default:
		delete(c.turns, id)
		if err := c.save(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.notify()
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
