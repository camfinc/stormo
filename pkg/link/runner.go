package link

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// RunnerOptions configure a runner: a host that runs an instance's agents for one placement (a
// VPS…) and takes its orders from the core over a link it dials (docs/remote-core.md §6a).
type RunnerOptions struct {
	CoreURL   string
	Token     string // a person's token with the operate scope
	Placement string
	Version   string
	// Act carries out an order; log lines go back to whoever asked. state is the agent's after.
	Act    func(ctx context.Context, action, agent string, log func(string)) (state string, err error)
	Client *http.Client
	Logf   func(format string, args ...any)
}

// RunRunner keeps the runner's link up until ctx ends.
func RunRunner(ctx context.Context, o RunnerOptions) {
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	o.CoreURL = strings.TrimRight(o.CoreURL, "/")
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := runnerSession(ctx, o)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		o.Logf("runner: %v; again in %s", err, backoff.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff/2 + time.Duration(rand.Int64N(int64(backoff)))):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func runnerSession(ctx context.Context, o RunnerOptions) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, wsURLAt(o.CoreURL, RunnerPath), &websocket.DialOptions{
		HTTPClient: o.Client, HTTPHeader: http.Header{"Authorization": {"Bearer " + o.Token}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.SetReadLimit(1 << 20)
	defer c.CloseNow()
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var wmu sync.Mutex
	send := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(sctx, 10*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	hello, _ := Marshal(TypeHello, "", Hello{Agent: o.Placement, Placement: o.Placement, Version: o.Version})
	if err := send(hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	o.Logf("runner: serving %s for %s", o.Placement, o.CoreURL)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(sctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				stop()
				return
			}
		}
	}()
	// One order at a time: lifecycle actions on one host do not overlap.
	var omu sync.Mutex
	for {
		_, data, err := c.Read(sctx)
		if err != nil {
			return err
		}
		var env Envelope
		if json.Unmarshal(data, &env) != nil || env.Type != TypeOrder {
			continue
		}
		go func(env Envelope) {
			omu.Lock()
			defer omu.Unlock()
			res := Envelope{Type: TypeResult, Reply: env.ID}
			var ord Order
			if err := json.Unmarshal(env.Body, &ord); err != nil {
				res.Error = err.Error()
			} else {
				var mu sync.Mutex
				out := OrderResult{Log: []string{}}
				state, err := o.Act(sctx, ord.Action, ord.Agent, func(l string) { mu.Lock(); out.Log = append(out.Log, l); mu.Unlock() })
				out.State = state
				if err != nil {
					res.Error = err.Error()
				}
				res.Body, _ = json.Marshal(out)
			}
			b, _ := json.Marshal(res)
			_ = send(b)
		}(env)
	}
}
