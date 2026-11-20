// The decide worker's adapter: typed decisions (yes/no, a choice, a score, a box on a picture) about
// a state, from a structured decision model on OpenRouter's Decisions API, for the capability
// contract (docs/capabilities.md): GET /health, POST /run {job, agent, files, input}. One state per
// job, or a batch of states that share the questions, asked in parallel. Pictures handed over with
// the job are named in the state or the questions as {"image": "<file name>"}. The key comes from
// the environment (OPENROUTER_API_KEY, the capability's secrets:); the agent never holds it.
// Standard library only: the image builds it with no module.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // decodes the PNGs handed over
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultModel is pinned, never an alias: probabilities shift between versions, so callers tune
// their thresholds to one.
const defaultModel = "typesafe/jev-1.14"

const (
	maxQuestions  = 32
	maxStates     = 200
	maxStateBytes = 128 << 10
)

// The pictures a job may hand over, and the bounds the Decisions API takes them in (PNG or JPEG,
// each side 256-1568 px, 2,458,624 px at most; no EXIF rotation applied).
const (
	maxImages    = 16
	maxImageFile = 20 << 20
	minSide      = 256
	maxSide      = 1568
	maxPixels    = 2458624
)

type job struct {
	Job   int64    `json:"job"`
	Files []string `json:"files"`
	Input input    `json:"input"`
}

type input struct {
	Model     string                     `json:"model"`
	Questions map[string]json.RawMessage `json:"questions"`
	State     json.RawMessage            `json:"state"`
	States    map[string]json.RawMessage `json:"states"`
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
		out, err := run(r.Context(), j.Input, j.Files...)
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
	log.Printf("decide: listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

var questionName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// check refuses what the API would refuse anyway, with a reason an agent can act on.
func check(in input, pictures bool) error {
	if len(in.Questions) == 0 {
		return bad("questions: at least one, by name ({\"is_bug\": {\"type\": \"noul\", ...}})")
	}
	if len(in.Questions) > maxQuestions {
		return bad("questions: %d, %d at most", len(in.Questions), maxQuestions)
	}
	for name, raw := range in.Questions {
		if !questionName.MatchString(name) {
			return bad("question %q: a name of letters, digits, _ . - (64 at most)", name)
		}
		var q struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return bad("question %s: an object with type, instructions and criteria", name)
		}
		switch q.Type {
		case "noul":
			var c map[string]json.RawMessage
			if len(q.Criteria) > 0 && json.Unmarshal(q.Criteria, &c) != nil {
				return bad("question %s: noul criteria are {\"true\": \"…\", \"false\": \"…\"}", name)
			}
		case "choice":
			var c map[string]json.RawMessage
			if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 {
				return bad("question %s: choice criteria are two or more options, {\"option\": \"when to pick it\"}", name)
			}
		case "score":
			var c []json.RawMessage
			if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 {
				return bad("question %s: score criteria are two or more levels, lowest first", name)
			}
		case "box":
			if !pictures {
				return bad("question %s: box finds something on a picture: hand one over (files) and name it in the state as {\"image\": \"<file name>\"}", name)
			}
			if len(q.Criteria) > 0 {
				return bad("question %s: box takes no criteria: describe the target in instructions, and the picture it is on", name)
			}
		default:
			return bad("question %s: type is noul, choice, score or box", name)
		}
	}
	if (len(in.State) > 0) == (len(in.States) > 0) {
		return bad("give state (one decision) or states (a batch sharing the questions), not both")
	}
	if len(in.States) > maxStates {
		return bad("states: %d, %d at most a job", len(in.States), maxStates)
	}
	states := in.States
	if len(in.State) > 0 {
		states = map[string]json.RawMessage{"": in.State}
	}
	for id, s := range states {
		if len(s) > maxStateBytes {
			return bad("state %s: %d bytes, %d at most (past ~32 KB the model reads slower and less accurately)", id, len(s), maxStateBytes)
		}
		if t := strings.TrimSpace(string(s)); t == "null" || t == `""` || t == "{}" || t == "[]" {
			return bad("state %s: empty", id)
		}
	}
	return nil
}

type decision struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens int      `json:"input_tokens"`
		Cost        *float64 `json:"cost"`
	} `json:"usage"`
}

