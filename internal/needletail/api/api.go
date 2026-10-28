// Package api is the runtime's HTTP API on port 8642 (docs/needletail.md §4.8), the one the core
// and `stormo chat` call through the adapter (pkg/engine/needletail: runtime.go, conversations.go).
// The adapter is the contract: status codes, bodies and lineage behaviour follow it exactly.
//
// Every route but /health needs "Authorization: Bearer <API_SERVER_KEY>". Turns are limited to one
// per conversation (409 when it is busy) and two chat turns in all (429), so the adapter's ErrBusy
// covers both; scheduled runs have their own lane (ReserveJob, one turn by default).
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Turns runs one turn (loop.Agent).
type Turns interface {
	Run(ctx context.Context, session, input string, opts loop.Options) (*loop.Result, error)
}

// SourceAPI is the source of sessions the API creates (Hermes' name for its API platform). The
// API creates nothing else: a request's source is ignored.
const SourceAPI = "api_server"

// The conversations the runtime makes itself, which the API neither creates nor continues
// (Reserved): a Slack conversation is the gateway's (slack-<channel>-…, source slack), a Telegram
// chat's too (telegram-<chat>…, source telegram), and a
// oneshot the owner's at the command line (oneshot-…, source cli). The memory reads a turn's
// audience from them (docs/needletail.md §4.6), so a caller that could make or resume one would
// claim its channel's records, or every scope.
var (
	reservedPrefixes = []string{"slack-", "telegram-", "oneshot-"}
	reservedSources  = []string{"slack", "telegram", "cli"}
)

// ErrReserved refuses an API request for a conversation the runtime makes itself.
var ErrReserved = errors.New("that conversation is the runtime's own (a Slack or Telegram conversation, a oneshot): the API does not create or continue it")

// Reserved: id, or the conversation the store has it in, is one the API may not create or
// continue: its id or its root's has a reserved prefix (case-folded: a case-insensitive disk
// holds one file for both spellings), or its root's source is a reserved one.
func Reserved(store *sessions.Store, id string) bool {
	if reservedID(id) {
		return true
	}
	root, ok := store.Root(id)
	if !ok {
		return false
	}
	h, _ := store.Get(root)
	return reservedID(root) || slices.Contains(reservedSources, h.Source)
}

