// The transcribe worker's adapter: it fetches the model into /cache on first boot, keeps
// whisper.cpp's server running with it, and serves the capability contract: GET /health once the
// model is loaded, POST /run {job, agent, input, files, out} with the transcript as the answer and
// transcript files written to out. Standard library only: the image builds it with no module.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	whisperAddr = "127.0.0.1:8178"
	modelBase   = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-%s.bin"
	maxAnswer   = 200 << 10
)

var (
	ready     atomic.Bool
	modelName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	one       sync.Mutex // whisper.cpp's server works one file at a time
)

func main() {
	model := os.Getenv("WHISPER_MODEL")
	if model == "" {
		model = "large-v3-turbo-q5_0"
	}
	if !modelName.MatchString(model) {
		log.Fatalf("WHISPER_MODEL %q is not a model name", model)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	go func() {
		path, err := fetchModel(model)
		if err != nil {
			log.Fatalf("model: %v", err)
		}
		if err := runWhisper(path); err != nil {
			log.Fatalf("whisper-server: %v", err)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "loading the model", 503)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("POST /run", run)
	log.Printf("transcribe: model %s, listening on :%s", model, port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// fetchModel downloads the model into the cache volume once; later boots find it there.
func fetchModel(model string) (string, error) {
	dir := "/cache/models"
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ggml-"+model+".bin")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return path, nil
	}
	log.Printf("downloading %s", fmt.Sprintf(modelBase, model))
	res, err := http.Get(fmt.Sprintf(modelBase, model))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("model %s: HTTP %d", model, res.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, res.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

func runWhisper(model string) error {
	args := []string{"-m", model, "--host", "127.0.0.1", "--port", strings.Split(whisperAddr, ":")[1]}
	if t, _ := strconv.Atoi(os.Getenv("WHISPER_THREADS")); t > 0 {
		args = append(args, "-t", strconv.Itoa(t))
	} else {
		args = append(args, "-t", strconv.Itoa(max(1, cpus())))
	}
	cmd := exec.Command("whisper-server", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		for {
			if c, err := net.DialTimeout("tcp", whisperAddr, time.Second); err == nil {
				c.Close()
				ready.Store(true)
				log.Print("model loaded")
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return cmd.Wait() // the container ends with it, and the core boots a fresh one
}

// cpus is what the container may use (its cgroup's quota), else the host's count.
func cpus() int {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			q, _ := strconv.Atoi(f[0])
			p, _ := strconv.Atoi(f[1])
			if q > 0 && p > 0 {
				return max(1, q/p)
			}
		}
	}
	n := 0
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		n = strings.Count(string(b), "processor\t:")
	}
	return max(1, n)
}

type job struct {
	Job   int64    `json:"job"`
	Files []string `json:"files"`
	Out   string   `json:"out"`
	Input struct {
		Language  string   `json:"language"`
		Translate bool     `json:"translate"`
		Prompt    string   `json:"prompt"`
		Formats   []string `json:"formats"`
		Segments  *bool    `json:"segments"`
	} `json:"input"`
}

type segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

func fail(w http.ResponseWriter, code int, format string, a ...any) {
	http.Error(w, fmt.Sprintf(format, a...), code)
}

func run(w http.ResponseWriter, r *http.Request) {
	var j job
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&j); err != nil {
		fail(w, 400, "body: %v", err)
		return
	}
	if len(j.Files) == 0 {
		fail(w, 400, "give the audio or video file to transcribe (files)")
		return
	}
	formats := j.Input.Formats
	if len(formats) == 0 {
		formats = []string{"txt", "srt"}
	}
	for _, f := range formats {
		if f != "txt" && f != "srt" && f != "vtt" && f != "json" {
			fail(w, 400, "formats are txt, srt, vtt and json, not %q", f)
			return
		}
	}
	lang := strings.ToLower(strings.TrimSpace(j.Input.Language))
	if lang == "" {
		lang = "auto"
	}
	if !regexp.MustCompile(`^(auto|[a-z]{2,3})$`).MatchString(lang) {
		fail(w, 400, "language is an ISO code (es, en…) or auto")
		return
	}
	src := j.Files[0]
	wav := filepath.Join(os.TempDir(), fmt.Sprintf("job-%d.wav", j.Job))
	defer os.Remove(wav)
	conv := exec.CommandContext(r.Context(), "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", src, "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav)
	if out, err := conv.CombinedOutput(); err != nil {
		fail(w, 422, "ffmpeg could not read %s: %s", filepath.Base(src), strings.TrimSpace(string(out)))
		return
	}
	one.Lock()
	res, err := inference(r.Context(), wav, lang, j.Input.Translate, j.Input.Prompt)
	one.Unlock()
	if err != nil {
		fail(w, 502, "whisper: %v", err)
		return
	}
	segs := []segment{}
	parts := []string{}
	for _, s := range res.Segments {
		t := strings.TrimSpace(s.Text)
		segs = append(segs, segment{Start: round(s.Start), End: round(s.End), Text: t})
		parts = append(parts, t)
	}
	text := strings.Join(parts, " ")
	if len(segs) == 0 {
		text = strings.Join(strings.Fields(res.Text), " ")
	}
	written := []string{}
	if j.Out != "" {
		name := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		for _, f := range formats {
			var body []byte
			switch f {
			case "txt":
				body = []byte(text + "\n")
			case "srt":
				body = subtitles(segs, false)
			case "vtt":
				body = subtitles(segs, true)
			case "json":
				body, _ = json.MarshalIndent(map[string]any{"language": res.Language, "duration_seconds": round(res.Duration), "text": text, "segments": segs}, "", "  ")
			}
			if err := os.WriteFile(filepath.Join(j.Out, name+"."+f), body, 0o644); err != nil {
				fail(w, 500, "writing %s.%s: %v", name, f, err)
				return
			}
			written = append(written, name+"."+f)
		}
	}
	answer := map[string]any{"language": res.Language, "duration_seconds": round(res.Duration), "files": written, "truncated": false}
	if len(text) > maxAnswer {
		text, answer["truncated"] = text[:maxAnswer], true
	}
	answer["text"] = text
	if j.Input.Segments == nil || *j.Input.Segments {
		size, keep := 0, 0
		for _, s := range segs {
			if size += len(s.Text) + 40; size > maxAnswer {
				answer["truncated"] = true
				break
			}
			keep++
		}
		answer["segments"] = segs[:keep]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
}

func round(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

type verbose struct {
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Text     string    `json:"text"`
	Segments []segment `json:"segments"`
}

// inference asks whisper.cpp's server for a verbose JSON transcript of a 16 kHz mono WAV.
func inference(ctx context.Context, wav, lang string, translate bool, prompt string) (*verbose, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", filepath.Base(wav))
	f, err := os.Open(wav)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(fw, f)
	f.Close()
	if err != nil {
		return nil, err
	}
	mw.WriteField("response_format", "verbose_json")
	mw.WriteField("language", lang)
	mw.WriteField("temperature", "0.0")
	if translate {
		mw.WriteField("translate", "true")
	}
	if prompt != "" {
		mw.WriteField("prompt", prompt)
	}
	mw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+whisperAddr+"/inference", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, bytes.TrimSpace(raw))
	}
	var v verbose
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("its answer: %v", err)
	}
	if v.Language == "" && len(v.Segments) == 0 && v.Text == "" && bytes.Contains(raw, []byte(`"error"`)) {
		return nil, errors.New(string(bytes.TrimSpace(raw)))
	}
	return &v, nil
}

func subtitles(segs []segment, vtt bool) []byte {
	var b strings.Builder
	if vtt {
		b.WriteString("WEBVTT\n\n")
	}
	stamp := func(t float64) string {
		ms := int64(t*1000 + 0.5)
		sep := ","
		if vtt {
			sep = "."
		}
		return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
	}
	for i, s := range segs {
		if !vtt {
			fmt.Fprintf(&b, "%d\n", i+1)
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(s.Start), stamp(s.End), s.Text)
	}
	return []byte(b.String())
}
