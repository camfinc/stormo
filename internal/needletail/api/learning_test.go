package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/sessions"
)

type reviewAgent struct {
	fakeAgent
	calls int
}

func (a *reviewAgent) LearningReview(context.Context, string, *loop.LearningRequest) (loop.LearningData, error) {
	a.calls++
	return loop.LearningData{}, nil
}
func TestLearningDedicatedOwnerCredential(t *testing.T) {
	home := t.TempDir()
	store, err := sessions.Open(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A missing session is sufficient to distinguish owner authentication from a request
	// that reaches the handler; ordinary core/agent credentials must fail before lookup.
	a := &reviewAgent{}
	srv, err := New(Options{Key: "ordinary-api-key-012345", LearningKey: "owner-learning-key-012345", Store: store, Agent: a, StateDir: filepath.Join(home, "state")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want int
	}{{"ordinary-api-key-012345", 401}, {"", 401}, {"owner-learning-key-012345", 404}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/missing/learning", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%d != %d", w.Code, tc.want)
		}
	}
	if a.calls != 0 {
		t.Fatal(a.calls)
	}
}