func reservedID(id string) bool {
	low := strings.ToLower(id)
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// MinKey is the shortest API key served (the adapter's ValidAPIKey).
const MinKey = 16

const (
	defaultMaxTurns = 2
	defaultJobTurns = 1
	defaultGrace    = 3 * time.Second
	maxBody         = 4 << 20
	defaultList     = 50
	maxList         = 200
	defaultMessages = 200
	maxMessages     = 2000
)

// Errors of Acquire.
var (
	ErrSessionBusy = errors.New("this conversation is already running a turn")
	ErrBusy        = errors.New("the agent is running as many turns as it can")
	ErrClosing     = errors.New("the agent is shutting down")
)

// Server is the API and the turn scheduler behind it.
type Server struct {
	learningKey string
	key         string
	store       *sessions.Store
	agent       Turns
	idem        *idemStore
	maxTurns    int
	maxJobs     int
	log         func(string)
	redact      func(string) string
	now         func() time.Time
	jobs        func() ([]string, time.Time)
	platform    func() map[string]wire.Platform
	jail        string
	trust       func(conversation string) error

	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // running turns; Add only under mu, with closing checked
	grace  time.Duration  // how long Shutdown waits for cancelled turns to stop

	mu      sync.Mutex
	closing bool            // Shutdown has begun: no turn starts
	busy    map[string]bool // conversation roots with a running turn
	// stops cancel the turn running in a conversation (its root): Cancel, a person's /stop
	stops      map[string]context.CancelFunc
	active     int // turns held, both lanes
	jobTurns   int // of which scheduled runs' (their own lane)
	lastActive time.Time
	lastSource string
}

// Options configure a Server.
type Options struct {
	// LearningKey is a separate owner-only credential, never the agent/core API key.
	LearningKey string
	// Key is API_SERVER_KEY; shorter than MinKey refuses every authenticated route.
	Key   string
	Store *sessions.Store
	Agent Turns
	// StateDir holds idem.json; TmpDir is where its temporary file goes (the home's fsx.TmpDir,
	// which no snapshot rule claims; "": beside it).
	StateDir, TmpDir string
	// MaxTurns is the chat turns' cap (default 2): the API's, Slack's.
	MaxTurns int
	// JobTurns is the scheduled runs' own lane (ReserveJob, default 1), beside the chat's, so
	// neither starves the other.
	JobTurns int
	// StopGrace is how long Shutdown waits for turns it cancelled to stop (default 3 s).
	StopGrace time.Duration
	Log       func(string)
	Redact    func(string) string
	Now       func() time.Time
	// Jobs is the scheduler's running jobs and when the last one started (the activity's
	// running_jobs and last_job_at; nil: none).
	Jobs func() (running []string, lastStart time.Time)
	// Platforms is the messaging gateways' states (the activity's platforms; nil: none).
	Platforms func() map[string]wire.Platform
	// Jail is the activity's jail: how the agent's commands are confined.
	Jail string
	// Trust is the owner's endorsement of a conversation (POST /v1/sessions/{id}/trust): its
	// marked rows no longer taint it, recorded with an audit entry (docs/needletail.md §7). nil:
	// the route answers 501.
	Trust func(conversation string) error
}

// New builds the server.
func New(o Options) (*Server, error) {
	idem, err := loadIdem(o.StateDir, o.TmpDir, o.Now)
	if err != nil {
		return nil, err
	}
	s := &Server{key: o.Key, learningKey: o.LearningKey, store: o.Store, agent: o.Agent, idem: idem, maxTurns: o.MaxTurns, maxJobs: o.JobTurns, log: o.Log, redact: o.Redact,
		now: o.Now, grace: o.StopGrace, jobs: o.Jobs, platform: o.Platforms, jail: o.Jail, trust: o.Trust, busy: map[string]bool{}, stops: map[string]context.CancelFunc{}}
	if s.maxTurns <= 0 {
		s.maxTurns = defaultMaxTurns
	}
	if s.maxJobs <= 0 {
		s.maxJobs = defaultJobTurns
	}
	if s.grace <= 0 {
		s.grace = defaultGrace
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.redact == nil {
		s.redact = func(v string) string { return v }
	}
	s.base, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log(s.redact(fmt.Sprintf(format, args...)))
	}
}

// Handler is the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/sessions/{id}/learning", s.learning)
	mux.HandleFunc("POST /v1/sessions/{id}/learning", s.learning)
	mux.Handle("GET /v1/activity", s.auth(s.activity))
	mux.Handle("POST /v1/runs", s.auth(s.runs))
	mux.Handle("GET /v1/sessions", s.auth(s.list))
	mux.Handle("POST /v1/sessions", s.auth(s.create))
	mux.Handle("GET /v1/sessions/{id}/messages", s.auth(s.messages))
	mux.Handle("POST /v1/sessions/{id}/chat", s.auth(s.chat))
	mux.Handle("DELETE /v1/sessions/{id}", s.auth(s.remove))
	mux.Handle("POST /v1/sessions/{id}/trust", s.auth(s.trustConversation))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s %s is not served", r.Method, r.URL.Path))
	})
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(s.key) < MinKey || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong API key")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	var e wire.Error
	e.Error.Message = msg
	writeJSON(w, status, e)
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

// ---- turns -----------------------------------------------------------------------------------

