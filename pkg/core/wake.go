package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// What the bus does outside the core: wake an agent with a run on its own API, and mirror an
// urgent message to the recipient's Slack home channel.

// HermesWake starts a run on a Hermes agent's API server (`POST /v1/runs`, Hermes 0.21+): input
// is the turn's user message, session the transcript it continues, idempotency the key Hermes
// uses to make a retried wake start one run. apiKey gives the agent's API_SERVER_KEY.
func HermesWake(apiKey func(id string) string, client *http.Client) WakeFn {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return func(ctx context.Context, a BusAgent, input, session, idempotency string) error {
		key := apiKey(a.ID)
		if len(key) < 16 {
			return fmt.Errorf("%s has no API_SERVER_KEY of 16+ characters", a.ID)
		}
		body, _ := json.Marshal(map[string]string{"input": input, "session_id": session})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoint+"/v1/runs", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotency)
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return fmt.Errorf("POST /v1/runs: HTTP %d", res.StatusCode)
		}
		return nil
	}
}

// SlackPostURL is Slack's chat.postMessage (a variable so tests can point it at a fake).
var SlackPostURL = "https://slack.com/api/chat.postMessage"

// SlackMirror posts to an agent's own Slack home channel with its own bot token, both as a local
// run sees them (the `local:` overlay applied). An agent without both is skipped with an error.
func SlackMirror(inst *instance.Instance, client *http.Client) MirrorFn {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return func(agent, text string) error {
		a, err := manifest.Load(inst.Root, agent, inst.Names.Secret)
		if err != nil {
			return err
		}
		eng, err := engines.Get(a.Engine.Kind, inst)
		if err != nil {
			return err
		}
		channel, _ := eng.Env(a, manifest.Local).Get("SLACK_HOME_CHANNEL")
		file, err := secrets.Load(secrets.Path(inst.Root))
		if err != nil {
			return err
		}
		token, _ := secrets.Resolve(file, a, manifest.Local).Values.Get("SLACK_BOT_TOKEN")
		if channel == "" || token == "" {
			return fmt.Errorf("%s has no Slack home channel or bot token for a local run", agent)
		}
		body, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
		req, err := http.NewRequest(http.MethodPost, SlackPostURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var out struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
			return fmt.Errorf("chat.postMessage: HTTP %d", res.StatusCode)
		}
		if !out.OK {
			return fmt.Errorf("chat.postMessage: %s", out.Error)
		}
		return nil
	}
}
