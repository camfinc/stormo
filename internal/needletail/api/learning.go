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
// is runtime-owned, scrubbed from process environments, and never passed to core/tools.
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
	}
	out, err := a.LearningReview(r.Context(), session, req)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
