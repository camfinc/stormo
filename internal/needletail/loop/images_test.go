package loop

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// images in a request: each user message's image_url parts, in order (index of the message, url).
func requestImages(req map[string]any) (at []int, urls []string) {
	for i, m := range req["messages"].([]any) {
		parts, ok := m.(map[string]any)["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			pm := p.(map[string]any)
			if pm["type"] == "image_url" {
				at = append(at, i)
				urls = append(urls, pm["image_url"].(map[string]any)["url"].(string))
			}
		}
	}
	return at, urls
}

// contentText is a message's text: its content string, or its text parts joined.
func contentText(c any) string {
	switch x := c.(type) {
	case string:
		return x
	case []any:
		out := ""
		for _, p := range x {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				out += pm["text"].(string)
			}
		}
		return out
	}
	return ""
}

// A turn's images go with its input in every request of that turn, never into the session file,
// and not into the next turn; a tool may show one more (view_file), placed right after its result.
func TestTurnImages(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		last := lastRow(req)
		text := contentText(last["content"])
		switch {
		case last["role"] == "user" && strings.Contains(text, "again"):
			return say("no image now")
		case last["role"] == "user" && strings.Contains(text, "picture"):
			return callTool("call_v1", "show_it", `{}`)
		case last["role"] == "user" && strings.Contains(text, "b.jpg"):
			return say("shown")
		default:
			return say("unexpected")
		}
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.Tools.Add(tools.Tool{Name: "show_it", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (any, error) {
		if env.Show == nil {
			t.Error("no Show in a turn")
			return nil, nil
		}
		return map[string]any{"shown": true}, env.Show("b.jpg", "image/jpeg", []byte("JPEGB"))
	}})
	if _, err := a.Store.Create(wire.Session{ID: "s1", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "s1", "what is in this picture?", Options{Images: []llm.Image{{Name: "a.png", MIME: "image/png", Data: []byte("PNGA")}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "shown" || len(st.requests) != 2 {
		t.Fatalf("res %+v, %d requests", res, len(st.requests))
	}
	for i, req := range st.requests[:2] {
		at, urls := requestImages(req)
		if len(urls) < 1 || !strings.HasPrefix(urls[0], "data:image/png;base64,") || at[0] != 1 {
			t.Errorf("request %d: the input image is missing: %v %v", i, at, urls)
		}
	}
	// the shown image follows the tool result that showed it
	at, urls := requestImages(st.requests[1])
	msgs := st.requests[1]["messages"].([]any)
	if len(urls) != 2 || !strings.HasPrefix(urls[1], "data:image/jpeg;base64,") || msgs[at[1]-1].(map[string]any)["tool_call_id"] != "call_v1" {
		t.Errorf("shown image: %v %v", at, urls)
	}
	// the session file has no image bytes
	tr, _ := a.Store.Transcript("s1", 0, false)
	for _, m := range tr.Data {
		if strings.Contains(m.Content, "PNGA") || strings.Contains(m.Content, "base64") {
			t.Errorf("image bytes stored: %q", m.Content)
		}
	}
	// the next turn sends no image
	if _, err := a.Run(context.Background(), "s1", "and again?", Options{}); err != nil {
		t.Fatal(err)
	}
	if _, urls := requestImages(st.requests[len(st.requests)-1]); len(urls) != 0 {
		t.Errorf("the next turn re-sent images: %d", len(urls))
	}
}

// At most MaxTurnImages images reach one turn: Show refuses past it.
func TestTurnImagesBounded(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		if lastRow(req)["role"] == "tool" {
			return say("ok")
		}
		return callTool("call_v1", "show_it", `{}`)
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	var showErr error
	a.Tools.Add(tools.Tool{Name: "show_it", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (any, error) {
		showErr = env.Show("x.png", "image/png", []byte("x"))
		return "ok", nil
	}})
	_, _ = a.Store.Create(wire.Session{ID: "s1", Source: "api_server"})
	imgs := []llm.Image{}
	for i := 0; i < MaxTurnImages; i++ {
		imgs = append(imgs, llm.Image{Name: "i.png", MIME: "image/png", Data: []byte("x")})
	}
	if _, err := a.Run(context.Background(), "s1", "look", Options{Images: imgs}); err != nil {
		t.Fatal(err)
	}
	if showErr == nil {
		t.Error("a fifth image was shown")
	}
}