// Acquire reserves a turn on root's conversation in the chat lane or the jobs' (job); release
// must follow. It counts the turn in wg under mu, after checking closing, so no turn starts once
// Shutdown has begun and no wg.Add races Shutdown's wg.Wait.
func (s *Server) acquire(root, source string, job bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrClosing
	}
	if s.busy[root] {
		return ErrSessionBusy
	}
	if job && s.jobTurns >= s.maxJobs || !job && s.active-s.jobTurns >= s.maxTurns {
		return ErrBusy
	}
	s.busy[root] = true
	s.active++
	if job {
		s.jobTurns++
	}
	s.lastActive, s.lastSource = s.now(), source
	s.wg.Add(1)
	return nil
}

func (s *Server) release(root string, job bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, root)
	s.active--
	if job {
		s.jobTurns--
	}
	s.lastActive = s.now()
	s.wg.Done()
}

// Hold takes session's conversation for a moment, outside both lanes: no turn starts in it until
// release, and none is running in it now (a follow-up's message appended to its transcript must
// never land between a turn's tool calls and their results). It counts as no turn (the caps, the
// activity). Errors: ErrSessionBusy (a turn runs in it), ErrClosing.
func (s *Server) Hold(session string) (release func(), err error) {
	root := session
	if r, ok := s.store.Root(session); ok {
		root = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return nil, ErrClosing
	case s.busy[root]:
		return nil, ErrSessionBusy
	}
	s.busy[root] = true
	s.wg.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.busy, root)
			s.mu.Unlock()
			s.wg.Done()
		})
	}, nil
}

// Turn runs one turn on session, creating it (with create's title and source) when it does not
// exist. It is the scheduler every caller goes through: the API, and later the gateway and cron.
func (s *Server) Turn(ctx context.Context, session, input string, create wire.Session, opts loop.Options) (*loop.Result, error) {
	r, err := s.Reserve(session, create)
	if err != nil {
		return nil, err
	}
	defer r.Release()
	return r.Turn(ctx, input, opts)
}

// Reservation is a turn slot held for one conversation (Reserve): it counts against the caps
// from the moment it is taken until Release, whether or not a turn runs in it.
type Reservation struct {
	s       *Server
	job     bool // the jobs' lane
	root    string
	session string
	create  wire.Session
	once    sync.Once
}

// Reserve takes a chat turn slot (the API's, Slack's) for session's conversation without running
// anything yet. A session that does not exist is created (with create's title and source) only
// when the turn runs. Errors as for a turn: ErrSessionBusy, ErrBusy, ErrClosing.
func (s *Server) Reserve(session string, create wire.Session) (*Reservation, error) {
	return s.reserve(session, create, false)
}

// ReserveJob is Reserve in the scheduled runs' own lane (JobTurns): a scheduled job holds it
// while it builds its prompt (one with scripts reserves only after they said the agent runs).
func (s *Server) ReserveJob(session string, create wire.Session) (*Reservation, error) {
	return s.reserve(session, create, true)
}

func (s *Server) reserve(session string, create wire.Session, job bool) (*Reservation, error) {
	if !sessions.ValidID(session) {
		return nil, sessions.ErrBadID
	}
	root, source := session, create.Source
	if r, ok := s.store.Root(session); ok {
		root = r
		h, _ := s.store.Get(root)
		source = h.Source
	}
	if err := s.acquire(root, source, job); err != nil {
		return nil, err
	}
	return &Reservation{s: s, job: job, root: root, session: session, create: create}, nil
}

// Turn runs one turn in the slot (creating the session if needed). It is cancelled with ctx,
// and when Shutdown gives up waiting, as the API's own turns are.
func (r *Reservation) Turn(ctx context.Context, input string, opts loop.Options) (*loop.Result, error) {
	root, err := r.s.ensure(r.session, r.create)
	if err != nil {
		return nil, err
	}
	if root != r.root {
		return nil, fmt.Errorf("session %s moved to another conversation", r.session)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.s.base, cancel)
	defer stop()
	r.s.mu.Lock()
	r.s.stops[r.root] = cancel
	r.s.mu.Unlock()
	defer func() {
		r.s.mu.Lock()
		delete(r.s.stops, r.root)
		r.s.mu.Unlock()
	}()
	return r.s.run(ctx, r.session, input, opts)
}

