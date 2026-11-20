package needletail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
)

var _ engine.ChannelTurner = runtime{}

// ChannelTurns is the runtime's channel-turn client (docs/needletail.md §4.12a), every request
// signed with the agent's core key.
func (runtime) ChannelTurns(call engine.RawCall, coreKey string) engine.ChannelTurns {
	return channelTurns{call: call, key: coreKey}
}

type channelTurns struct {
	call engine.RawCall
	key  string
}

func (c channelTurns) do(ctx context.Context, method, uri string, body any, out any) (int, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	h := http.Header{}
	h.Set(wire.ChannelSignatureHeader, wire.SignChannel(c.key, method, uri, raw, time.Now().Unix(), hex.EncodeToString(nonce)))
	if raw != nil {
		h.Set("Content-Type", "application/json")
	}
	status, resp, err := c.call(ctx, method, uri, raw, h)
	if err != nil {
		return 0, err
	}
	if status >= 300 {
		var e wire.Error
		_ = json.Unmarshal(resp, &e)
		return status, fmt.Errorf("%s %s: HTTP %d: %s", method, uri, status, e.Error.Message)
	}
	if out != nil {
		if err := json.Unmarshal(resp, out); err != nil {
			return status, fmt.Errorf("%s %s: %v", method, uri, err)
		}
	}
	return status, nil
}

func (c channelTurns) Capabilities(ctx context.Context) (engine.ChannelCaps, error) {
	var caps wire.Capabilities
	if _, err := c.do(ctx, http.MethodGet, "/v1/capabilities", nil, &caps); err != nil {
		return engine.ChannelCaps{}, err
	}
	return engine.ChannelCaps{Turns: caps.ChannelTurns == wire.ChannelTurnsV1, Enforcing: caps.Enforcing}, nil
}

func status(s wire.ChannelTurnStatus) engine.ChannelTurnStatus {
	out := engine.ChannelTurnStatus{Execution: s.Execution, State: s.State, Tip: s.Session, Text: s.Text, Error: s.Error,
		Acted: s.Acted, Marked: s.Marked, Taken: s.Taken}
	if a := s.Audio; a != nil {
		out.Audio = &engine.Audio{MIME: a.MIME, Name: a.Name, Data: a.Data, WithText: a.WithText}
	}
	return out
}

func (c channelTurns) Start(ctx context.Context, t engine.ChannelTurn) (engine.ChannelTurnStatus, error) {
	req := wire.ChannelTurn{Execution: t.Execution, Token: t.Token, Kind: t.Kind, Session: t.Session, Title: t.Title, Message: t.In.Text, AudioFormat: t.In.AudioFormat,
		Files: chatFiles(ctx, t.In.Files)}
	var out wire.ChannelTurnStatus
	code, err := c.do(ctx, http.MethodPost, "/v1/channel/turns", req, &out)
	switch {
	case code == http.StatusConflict && err != nil && strings.Contains(err.Error(), "already running a turn"),
		code == http.StatusTooManyRequests, code == http.StatusServiceUnavailable:
		return engine.ChannelTurnStatus{}, fmt.Errorf("%w (%v)", engine.ErrChannelBusy, err)
	case code >= 400 && code < 500 && code != http.StatusRequestTimeout:
		// the runtime answered it will not run this request (a bad, forbidden or conflicting one)
		return engine.ChannelTurnStatus{}, fmt.Errorf("%w (%v)", engine.ErrChannelRefused, err)
	case err != nil:
		return engine.ChannelTurnStatus{}, err
	}
	return status(out), nil
}

func (c channelTurns) Status(ctx context.Context, exec string, wait time.Duration) (engine.ChannelTurnStatus, error) {
	var out wire.ChannelTurnStatus
	uri := "/v1/channel/turns/" + url.PathEscape(exec) + fmt.Sprintf("?wait=%d", int(wait/time.Second))
	code, err := c.do(ctx, http.MethodGet, uri, nil, &out)
	if code == http.StatusNotFound {
		return engine.ChannelTurnStatus{}, engine.ErrChannelUnknown
	}
	if err != nil {
		return engine.ChannelTurnStatus{}, err
	}
	return status(out), nil
}

func (c channelTurns) Take(ctx context.Context, exec string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/channel/turns/"+url.PathEscape(exec), nil, nil)
	return err
}
