package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A 2×3 PNG (header only matters to size()).
var png = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x02\x00\x00\x00\x03"), make([]byte, 16)...)

func TestRunAsksTheImageAPIAndWritesThePicture(t *testing.T) {
	var got map[string]any
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/images" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(png), "media_type": "image/png"}},
			"usage": map[string]any{"cost": 0.0093}})
	}))
	defer api.Close()
	t.Setenv("IMAGE_API_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", "k-test")
	dir := t.TempDir()
	ref := filepath.Join(dir, "in", "frame.jpg")
	os.MkdirAll(filepath.Dir(ref), 0o755)
	os.WriteFile(ref, []byte("\xff\xd8jpeg"), 0o644)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)
	var j job
	j.Out, j.Files = out, []string{ref}
	j.Input.Prompt, j.Input.AspectRatio, j.Input.Name, j.Input.References = " A quiet beach at dawn ", "9:16", "cover bg", []string{"frame.jpg"}
	res, err := run(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer k-test" || got["model"] != defaultModel || got["prompt"] != "A quiet beach at dawn" || got["aspect_ratio"] != "9:16" {
		t.Fatalf("request %v (auth %q)", got, auth)
	}
	if _, ok := got["quality"]; ok {
		t.Fatal("unset options stay out of the request")
	}
	refs := got["input_references"].([]any)
	if url := refs[0].(map[string]any)["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("reference %s", url)
	}
	if res["files"].([]string)[0] != "cover-bg.png" || res["width"] != 2 || res["height"] != 3 || *res["cost_usd"].(*float64) != 0.0093 {
		t.Fatalf("answer %v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "cover-bg.png")); string(b) != string(png) {
		t.Fatal("the picture was not written")
	}
	res, _ = run(context.Background(), j)
	if res["files"].([]string)[0] != "cover-bg-2.png" {
		t.Fatalf("a second picture overwrote the first: %v", res["files"])
	}
}

func TestRunRefusesWithoutKeyPromptOrKnownReference(t *testing.T) {
	var j job
	j.Out = t.TempDir()
	j.Input.Prompt = "x"
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := run(context.Background(), j); err == nil || !strings.Contains(err.Error(), "secrets: [OPENROUTER_API_KEY]") {
		t.Fatalf("no key: %v", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	j.Input.Prompt = "  "
	if _, err := run(context.Background(), j); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("no prompt: %v", err)
	}
	j.Input.Prompt, j.Input.References = "x", []string{"nope.png"}
	if _, err := run(context.Background(), j); err == nil || !strings.Contains(err.Error(), "not among the files") {
		t.Fatalf("unknown reference: %v", err)
	}
	j.Input.References, j.Input.AspectRatio = nil, "tall"
	if _, err := run(context.Background(), j); err == nil || !strings.Contains(err.Error(), "aspect_ratio") {
		t.Fatalf("bad ratio: %v", err)
	}
}
