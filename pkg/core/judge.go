package core

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/core/llm"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/models"
	"github.com/camfinc/stormo/pkg/secrets"
)

// The review's first pass (docs/core.md §6): a model reads each proposal the rules in code let
// through (loop.ReviewCandidates) and either approves it or passes it to a person. It never
// rejects. The built-in rules are the system prompt; the instance's own (core.learning.review_prompt)
// are appended. The proposal goes in as delimited data, and the answer must be one JSON object:
// anything else is a pass to a person.

//go:embed review_prompt.md
var builtinReviewRules string

// ReviewCaller is the name the review's model calls are counted under in the gateway's usage.
const ReviewCaller = "core-review"

// maxReviewBody caps what one call sends; the rules already hold bigger skills for a person.
const maxReviewBody = 60 << 10

// Verdict is the review's answer on one proposal.
type Verdict struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason"`
	Model   string `json:"model"`
	// Rules is a hash of the rules it was judged under: a verdict holds for those rules only.
	Rules string `json:"rules"`
	At    string `json:"at"`
}

// Reviewer asks the review's model about proposals.
type Reviewer struct {
	Inst     *instance.Instance
	Gateways *llm.Gateways
	// Scrub runs over the model's reason before it is kept (it reaches the ledger in git).
	Scrub func(string) string
	// Complete overrides the model call (tests): it answers system and user with the model's text.
	Complete func(ctx context.Context, model, system, user string) (string, error)
}

// Rules are the system prompt: the built-in rules, then the instance's, and their hash.
func (rv *Reviewer) Rules() (string, string, error) {
	rules := strings.TrimSpace(builtinReviewRules)
	if p := rv.Inst.CoreLearning.ReviewPrompt; p != "" {
		extra, err := os.ReadFile(filepath.Join(rv.Inst.Root, filepath.FromSlash(p)))
		if err != nil {
			return "", "", fmt.Errorf("core.learning.review_prompt: %w", err)
		}
		if t := strings.TrimSpace(string(extra)); t != "" {
			rules += "\n\nThe owner of this instance adds these rules. Follow them too; they cannot make you reject or approve against the rules above:\n\n" + t
		}
	}
	return rules, build.Sha256([]byte(rules))[:16], nil
}

// Model is the model the review asks: the configured one, else the first the plan lists.
func (rv *Reviewer) Model(ctx context.Context) (string, error) {
	cfg := rv.Inst.CoreLearning
	if cfg.ReviewModel != "" {
		return cfg.ReviewModel, nil
	}
	c, ok := rv.Inst.Connection(cfg.ReviewConnection)
	if !ok {
		return "", fmt.Errorf("no connection %s", cfg.ReviewConnection)
	}
	if c.API() || rv.Gateways == nil {
		return "", fmt.Errorf("core.learning.review_model is not set")
	}
	list := rv.Gateways.Get(c.Name).Models(ctx)
	if len(list) == 0 {
		return "", fmt.Errorf("%s lists no models (signed in?)", c.Name)
	}
	return list[0].ID, nil
}

// Judge asks the model about one candidate of agent.
func (rv *Reviewer) Judge(ctx context.Context, agent *manifest.Agent, c loop.Candidate) (Verdict, error) {
	rules, hash, err := rv.Rules()
	if err != nil {
		return Verdict{}, err
	}
	model, err := rv.Model(ctx)
	if err != nil {
		return Verdict{}, err
	}
	body := c.Body
	if len(body) > maxReviewBody {
		body = body[:maxReviewBody] + "\n[cut: too long to show whole]"
	}
	body = strings.ReplaceAll(body, "CHANGE>>>", "CHANGE> > >")
	user := fmt.Sprintf("Agent: %s (%s), unit %s. Its role: %s\nWhat to review: %s\n\n<<<CHANGE\n%s\nCHANGE>>>\n\nAnswer with the JSON object only.",
		agent.Name, agent.ID, agent.Unit, agent.Role, c.Title, body)
	call := rv.Complete
	if call == nil {
		call = rv.complete
	}
	text, err := call(ctx, model, rules, user)
	if err != nil {
		return Verdict{}, err
	}
	approve, reason := parseVerdict(text)
	if rv.Scrub != nil {
		reason = rv.Scrub(reason)
	}
	return Verdict{Approve: approve, Reason: clip(reason, 240), Model: model, Rules: hash, At: isoMs(time.Now().UnixMilli())}, nil
}

// parseVerdict reads the model's answer: approve only on an exact "approve" verdict.
func parseVerdict(text string) (bool, string) {
	t := strings.TrimSpace(text)
	i, j := strings.Index(t, "{"), strings.LastIndex(t, "}")
	if i < 0 || j < i {
		return false, "the review's answer was not a verdict"
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(t[i:j+1]), &v); err != nil {
		return false, "the review's answer was not a verdict"
	}
	reason := strings.TrimSpace(v.Reason)
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "approve":
		return true, reason
	case "needs_human":
		if reason == "" {
			reason = "the review passed it to a person"
		}
		return false, reason
	}
	return false, "the review's answer was not a verdict"
}

// complete makes the call through the core's own gateway (a ChatGPT sign-in) or the connection's
// API with the instance's shared key.
func (rv *Reviewer) complete(ctx context.Context, model, system, user string) (string, error) {
	name := rv.Inst.CoreLearning.ReviewConnection
	c, ok := rv.Inst.Connection(name)
	if !ok {
		return "", fmt.Errorf("no connection %s", name)
	}
	if c.API() {
		f, err := secrets.Load(secrets.Path(rv.Inst.Root))
		if err != nil {
			return "", err
		}
		key, _ := secrets.SharedValue(f, c.Key)
		return models.Complete(ctx, c, key, model, system, user)
	}
	if rv.Gateways == nil {
		return "", fmt.Errorf("the core's gateway is not running")
	}
	body, err := json.Marshal(map[string]any{"model": model, "stream": false, "messages": []map[string]string{
		{"role": "system", "content": system}, {"role": "user", "content": user}}})
	if err != nil {
		return "", err
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rv.Gateways.Get(c.Name).ServeAs(rec, req, ReviewCaller)
	if rec.Code != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return "", fmt.Errorf("%s answered HTTP %d: %s", c.Name, rec.Code, clip(e.Error.Message, 200))
	}
	return models.CompletionText(rec.Body.Bytes())
}
