package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Socket Mode (https://api.slack.com/apis/socket-mode): apps.connections.open gives a websocket
// URL; Slack sends "hello", then envelopes ({envelope_id, type, payload}) that must be acked
// with {"envelope_id": …} within 3 s or Slack sends them again; "disconnect" (refresh_requested,
// warning, link_disabled) asks for a new connection. Hermes runs slack_sdk's client with a
// watchdog that restarts a stale link; this is the same loop in Go.

// Socket timings.
const (
	pingEvery    = 30 * time.Second // our pings: a dead link fails a write, or the read deadline
	readTimeout  = 2*pingEvery + 10*time.Second
	backoffStart = time.Second
	backoffMax   = time.Minute
)

// envelope is one Socket Mode frame.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"`
	Retry      int             `json:"retry_attempt"`
}

// socket keeps one Socket Mode link up until its context ends.
type socket struct {
	web    *Web
	dialer *websocket.Dialer
	// handle gets every events_api payload, after its ack; ack decides whether to ack it (false:
	// left for Slack to send again, as during a shutdown).
	handle func(payload json.RawMessage)
	ack    func() bool
	state  func(string)
	logf   func(string, ...any)
	mu     sync.Mutex
	conn   *websocket.Conn
	wmu    sync.Mutex // one writer at a time on conn
	sleep  func(context.Context, time.Duration) error
}

// run connects and reconnects (backoff from 1 s doubling to 1 min, reset by a hello) until ctx
// ends.
func (s *socket) run(ctx context.Context) {
	wait := backoffStart
	for ctx.Err() == nil {
		s.state("connecting")
		link, err := s.web.ConnectionsOpen(ctx)
		if err == nil {
			err = s.checkLink(link)
		}
		if err == nil {
			var hello bool
			hello, err = s.serve(ctx, link)
			if hello {
				wait = backoffStart
			}
		}
		if ctx.Err() != nil {
			break
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "invalid_auth" || apiErr.Code == "not_authed" || apiErr.Code == "account_inactive") {
			s.state("fatal")
			s.logf("slack: the app token is refused (%s); trying again in %s", apiErr.Code, backoffMax)
			wait = backoffMax
		} else {
			s.state("disconnected")
			if err != nil {
				s.logf("slack: socket: %v; reconnecting in %s", err, wait)
			}
		}
		if s.sleep(ctx, wait) != nil {
			break
		}
		wait = min(wait*2, backoffMax)
	}
	s.state("disconnected")
}

// checkLink refuses a websocket URL that is not wss, unless the Web API is a loopback test
// server (whose links are ws on the loopback too).
func (s *socket) checkLink(link string) error {
	u, err := url.Parse(link)
	if err != nil {
		return err
	}
	if u.Scheme == "wss" {
		return nil
	}
	if u.Scheme == "ws" && s.web.BaseURL != "" && s.web.BaseURL != DefaultAPI && CheckBaseURL(s.web.BaseURL) == nil && CheckBaseURL("http://"+u.Host) == nil {
		return nil
	}
	return errors.New("slack: apps.connections.open gave a link that is not wss")
}

// serve runs one connection until it closes, Slack asks for a new one, or ctx ends; hello says
// whether Slack greeted it.
func (s *socket) serve(ctx context.Context, link string) (hello bool, err error) {
	conn, res, err := s.dialer.DialContext(ctx, link, nil)
	if res != nil && res.Body != nil {
		res.Body.Close()
	}
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
		conn.Close()
	}()
	conn.SetReadLimit(8 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		return s.write(conn, websocket.PongMessage, []byte(data))
	})
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readTimeout)) })
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = s.write(conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"))
				conn.Close()
				return
			case <-t.C:
				if s.write(conn, websocket.PingMessage, nil) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return hello, nil
			}
			return hello, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		var e envelope
		if json.Unmarshal(body, &e) != nil {
			continue
		}
		switch e.Type {
		case "hello":
			hello = true
			s.state("connected")
		case "disconnect":
			s.logf("slack: Slack asks for a new connection (%s)", orDefault(e.Reason, "no reason"))
			return hello, nil
		case "events_api":
			if e.EnvelopeID == "" || !s.ack() {
				continue
			}
			if err := s.write(conn, websocket.TextMessage, ackFrame(e.EnvelopeID)); err != nil {
				return hello, err
			}
			s.handle(e.Payload)
		default:
			// slash commands, interactivity, options: acked so Slack does not retry them;
			// needletail serves none of them (docs/needletail.md §1 non-goals)
			if e.EnvelopeID != "" {
				if err := s.write(conn, websocket.TextMessage, ackFrame(e.EnvelopeID)); err != nil {
					return hello, err
				}
			}
		}
	}
}

func ackFrame(id string) []byte {
	b, _ := json.Marshal(map[string]string{"envelope_id": id})
	return b
}

// write sends one frame; gorilla allows one writer at a time.
func (s *socket) write(conn *websocket.Conn, kind int, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	if kind == websocket.TextMessage {
		_ = conn.SetWriteDeadline(deadline)
		return conn.WriteMessage(kind, data)
	}
	return conn.WriteControl(kind, data, deadline)
}

// close ends the current connection (the run loop ends with its context).
func (s *socket) close() {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		_ = s.write(c, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"))
		c.Close()
	}
}