func run(ctx context.Context, in input, files ...string) (map[string]any, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil, bad("no OPENROUTER_API_KEY: add secrets: [OPENROUTER_API_KEY] to this capability in stormo.yaml (the value comes from the secrets file)")
	}
	if err := check(in, len(files) > 0); err != nil {
		return nil, err
	}
	if len(files) > 0 {
		var err error
		if in, err = withPictures(in, files); err != nil {
			return nil, err
		}
	}
	model := in.Model
	if model == "" {
		model = defaultModel
	}
	if len(in.State) > 0 {
		d, err := ask(ctx, key, model, in.State, in.Questions)
		if err != nil {
			return nil, err
		}
		return map[string]any{"model": d.Model, "answers": d.Answers, "input_tokens": d.Usage.InputTokens, "cost_usd": d.Usage.Cost}, nil
	}

	ids := make([]string, 0, len(in.States))
	for id := range in.States {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := make(map[string]any, len(ids))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		cost     float64
		tokens   int
		failed   int
		firstErr error
		snapshot string
	)
	slots := make(chan struct{}, parallel())
	for _, id := range ids {
		wg.Add(1)
		slots <- struct{}{}
		go func(id string) {
			defer func() { <-slots; wg.Done() }()
			d, err := ask(ctx, key, model, in.States[id], in.Questions)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				results[id] = map[string]any{"error": err.Error()}
				return
			}
			snapshot = d.Model
			tokens += d.Usage.InputTokens
			if d.Usage.Cost != nil {
				cost += *d.Usage.Cost
			}
			results[id] = map[string]any{"answers": d.Answers}
		}(id)
	}
	wg.Wait()
	if failed == len(ids) {
		return nil, firstErr // nothing decided: the job fails with why
	}
	return map[string]any{"model": snapshot, "results": results, "decided": len(ids) - failed, "failed": failed,
		"input_tokens": tokens, "cost_usd": cost}, nil
}

// parallel is how many states of a batch are asked at once (DECIDE_PARALLEL, default 8).
func parallel() int {
	if n, err := strconv.Atoi(os.Getenv("DECIDE_PARALLEL")); err == nil && n > 0 && n <= 32 {
		return n
	}
	return 8
}

// retried are the answers worth asking again: rate limits, the API's or the model's own trouble.
var retried = map[int]bool{429: true, 500: true, 502: true, 503: true, 524: true, 529: true}

// backoff is the wait before each retry (two of them).
var backoff = []time.Duration{time.Second, 3 * time.Second}

// ask puts one state's questions to the Decisions API.
func ask(ctx context.Context, key, model string, state json.RawMessage, questions map[string]json.RawMessage) (decision, error) {
	raw, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": questions})
	url := os.Getenv("DECISIONS_API_URL")
	if url == "" {
		url = "https://openrouter.ai/api/alpha/decisions"
	}
	var last error
	for try := 0; ; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return decision{}, ctx.Err()
			case <-time.After(backoff[try-1]):
			}
		}
		d, status, err := post(ctx, url, key, raw)
		if err == nil {
			return d, nil
		}
		last = err
		if ctx.Err() != nil || (status != 0 && !retried[status]) || try == len(backoff) {
			return decision{}, last
		}
	}
}

// post is one call; status is 0 when no answer came (the network), else the HTTP status.
func post(ctx context.Context, url, key string, raw []byte) (decision, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return decision{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return decision{}, 0, fmt.Errorf("the Decisions API is not reachable: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return decision{}, 0, err
	}
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(answer))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return decision{}, resp.StatusCode, jobError{502, fmt.Sprintf("the Decisions API answered %d: %s", resp.StatusCode, msg)}
	}
	var d decision
	if err := json.Unmarshal(answer, &d); err != nil || len(d.Answers) == 0 || d.Answers[0] != '{' {
		return decision{}, resp.StatusCode, jobError{502, "the Decisions API answered no answers"}
	}
	return d, 200, nil
}

// withPictures puts the handed-over pictures where the state and the questions name them.
func withPictures(in input, files []string) (input, error) {
	pics, err := pictures(files)
	if err != nil {
		return in, err
	}
	used := map[string]bool{}
	out := in
	out.Questions = make(map[string]json.RawMessage, len(in.Questions))
	for name, q := range in.Questions {
		if out.Questions[name], err = expand(q, pics, used); err != nil {
			return in, fmt.Errorf("question %s: %w", name, err)
		}
	}
	if len(in.State) > 0 {
		if out.State, err = expand(in.State, pics, used); err != nil {
			return in, err
		}
	}
	if len(in.States) > 0 {
		out.States = make(map[string]json.RawMessage, len(in.States))
		for id, s := range in.States {
			if out.States[id], err = expand(s, pics, used); err != nil {
				return in, fmt.Errorf("state %s: %w", id, err)
			}
		}
	}
	for name := range pics {
		if !used[name] {
			return in, bad("%s was handed over but never named: put {\"image\": %q} where the model should look at it", name, name)
		}
	}
	return out, nil
}

// pictures reads each handed-over file, fitted to the API's bounds, as an image_url part by its name.
func pictures(files []string) (map[string]json.RawMessage, error) {
	if len(files) > maxImages {
		return nil, bad("%d pictures: %d at most a job", len(files), maxImages)
	}
	out := map[string]json.RawMessage{}
	for _, f := range files {
		name := filepath.Base(f)
		if _, dup := out[name]; dup {
			return nil, bad("two pictures are named %s: give them different names", name)
		}
		st, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		if st.Size() > maxImageFile {
			return nil, bad("%s is over 20 MB", name)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		mime, data, err := fitted(b)
		if err != nil {
			return nil, bad("%s: %v", name, err)
		}
		out[name], _ = json.Marshal(map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}})
	}
	return out, nil
}

