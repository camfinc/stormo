package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Upload posts the bytes to the core's job route with the agent's key, next to the MCP URL.
func TestUpload(t *testing.T) {
	var gotPath, gotName, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotName, gotAuth = r.URL.Path, r.URL.Query().Get("name"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Query().Get("name") == "nope.mp4" {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"refused","message":"job 3 is done: it waits for no files"}}`)
			return
		}
		io.WriteString(w, `{"job":3,"status":"queued"}`)
	}))
	defer srv.Close()
	c := New(srv.URL+"/mcp", "k-123", nil)
	out, err := c.Upload(context.Background(), 3, "clip one.mp4", []byte("bytes"))
	if err != nil || gotPath != "/v1/capabilities/jobs/3/files" || gotName != "clip one.mp4" || gotAuth != "Bearer k-123" || gotBody != "bytes" ||
		!strings.Contains(string(out), "queued") {
		t.Fatalf("%s %v | %s %s %s %s", out, err, gotPath, gotName, gotAuth, gotBody)
	}
	if _, err := c.Upload(context.Background(), 3, "nope.mp4", []byte("x")); err == nil || !strings.Contains(err.Error(), "waits for no files") {
		t.Errorf("refusal: %v", err)
	}
}
