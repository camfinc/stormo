package core

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Access to the core's routes (docs/remote-core.md §3). /health is public. The agent routes
// (/v1/*, /ingest/*, /mcp) check agent keys themselves. Everything else needs a person's token:
// read for GET, operate for anything that changes state, admin for tokens and the audit log. A
// request that reaches the loopback listener directly may still read the views that were always
// open locally (the office, the fleet, the instance's look) while core.loopback_trust is on.

// openViews are the reads a direct loopback request gets without a token.
func openView(path string) bool {
	switch path {
	case "/", "/review", "/api/core", "/api/instance", "/api/fleet", "/api/gateway", "/api/gateway/models", "/api/learning":
		return true
	}
	if uiFiles[path].body != nil || avatarPath.MatchString(path) {
		return true
	}
	if m := agentPath.FindStringSubmatch(path); m != nil && m[2] == "timeline" {
		return true
	}
	return false
}

func agentRoute(path string) bool {
	return path == "/mcp" || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/ingest/")
}

// needs reports the scope a request needs and whether the caller has it.
func (c *Core) needs(r *http.Request, who Caller) (Scope, bool) {
	path := r.URL.Path
	if path == "/health" || agentRoute(path) {
		return ScopeNone, true
	}
	// An agent's own activity: the route checks the key matches the agent.
	if m := agentPath.FindStringSubmatch(path); m != nil && who.Agent != "" && r.Method == http.MethodGet {
		return ScopeNone, true
	}
	need := ScopeRead
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		need = ScopeOperate
	}
	if strings.HasPrefix(path, "/api/tokens") || path == "/api/audit" {
		need = ScopeAdmin
	}
	if who.Can(need) {
		return need, true
	}
	if need == ScopeRead && c.loopbackTrust && fromLoopback(r) && openView(path) {
		return need, true
	}
	return need, false
}

// handleAdmin serves the token and audit routes; false when the path is not one of them.
func (c *Core) handleAdmin(w http.ResponseWriter, r *http.Request, who Caller) bool {
	path := r.URL.Path
	switch {
	case path == "/api/tokens" && r.Method == http.MethodGet:
		ts, err := c.DB.Tokens(r.URL.Query().Get("all") == "1")
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read tokens"))
			return true
		}
		writeJSONBody(w, 200, map[string]any{"tokens": ts})
	case path == "/api/tokens" && r.Method == http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
			// Days until it expires; 0 never.
			Days int `json:"days"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
			writeJSONBody(w, 400, errBody("bad_request", "body is {name, scope, days}"))
			return true
		}
		sc, err := ParseScope(req.Scope)
		if err != nil || req.Days < 0 || req.Days > 3650 {
			writeJSONBody(w, 400, errBody("bad_request", "scope is read, operate or admin; days 0 to 3650"))
			return true
		}
		value, info, err := c.DB.AddToken(req.Name, sc, time.Duration(req.Days)*24*time.Hour)
		if err != nil {
			writeJSONBody(w, 400, errBody("refused", err.Error()))
			return true
		}
		c.DB.Audit(who.Actor(), "token.add", req.Name, sc.String()+" days="+strconv.Itoa(req.Days))
		writeJSONBody(w, 201, map[string]any{"token": value, "info": info})
	case strings.HasPrefix(path, "/api/tokens/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/tokens/")
		if err := c.DB.RevokeToken(name); err != nil {
			writeJSONBody(w, 404, errBody("not_found", err.Error()))
			return true
		}
		c.DB.Audit(who.Actor(), "token.revoke", name, "")
		writeJSONBody(w, 200, map[string]any{"revoked": name})
	case path == "/api/audit" && r.Method == http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		log, err := c.DB.AuditLog(limit)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read the audit log"))
			return true
		}
		writeJSONBody(w, 200, map[string]any{"entries": log})
	default:
		return false
	}
	return true
}