// fitted is the picture as the API takes it: as it is when it already fits, else scaled into the
// bounds and sent as a JPEG.
func fitted(b []byte) (string, []byte, error) {
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil || (format != "png" && format != "jpeg") {
		return "", nil, errors.New("a PNG or JPEG picture")
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	s := math.Min(1, math.Min(float64(maxSide)/float64(w), float64(maxSide)/float64(h)))
	if px := float64(w*h) * s * s; px > maxPixels {
		s *= math.Sqrt(maxPixels / px)
	}
	if short := float64(min(w, h)) * s; short < minSide {
		s = minSide / float64(min(w, h))
	}
	nw, nh := int(math.Round(float64(w)*s)), int(math.Round(float64(h)*s))
	if nw > maxSide || nh > maxSide || nw*nh > maxPixels {
		return "", nil, fmt.Errorf("%dx%d is too long and thin to read (each side %d-%d px)", w, h, minSide, maxSide)
	}
	if nw == w && nh == h {
		return "image/" + format, b, nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled(img, nw, nh), &jpeg.Options{Quality: 90}); err != nil {
		return "", nil, err
	}
	return "image/jpeg", buf.Bytes(), nil
}

// scaled resamples by area (each new pixel the weighted mean of the ones it covers), over white
// where the picture is transparent.
func scaled(src image.Image, nw, nh int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	flat := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Over)
	cols, rows := spans(w, nw), spans(h, nh)
	mid := make([]float32, nw*h*3) // a 1080x1920 frame: 20 MB

	for y := 0; y < h; y++ {
		for x, sp := range cols {
			for _, c := range sp {
				p := flat.Pix[y*flat.Stride+c.at*4:]
				for k := 0; k < 3; k++ {
					mid[(y*nw+x)*3+k] += float32(float64(p[k]) * c.w)
				}
			}
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y, sp := range rows {
		for x := 0; x < nw; x++ {
			var v [3]float64
			for _, c := range sp {
				for k := 0; k < 3; k++ {
					v[k] += float64(mid[(c.at*nw+x)*3+k]) * c.w
				}
			}
			p := dst.Pix[y*dst.Stride+x*4:]
			for k := 0; k < 3; k++ {
				p[k] = uint8(math.Min(255, math.Round(v[k])))
			}
			p[3] = 255
		}
	}
	return dst
}

type span struct {
	at int
	w  float64
}

// spans are, for each of n new pixels along an axis of size, the old pixels it covers and their weights.
func spans(size, n int) [][]span {
	step := float64(size) / float64(n)
	out := make([][]span, n)
	for i := range out {
		a, b := float64(i)*step, float64(i+1)*step
		for j := int(a); j < size && float64(j) < b; j++ {
			if w := math.Min(b, float64(j+1)) - math.Max(a, float64(j)); w > 0 {
				out[i] = append(out[i], span{j, w / step})
			}
		}
	}
	return out
}

// expand rewrites raw with each {"image": "<name>"} object replaced by that picture's part; the
// rest stays as written, in its order.
func expand(raw json.RawMessage, pics map[string]json.RawMessage, used map[string]bool) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := expandValue(dec, &buf, pics, used); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func expandValue(dec *json.Decoder, buf *bytes.Buffer, pics map[string]json.RawMessage, used map[string]bool) error {
	tok, err := dec.Token()
	if err != nil {
		return bad("not JSON: %v", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			buf.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					buf.WriteByte(',')
				}
				if err := expandValue(dec, buf, pics, used); err != nil {
					return err
				}
			}
			dec.Token()
			buf.WriteByte(']')
			return nil
		}
		var keys []string
		var vals []json.RawMessage
		for dec.More() {
			k, _ := dec.Token()
			var v bytes.Buffer
			if err := expandValue(dec, &v, pics, used); err != nil {
				return err
			}
			keys, vals = append(keys, k.(string)), append(vals, v.Bytes())
		}
		dec.Token()
		var name string
		if len(keys) == 1 && keys[0] == "image" && json.Unmarshal(vals[0], &name) == nil {
			part, ok := pics[filepath.Base(name)]
			if !ok {
				return bad("no picture named %s was handed over", name)
			}
			used[filepath.Base(name)] = true
			buf.Write(part)
			return nil
		}
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSON(buf, k)
			buf.WriteByte(':')
			buf.Write(vals[i])
		}
		buf.WriteByte('}')
		return nil
	default:
		writeJSON(buf, t)
		return nil
	}
}

// writeJSON writes a string, number, bool or null as the caller wrote it (no HTML escaping).
func writeJSON(buf *bytes.Buffer, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	buf.Write(bytes.TrimRight(b.Bytes(), "\n"))
}