// Cancel stops the turn running in session's conversation (a person's /stop): its context is
// cancelled, so the model call or the tool running ends and the turn returns its error. False
// when no turn runs there.
func (s *Server) Cancel(session string) bool {
	root := session
	if r, ok := s.store.Root(session); ok {
		root = r
	}
	s.mu.Lock()
	stop := s.stops[root]
	s.mu.Unlock()
	if stop == nil {
		return false
	}
	stop()
	return true
}

// Running is whether a turn (or a hold) has session's conversation now.
func (s *Server) Running(session string) bool {
	root := session
	if r, ok := s.store.Root(session); ok {
		root = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[root]
}

// Release gives the slot back; calls after the first do nothing.
func (r *Reservation) Release() { r.once.Do(func() { r.s.release(r.root, r.job) }) }

func (s *Server) run(ctx context.Context, session, input string, opts loop.Options) (*loop.Result, error) {
	started := time.Now()
	res, err := s.agent.Run(ctx, session, input, opts)
	line := fmt.Sprintf("turn session=%s ms=%d", session, time.Since(started).Milliseconds())
	if res != nil {
		line += fmt.Sprintf(" tip=%s steps=%d tokens=%d", res.Session, res.Steps, res.Tokens)
	}
	if err != nil {
		line += " error=" + err.Error()
	}
	s.logf("%s", line)
	return res, err
}

// ensure is session's conversation root, creating the session when needed.
func (s *Server) ensure(session string, create wire.Session) (string, error) {
	if root, ok := s.store.Root(session); ok {
		return root, nil
	}
	create.ID = session
	if _, err := s.store.Create(create); err != nil && !errors.Is(err, sessions.ErrExists) {
		return "", err
	}
	root, ok := s.store.Root(session)
	if !ok {
		return "", sessions.ErrNotFound
	}
	return root, nil
}

func busyStatus(err error) int {
	switch {
	case errors.Is(err, ErrSessionBusy):
		return http.StatusConflict
	case errors.Is(err, ErrClosing):
		return http.StatusServiceUnavailable
	}
	return http.StatusTooManyRequests
}

// Shutdown stops new turns, waits for running ones until ctx ends, then cancels them and waits
// for them to stop, at most the grace period (a turn stuck in something that ignores its
// cancellation must not keep the process from exiting).
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		t := time.NewTimer(s.grace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			s.mu.Lock()
			n := s.active
			s.mu.Unlock()
			s.logf("shutdown: %d turns still running %s after cancellation; not waiting for them", n, s.grace)
		}
	}
	s.cancel()
}

// ---- routes ----------------------------------------------------------------------------------

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	a := wire.Activity{ActiveTurns: s.active, Busy: s.active > 0, Source: s.lastSource, RunningJobs: []string{}, Platforms: map[string]wire.Platform{}, Jail: s.jail}
	if !s.lastActive.IsZero() {
		a.LastActive = s.lastActive.UTC().Format(time.RFC3339Nano)
	}
	s.mu.Unlock()
	if s.jobs != nil {
		running, last := s.jobs()
		a.RunningJobs = append(a.RunningJobs, running...)
		if !last.IsZero() {
			a.LastJobAt = last.UTC().Format(time.RFC3339Nano)
		}
	}
	if s.platform != nil {
		for name, p := range s.platform() {
			a.Platforms[name] = p
		}
	}
	writeJSON(w, http.StatusOK, a)
}

