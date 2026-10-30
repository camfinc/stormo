// The image worker's adapter: one picture generated from a prompt (and reference images) on
// OpenRouter's Image API, for the capability contract (docs/capabilities.md): GET /health,
// POST /run {job, agent, input, files, out}. The key comes from the environment
// (OPENROUTER_API_KEY, the capability's secrets:); the agent never holds it. Standard library
// only: the image builds it with no module.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultModel leads the text-to-image arenas as of October 2026.
const defaultModel = "openai/gpt-image-2.5-flare"

const maxReference = 20 << 20

type job struct {
	Job   int64    `json:"job"`
	Files []string `json:"files"`
	Out   string   `json:"out"`
	Input struct {
		Prompt      string   `json:"prompt"`
		Model       string   `json:"model"`
		AspectRatio string   `json:"aspect_ratio"`
		Quality     string   `json:"quality"`
		Resolution  string   `json:"resolution"`
		Background  string   `json:"background"`
		References  []string `json:"references"`
		Name        string   `json:"name"`
	} `json:"input"`
}

type jobError struct {
	code int
	msg  string
}

func (e jobError) Error() string { return e.msg }

func bad(format string, a ...any) error { return jobError{400, fmt.Sprintf(format, a...)} }

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var j job
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&j); err != nil {
			http.Error(w, "body: "+err.Error(), 400)
			return
		}
		out, err := run(r.Context(), j)
		if err != nil {
			code := 422
			var je jobError
			if errors.As(err, &je) {
				code = je.code
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	log.Printf("image: listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

var ratio = regexp.MustCompile(`^(auto|\d+(\.\d+)?:\d+(\.\d+)?)$`)

func run(ctx context.Context, j job) (map[string]any, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil, bad("no OPENROUTER_API_KEY: add secrets: [OPENROUTER_API_KEY] to this capability in stormo.yaml (the value comes from the secrets file)")
	}
	in := j.Input
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, bad("prompt: what to draw")
	}
	if len(prompt) > 8000 {
		return nil, bad("prompt: %d characters, 8000 at most", len(prompt))
	}
	if j.Out == "" {
		return nil, bad("no out folder for the image")
	}
	if in.AspectRatio != "" && !ratio.MatchString(in.AspectRatio) {
		return nil, bad("aspect_ratio: like 9:16, 16:9, 1:1 or auto")
	}
	model := in.Model
	if model == "" {
		model = defaultModel
	}
	body := map[string]any{"model": model, "prompt": prompt, "n": 1}
	for k, v := range map[string]string{"aspect_ratio": in.AspectRatio, "quality": in.Quality, "resolution": in.Resolution, "background": in.Background} {
		if v != "" {
			body[k] = v
		}
	}
	refs, err := references(j.Files, in.References)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		body["input_references"] = refs
	}
	raw, _ := json.Marshal(body)
	base := os.Getenv("IMAGE_API_URL")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/images", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the image API is not reachable: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(answer))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, jobError{502, fmt.Sprintf("%s answered %d: %s", model, resp.StatusCode, msg)}
	}
	var res struct {
		Data []struct {
			B64       string `json:"b64_json"`
			MediaType string `json:"media_type"`
		} `json:"data"`
		Usage struct {
			Cost *float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(answer, &res); err != nil || len(res.Data) == 0 || res.Data[0].B64 == "" {
		return nil, jobError{502, fmt.Sprintf("%s answered no image", model)}
	}
	img, err := base64.StdEncoding.DecodeString(res.Data[0].B64)
	if err != nil {
		return nil, jobError{502, "the image is not base64"}
	}
	ext := map[string]string{"image/jpeg": ".jpg", "image/webp": ".webp", "image/svg+xml": ".svg"}[res.Data[0].MediaType]
	if ext == "" {
		ext = ".png"
	}
	name := free(j.Out, safeName(in.Name, "image"), ext)
	if err := os.WriteFile(filepath.Join(j.Out, name), img, 0o644); err != nil {
		return nil, err
	}
	out := map[string]any{"files": []string{name}, "model": model, "media_type": res.Data[0].MediaType, "cost_usd": res.Usage.Cost}
	if w, h, ok := size(img); ok {
		out["width"], out["height"] = w, h
	}
	return out, nil
}

// references are the handed-over images named in refs (all of them when refs is empty), as data URLs.
func references(files, refs []string) ([]map[string]any, error) {
	byName := map[string]string{}
	for _, f := range files {
		byName[filepath.Base(f)] = f
	}
	pick := files
	if len(refs) > 0 {
		pick = nil
		for _, r := range refs {
			f, ok := byName[filepath.Base(r)]
			if !ok {
				return nil, bad("reference %q is not among the files handed over", r)
			}
			pick = append(pick, f)
		}
	}
	if len(pick) > 8 {
		return nil, bad("%d references: 8 at most", len(pick))
	}
	out := []map[string]any{}
	for _, f := range pick {
		mime := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}[strings.ToLower(filepath.Ext(f))]
		if mime == "" {
			return nil, bad("reference %s: a PNG, JPEG or WebP", filepath.Base(f))
		}
		st, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		if st.Size() > maxReference {
			return nil, bad("reference %s is over 20 MB", filepath.Base(f))
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)}})
	}
	return out, nil
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s, fallback string) string {
	s = strings.Trim(unsafe.ReplaceAllString(strings.TrimSuffix(filepath.Base(s), filepath.Ext(s)), "-"), "-.")
	if s == "" {
		return fallback
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// free is name+ext, or name-2+ext… when the out folder already has it.
func free(dir, name, ext string) string {
	for i := 1; ; i++ {
		n := name + ext
		if i > 1 {
			n = fmt.Sprintf("%s-%d%s", name, i, ext)
		}
		if _, err := os.Stat(filepath.Join(dir, n)); errors.Is(err, os.ErrNotExist) {
			return n
		}
	}
}

// size reads a PNG's, JPEG's or WebP's width and height from its header.
func size(b []byte) (int, int, bool) {
	switch {
	case len(b) > 24 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24])), true
	case len(b) > 30 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		switch string(b[12:16]) {
		case "VP8X":
			return 1 + (int(b[24]) | int(b[25])<<8 | int(b[26])<<16), 1 + (int(b[27]) | int(b[28])<<8 | int(b[29])<<16), true
		case "VP8 ":
			return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF), true
		case "VP8L":
			v := binary.LittleEndian.Uint32(b[21:25])
			return int(v&0x3FFF) + 1, int(v>>14&0x3FFF) + 1, true
		}
	case len(b) > 4 && b[0] == 0xFF && b[1] == 0xD8:
		for i := 2; i+9 < len(b); {
			if b[i] != 0xFF {
				return 0, 0, false
			}
			m := b[i+1]
			if m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
				i += 2
				continue
			}
			if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
				return int(binary.BigEndian.Uint16(b[i+7:])), int(binary.BigEndian.Uint16(b[i+5:])), true
			}
			i += 2 + int(binary.BigEndian.Uint16(b[i+2:]))
		}
	}
	return 0, 0, false
}
