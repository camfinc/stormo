package llm

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// imageParts are the image_url parts of a request's user messages, and their text parts.
func imageParts(body map[string]any) (urls, texts []string, plain []string) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		switch c := mm["content"].(type) {
		case string:
			plain = append(plain, c)
		case []any:
			for _, p := range c {
				pm, _ := p.(map[string]any)
				switch pm["type"] {
				case "text":
					texts = append(texts, pm["text"].(string))
				case "image_url":
					iu, _ := pm["image_url"].(map[string]any)
					urls = append(urls, iu["url"].(string))
				}
			}
		}
	}
	return urls, texts, plain
}

// A user message with images is sent as content parts (text, then each image as a data URL); a
// message without stays a string.
func TestImagesAreContentParts(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		sse(w, true, text("I see a cat."))
	}))
	defer srv.Close()
	c := &Client{Endpoints: []Endpoint{{BaseURL: srv.URL + "/v1", Model: "m"}}}
	png := []byte("\x89PNG fake")
	_, err := c.NewTurn().Complete(context.Background(), Request{Messages: []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "what is this?", Images: []Image{{Name: "cat.png", MIME: "image/png", Data: png}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	urls, texts, plain := imageParts(rec.bodies[0])
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if len(urls) != 1 || urls[0] != want {
		t.Errorf("image parts %v", urls)
	}
	if len(texts) != 1 || texts[0] != "what is this?" || len(plain) != 1 || plain[0] != "sys" {
		t.Errorf("text parts %v, plain %v", texts, plain)
	}
}

// vision off: no image part reaches the model; the text says the image was not shown.
func TestVisionOffStripsImages(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		sse(w, true, text("ok"))
	}))
	defer srv.Close()
	c := &Client{Endpoints: []Endpoint{{BaseURL: srv.URL + "/v1", Model: "m"}}, Vision: VisionOff}
	if c.Sees(0) {
		t.Error("vision off sees images")
	}
	_, err := c.NewTurn().Complete(context.Background(), Request{Messages: []Message{
		{Role: "user", Content: "look", Images: []Image{{Name: "cat.png", MIME: "image/png", Data: []byte("x")}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	urls, _, plain := imageParts(rec.bodies[0])
	if len(urls) != 0 || len(plain) != 1 || !strings.Contains(plain[0], "cat.png") || !strings.Contains(plain[0], "not shown") {
		t.Errorf("urls %v plain %q", urls, plain)
	}
}

// vision auto: a model that refuses images (a 400 naming them) gets the request again without
// them, and the endpoint is not given images again for the process.
func TestVisionAutoFallsBackOnRefusal(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if rec.n() == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"This model does not support image input."}}`))
			return
		}
		sse(w, true, text("ok"))
	}))
	defer srv.Close()
	c := &Client{Endpoints: []Endpoint{{BaseURL: srv.URL + "/v1", Model: "m"}}}
	if !c.Sees(0) {
		t.Fatal("auto should try images")
	}
	req := Request{Messages: []Message{{Role: "user", Content: "look", Images: []Image{{Name: "cat.png", MIME: "image/png", Data: []byte("x")}}}}}
	if _, err := c.NewTurn().Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if rec.n() != 2 {
		t.Fatalf("%d requests", rec.n())
	}
	if urls, _, _ := imageParts(rec.bodies[1]); len(urls) != 0 {
		t.Errorf("the retry still had images")
	}
	if c.Sees(0) {
		t.Error("the refusing endpoint is still given images")
	}
	// a plain 400 not about images is the request's fault: no retry
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"bad","message":"tools[3] is invalid"}}`))
	}))
	defer srv2.Close()
	c2 := &Client{Endpoints: []Endpoint{{BaseURL: srv2.URL + "/v1", Model: "m"}}}
	if _, err := c2.NewTurn().Complete(context.Background(), req); err == nil || !c2.Sees(0) {
		t.Errorf("err %v, sees %v", err, c2.Sees(0))
	}
}