// runs starts a turn in the background (the core's wake). A repeated Idempotency-Key answers with
// the run it started, for 24 h.
func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	var req wire.RunRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not a run request")
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeError(w, http.StatusBadRequest, "input is empty")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 256 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is too long")
		return
	}
	s.idem.mu.Lock()
	defer s.idem.mu.Unlock()
	if key != "" {
		if prev, ok := s.idem.get(key); ok {
			writeJSON(w, http.StatusAccepted, prev)
			return
		}
	}
	session := req.SessionID
	if session == "" {
		session = sessions.NewID()
	}
	if !sessions.ValidID(session) {
		writeError(w, http.StatusBadRequest, sessions.ErrBadID.Error())
		return
	}
	if Reserved(s.store, session) {
		writeError(w, http.StatusForbidden, ErrReserved.Error())
		return
	}
	root, err := s.ensure(session, wire.Session{Source: SourceAPI})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h, _ := s.store.Get(root)
	if err := s.acquire(root, h.Source, false); err != nil {
		writeError(w, busyStatus(err), err.Error())
		return
	}
	tip, _ := s.store.Tip(session)
	accepted := wire.RunAccepted{RunID: "run_" + randHex(8), SessionID: tip}
	if key != "" {
		if err := s.idem.put(key, accepted); err != nil {
			s.logf("idempotency store: %v", err)
		}
	}
	go func() {
		defer s.release(root, false)
		_, _ = s.run(s.base, session, req.Input, loop.Options{})
	}()
	writeJSON(w, http.StatusAccepted, accepted)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, wire.SessionList{Data: s.store.List(intParam(r, "limit", defaultList, maxList))})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req wire.NewSession
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not a session")
		return
	}
	if Reserved(s.store, req.ID) {
		writeError(w, http.StatusForbidden, ErrReserved.Error())
		return
	}
	// always the API's own, whatever the body says: the source decides no audience, but a
	// listing or a hook should not show a Slack or oneshot conversation the API made
	h, err := s.store.Create(wire.Session{ID: req.ID, Title: req.Title, Source: SourceAPI})
	switch {
	case errors.Is(err, sessions.ErrExists):
		writeError(w, http.StatusConflict, "session exists")
	case errors.Is(err, sessions.ErrBadID):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, h)
	}
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	order := r.URL.Query().Get("order")
	if order != "" && order != "latest" && order != "oldest" {
		writeError(w, http.StatusBadRequest, `order is "latest" or "oldest"`)
		return
	}
	t, err := s.store.Transcript(id, intParam(r, "limit", defaultMessages, maxMessages), order == "latest")
	if errors.Is(err, sessions.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// chat runs a turn and answers with its reply. The turn is not tied to the request: a client that
// leaves does not lose the turn, which stays in the transcript.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req wire.ChatRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not a chat request")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is empty")
		return
	}
	if Reserved(s.store, id) {
		writeError(w, http.StatusForbidden, ErrReserved.Error())
		return
	}
	root, ok := s.store.Root(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	h, _ := s.store.Get(root)
	if err := s.acquire(root, h.Source, false); err != nil {
		writeError(w, busyStatus(err), err.Error())
		return
	}
	res, err := func() (*loop.Result, error) {
		defer s.release(root, false)
		return s.run(s.base, id, req.Message, loop.Options{})
	}()
	if err != nil {
		writeError(w, http.StatusBadGateway, s.redact("turn failed: "+err.Error()))
		return
	}
	var out wire.ChatResponse
	out.SessionID, out.Message.Content = res.Session, res.Text
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	root, ok := s.store.Root(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	// checked and deleted under the scheduler's lock, so no turn starts in between
	s.mu.Lock()
	if s.busy[root] {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, ErrSessionBusy.Error())
		return
	}
	n, err := s.store.DeleteLineage(id)
	s.mu.Unlock()
	if errors.Is(err, sessions.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.Deleted{Deleted: n})
}

// trustConversation is the owner endorsing a conversation (its marked rows no longer taint it),
// with the API's key: the owner's. Refused while a turn runs in it.
func (s *Server) trustConversation(w http.ResponseWriter, r *http.Request) {
	if s.trust == nil {
		writeError(w, http.StatusNotImplemented, "trusting a conversation is not available")
		return
	}
	id := r.PathValue("id")
	root, ok := s.store.Root(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	release, err := s.Hold(root)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	defer release()
	if err := s.trust(root); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trusted": root})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
