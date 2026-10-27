package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

var runtimeLearningPath = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/learning$`)

// RuntimeLearningView contains private owner-only data. State is explicit even when
// no runtime, engine support or dedicated credential is available. Credentials and
// upstream failure bodies never appear in this response or the audit log.
type RuntimeLearningView struct {
	State    string                      `json:"state"`
	Message  string                      `json:"message,omitempty"`
	Sessions []engine.Conversation       `json:"sessions"`
	Session  string                      `json:"session,omitempty"`
	Data     *learning.RuntimeReviewData `json:"data,omitempty"`
}

func (c *Core) handleRuntimeLearning(w http.ResponseWriter, r *http.Request, agent string, roster func() []BusAgent) {
	w.Header().Set("Cache-Control", "no-store")
	who := c.caller(r)
	need := ScopeRead
	if r.Method == http.MethodPost {
		need = ScopeOperate
	}
	if !who.Can(need) {
		writeJSONBody(w, 401, errBody("unauthorized", "an owner token is required"))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSONBody(w, 405, errBody("method", "use GET or POST"))
		return
	}
	view := RuntimeLearningView{State: "unavailable", Sessions: []engine.Conversation{}}
	secretValues := map[string]string{}
	send := func(state, message string) {
		view.State = state
		view.Message = message
		writeJSONBody(w, 200, c.redactLearningView(view, secretValues))
	}
	m, err := manifest.Load(c.inst.Root, agent, c.inst.Names.Secret)
	if err != nil {
		send("unavailable", "The agent configuration is unavailable.")
		return
	}
	eng, err := engines.Get(m.Engine.Kind, c.inst)
	if err != nil {
		send("unsupported", "This engine does not expose learning review.")
		return
	}
	rt, ok := eng.Runtime().(engine.LearningRuntime)
	if !ok {
		send("unsupported", "This engine does not expose learning review.")
		return
	}
	session := r.URL.Query().Get("session")
	for name, values := range r.URL.Query() {
		if name != "session" || len(values) != 1 {
			writeJSONBody(w, 400, errBody("bad_request", "only a session selector is accepted"))
			return
		}
	}
	if len(session) > 256 || strings.ContainsAny(session, "\x00\r\n") {
		writeJSONBody(w, 400, errBody("bad_request", "invalid session"))
		return
	}
	view.Session = session
	var operation *learning.RuntimeReviewRequest
	if r.Method == http.MethodPost {
		if session == "" {
			writeJSONBody(w, 400, errBody("bad_request", "select a session first"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) > 4096 {
			writeJSONBody(w, 400, errBody("bad_request", "learning operation is too large"))
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		operation = &learning.RuntimeReviewRequest{}
		if dec.Decode(operation) != nil || dec.Decode(&struct{}{}) != io.EOF || !operation.Valid() {
			writeJSONBody(w, 400, errBody("bad_request", "invalid learning operation"))
			return
		}
	}
	file, err := secrets.Load(secrets.Path(c.inst.Root))
	if err != nil {
		send("missing_credential", "Owner learning credentials are not configured on the core.")
		return
	}
	// Optional declarations receive the same resolution as the runtime's local start.
	resolved := secrets.Resolve(file, m, manifest.Local)
	for _, optional := range m.OptionalSecrets {
		cp := *m
		cp.Secrets = append(append([]string{}, m.Secrets...), optional.Name)
		v := secrets.Resolve(file, &cp, manifest.Local)
		if value, ok := v.Values.Get(optional.Name); ok {
			resolved.Values.Set(optional.Name, value)
		}
	}
	secretValues = resolved.Values.Map()
	key, _ := resolved.Values.Get(rt.LearningKeyName())
	apiKey, _ := resolved.Values.Get(eng.Runtime().APIKeyName())
	if !eng.Runtime().ValidAPIKey(key) || !eng.Runtime().ValidAPIKey(apiKey) || key == apiKey {
		send("missing_credential", "A distinct owner learning credential and conversation credential are required on the core.")
		return
	}
	endpoint := ""
	if roster != nil {
		for _, a := range roster() {
			if a.ID == agent && a.Running {
				endpoint = a.Endpoint
				break
			}
		}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		send("unavailable", "The agent runtime is offline or unreachable from the core.")
		return
	}
	endpoint = strings.TrimRight(endpoint, "/")
	client := &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	call := func(credential string) engine.APICall {
		return func(ctx context.Context, method, path string, body any) (int, []byte, error) {
			var encoded []byte
			if body != nil {
				encoded, _ = json.Marshal(body)
			}
			req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(encoded))
			if err != nil {
				return 0, nil, err
			}
			req.Header.Set("Authorization", "Bearer "+credential)
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer res.Body.Close()
			b, err := io.ReadAll(io.LimitReader(res.Body, learning.MaxEvidenceBytes+1))
			if len(b) > learning.MaxEvidenceBytes {
				return 0, nil, io.ErrShortBuffer
			}
			return res.StatusCode, b, err
		}
	}
	sessions, err := eng.Runtime().Conversations(call(apiKey)).List(ctx, 100)
	if err != nil {
		send("unavailable", "The agent sessions could not be read. Check its connection and conversation credential.")
		return
	}
	if sessions != nil {
		view.Sessions = sessions
	}
	if session == "" {
		view.State = "ready"
		writeJSONBody(w, 200, c.redactLearningView(view, resolved.Values.Map()))
		return
	}
	method := http.MethodGet
	if operation != nil {
		method = http.MethodPost
	}
	status, body, err := call(key)(ctx, method, rt.LearningPath(session), operation)
	if err != nil {
		send("unavailable", "The runtime learning request did not complete. Refresh before retrying an operation.")
		return
	}
	if status == 401 || status == 403 {
		send("missing_credential", "The runtime refused the dedicated owner learning credential.")
		return
	}
	if status == 404 {
		send("unavailable", "The selected session or its learning API is unavailable.")
		return
	}
	if status == 503 {
		send("unavailable", "The runtime learning service is unavailable.")
		return
	}
	if status < 200 || status > 299 {
		send("error", safeLearningFailure(body))
		return
	}
	var data learning.RuntimeReviewData
	if json.Unmarshal(body, &data) != nil || len(data.Events) > learning.MaxOutcomeEvents || len(data.Candidates) > 100 || (data.Background != nil && len(data.Background.Candidates) > 256) {
		send("unavailable", "The runtime learning response could not be read.")
		return
	}
	view.State = "ready"
	view.Data = &data
	if operation != nil && c.DB != nil {
		action := "feedback"
		if operation.ExportID != "" {
			action = "export"
		}
		c.DB.Audit(who.Actor(), "learning."+action, agent, "owner session review")
	}
	writeJSONBody(w, 200, c.redactLearningView(view, resolved.Values.Map()))
}

func safeLearningFailure(body []byte) string {
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &failure)
	switch failure.Error.Code {
	case "untrusted_session":
		return "This conversation cannot produce feedback or proposals."
	case "budget_exhausted":
		return "The reflection budget is exhausted. Refresh the saved evidence."
	case "private_candidate":
		return "Only a scrubbed agent-wide procedural candidate can be exported."
	case "not_found":
		return "The selected event or candidate is no longer available in this session."
	case "sensitive_input":
		return "Sensitive input was refused."
	default:
		return "The runtime refused the learning operation. Refresh the selected session and check its status."
	}
}

func (c *Core) redactLearningView(view RuntimeLearningView, values map[string]string) RuntimeLearningView {
	// Redact string values after decoding, never JSON keys or numeric counters. Longest
	// values go first so an overlapping short secret cannot expose a longer key's suffix.
	values["owner"] = c.owner
	secrets := []string{}
	for _, value := range values {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	b, _ := json.Marshal(view)
	var object any
	if json.Unmarshal(b, &object) != nil {
		return unavailableLearningView()
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case string:
			for _, secret := range secrets {
				x = strings.ReplaceAll(x, secret, "[SECRET]")
			}
			return x
		case []any:
			for i := range x {
				x[i] = redact(x[i])
			}
			return x
		case map[string]any:
			for key, value := range x {
				x[key] = redact(value)
			}
			return x
		default:
			return v
		}
	}
	b, err := json.Marshal(redact(object))
	if err != nil {
		return unavailableLearningView()
	}
	var safe RuntimeLearningView
	if json.Unmarshal(b, &safe) != nil {
		return unavailableLearningView()
	}
	return safe
}
func unavailableLearningView() RuntimeLearningView {
	return RuntimeLearningView{State: "unavailable", Message: "The learning response could not be safely displayed.", Sessions: []engine.Conversation{}}
}
