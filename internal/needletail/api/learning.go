package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/loop"
)

type learningReviewer interface {
	LearningReview(context.Context, string, *loop.LearningRequest) (loop.LearningData, error)
}

// The ordinary API credential cannot assert human feedback. This dedicated owner key
// is runtime-owned and scrubbed from tools; the core holds its own owner-side copy.
func (s *Server) learning(w http.ResponseWriter, r *http.Request) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		http.Error(w, "unauthorized", 401)
		return
	}
	key := strings.TrimPrefix(header, "Bearer ")
	if s.learningKey == s.key || len(s.learningKey) < MinKey || subtle.ConstantTimeCompare([]byte(key), []byte(s.learningKey)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	a, ok := s.agent.(learningReviewer)
	if !ok {
		http.Error(w, "learning unavailable", 503)
		return
	}
	session := r.PathValue("id")
	if _, ok := s.store.Root(session); !ok {
		http.Error(w, "session not found", 404)
		return
	}
	var req *loop.LearningRequest
	if r.Method == http.MethodPost {
		req = &loop.LearningRequest{}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 4097))
		if readErr != nil || len(body) > 4096 {
			http.Error(w, "feedback too large", 400)
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(req); err != nil {
			http.Error(w, "invalid feedback", 400)
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "trailing feedback data", 400)
			return
		}
		if !req.Valid() {
			http.Error(w, "invalid learning operation", 400)
			return
		}
	}
	out, err := a.LearningReview(r.Context(), session, req)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// Errors are allowlisted descriptions: file paths, source text and secret values never escape.
func writeLearningError(w http.ResponseWriter, err error) {
	code, message := "learning_refused", "Learning review was refused. Refresh the selected session and check its status."
	text := err.Error()
	switch {
	case strings.Contains(text, "untrusted"):
		code, message = "untrusted_session", "This conversation cannot produce owner feedback or proposals."
	case strings.Contains(text, "budget"):
		code, message = "budget_exhausted", "The reflection budget is exhausted. Feedback may already be saved; refresh the evidence."
	case strings.Contains(text, "private candidate") || strings.Contains(text, "sensitive"):
		code, message = "private_candidate", "Only a scrubbed agent-wide procedural candidate can be exported."
	case strings.Contains(text, "not found"):
		code, message = "not_found", "The selected event or candidate is no longer available in this session."
	case strings.Contains(text, "secret"):
		code, message = "sensitive_input", "Sensitive input was refused."
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
