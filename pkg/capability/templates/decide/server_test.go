package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var isBug = map[string]json.RawMessage{"is_bug": json.RawMessage(`{"type":"noul","instructions":"A defect?","criteria":{"true":"broken","false":"a question"}}`)}

// fakeAPI answers like the Decisions API; fail picks a status for a request's state (0: answer).
func fakeAPI(t *testing.T, fail func(state string) int) (*httptest.Server, *[]map[string]any, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var got []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k-test" {
			http.Error(w, "no key", 401)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		state, _ := json.Marshal(body["state"])
		if code := fail(string(state)); code != 0 {
			http.Error(w, `{"error":{"code":`+http.StatusText(code)+`}}`, code)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": body["model"].(string) + "-20261014",
			"answers": map[string]any{"is_bug": map[string]any{"type": "noul", "noul": 0.96}},
			"usage":   map[string]any{"input_tokens": 400, "output_tokens": 20, "cost": 0.00002}})
	}))
	t.Cleanup(api.Close)
	t.Setenv("DECISIONS_API_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", "k-test")
	return api, &got, &mu
}

func TestOneStateIsAskedWithThePinnedModel(t *testing.T) {
	_, got, _ := fakeAPI(t, func(string) int { return 0 })
	res, err := run(context.Background(), input{Questions: isBug, State: json.RawMessage(`{"ticket":"blank checkout page"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0]["model"] != defaultModel || (*got)[0]["state"].(map[string]any)["ticket"] != "blank checkout page" {
		t.Fatalf("request %v", *got)
	}
	if _, ok := (*got)[0]["questions"].(map[string]any)["is_bug"]; !ok {
		t.Fatalf("questions not passed on: %v", (*got)[0])
	}
	var answers map[string]map[string]any
	json.Unmarshal(res["answers"].(json.RawMessage), &answers)
	if answers["is_bug"]["noul"] != 0.96 || res["model"] != defaultModel+"-20261014" || *res["cost_usd"].(*float64) != 0.00002 {
		t.Fatalf("answer %v", res)
	}
}

func TestABatchSharesTheQuestionsAndKeepsWhatWasDecided(t *testing.T) {
	_, got, _ := fakeAPI(t, func(state string) int {
		if strings.Contains(state, "broken") {
			return 400
		}
		return 0
	})
	res, err := run(context.Background(), input{Model: "vendor/other-1", Questions: isBug, States: map[string]json.RawMessage{
		"a": json.RawMessage(`"one"`), "b": json.RawMessage(`"two"`), "c": json.RawMessage(`"broken"`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 3 || (*got)[0]["model"] != "vendor/other-1" {
		t.Fatalf("requests %v", *got)
	}
	results := res["results"].(map[string]any)
	if res["decided"] != 2 || res["failed"] != 1 || res["cost_usd"].(float64) < 0.00003 {
		t.Fatalf("batch %v", res)
	}
	if e, _ := results["c"].(map[string]any)["error"].(string); !strings.Contains(e, "400") {
		t.Fatalf("c: %v", results["c"])
	}
	if _, ok := results["a"].(map[string]any)["answers"]; !ok {
		t.Fatalf("a: %v", results["a"])
	}
}

func TestABatchWithNothingDecidedFails(t *testing.T) {
	fakeAPI(t, func(string) int { return 402 })
	_, err := run(context.Background(), input{Questions: isBug, States: map[string]json.RawMessage{"a": json.RawMessage(`"x"`)}})
	if err == nil || !strings.Contains(err.Error(), "402") {
		t.Fatalf("err %v", err)
	}
}

func TestBusyAnswersAreRetriedAndRefusalsAreNot(t *testing.T) {
	backoff = []time.Duration{time.Millisecond, time.Millisecond}
	calls := 0
	fakeAPI(t, func(string) int {
		calls++
		if calls == 1 {
			return 503
		}
		return 0
	})
	if _, err := run(context.Background(), input{Questions: isBug, State: json.RawMessage(`"x"`)}); err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	calls = 0
	fakeAPI(t, func(string) int { calls++; return 402 })
	if _, err := run(context.Background(), input{Questions: isBug, State: json.RawMessage(`"x"`)}); err == nil || calls != 1 {
		t.Fatalf("a 402 was asked %d times (err %v)", calls, err)
	}
}

func TestBadInputIsRefusedBeforeAnyCall(t *testing.T) {
	_, got, _ := fakeAPI(t, func(string) int { return 0 })
	q := func(s string) map[string]json.RawMessage { return map[string]json.RawMessage{"q": json.RawMessage(s)} }
	for name, in := range map[string]input{
		"no questions":      {State: json.RawMessage(`"x"`)},
		"no state":          {Questions: isBug},
		"both":              {Questions: isBug, State: json.RawMessage(`"x"`), States: map[string]json.RawMessage{"a": json.RawMessage(`"y"`)}},
		"empty state":       {Questions: isBug, State: json.RawMessage(`{}`)},
		"unknown type":      {Questions: q(`{"type":"maybe"}`), State: json.RawMessage(`"x"`)},
		"one-option choice": {Questions: q(`{"type":"choice","criteria":{"a":"only"}}`), State: json.RawMessage(`"x"`)},
		"one-level score":   {Questions: q(`{"type":"score","criteria":["low"]}`), State: json.RawMessage(`"x"`)},
		"box, no picture":   {Questions: q(`{"type":"box","instructions":"the logo"}`), State: json.RawMessage(`"x"`)},
		"bad name":          {Questions: map[string]json.RawMessage{"a b": isBug["is_bug"]}, State: json.RawMessage(`"x"`)},
		"big state":         {Questions: isBug, State: json.RawMessage(`"` + strings.Repeat("x", maxStateBytes) + `"`)},
	} {
		_, err := run(context.Background(), in)
		var je jobError
		if err == nil || !errorsAs(err, &je) || je.code != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("bad input reached the API: %v", *got)
	}
}

func errorsAs(err error, je *jobError) bool {
	e, ok := err.(jobError)
	*je = e
	return ok
}

// picture writes a w×h PNG (or JPEG) into dir and answers its path.
func picture(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".jpg") {
		jpeg.Encode(&buf, img, nil)
	} else {
		png.Encode(&buf, img)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sent decodes the picture an image_url part carries: its type and size.
func sent(t *testing.T, part any) (string, int, int) {
	t.Helper()
	url := part.(map[string]any)["image_url"].(map[string]any)["url"].(string)
	mime, data, _ := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,")
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return mime, cfg.Width, cfg.Height
}

func TestPicturesGoWhereTheyAreNamedFittedToTheBounds(t *testing.T) {
	_, got, _ := fakeAPI(t, func(string) int { return 0 })
	dir := t.TempDir()
	frame := picture(t, dir, "frame.png", 1080, 1920) // a story frame: too tall for the API
	small := picture(t, dir, "thumb.png", 400, 300)   // fits as it is
	icon := picture(t, dir, "icon.jpg", 64, 128)      // too small: scaled up
	qs := map[string]json.RawMessage{
		"same":  json.RawMessage(`{"type":"noul","instructions":["Is the person in","the cover",{"image":"thumb.png"},"the one in ` + "`frame`" + `?"]}`),
		"face":  json.RawMessage(`{"type":"box","instructions":"the face of the woman in ` + "`frame`" + `"}`),
		"which": json.RawMessage(`{"type":"choice","instructions":"Which reads best?","criteria":{"a":{"image":"icon.jpg"},"b":"the plain one"}}`),
	}
	if _, err := run(context.Background(), input{Questions: qs, State: json.RawMessage(`{"hook":"Maiquetía hoy","frame":{"image":"frame.png"},"n":1.50}`)}, frame, small, icon); err != nil {
		t.Fatal(err)
	}
	body := (*got)[0]
	state := body["state"].(map[string]any)
	if mime, w, h := sent(t, state["frame"]); mime != "image/jpeg" || w != 882 || h != 1568 {
		t.Fatalf("frame sent as %s %dx%d, want a JPEG fitted to 882x1568", mime, w, h)
	}
	if state["hook"] != "Maiquetía hoy" || state["n"] != 1.5 {
		t.Fatalf("the rest of the state changed: %v", state)
	}
	q := body["questions"].(map[string]any)
	if mime, w, h := sent(t, q["same"].(map[string]any)["instructions"].([]any)[2]); mime != "image/png" || w != 400 || h != 300 {
		t.Fatalf("a picture that fits goes as it is, got %s %dx%d", mime, w, h)
	}
	if _, w, h := sent(t, q["which"].(map[string]any)["criteria"].(map[string]any)["a"]); w != 256 || h != 512 {
		t.Fatalf("a small picture is scaled up to the 256 px floor, got %dx%d", w, h)
	}
	if q["face"].(map[string]any)["type"] != "box" {
		t.Fatalf("box not passed on: %v", q["face"])
	}
}

func TestExpandKeepsTheOrderOfWhatItDoesNotTouch(t *testing.T) {
	pics := map[string]json.RawMessage{"a.png": json.RawMessage(`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}`)}
	used := map[string]bool{}
	out, err := expand(json.RawMessage(`{"z":1,"a":[{"image":"a.png"},{"image":"a.png","alt":"x"}],"m":"<b>"}`), pics, used)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z":1,"a":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},{"image":"a.png","alt":"x"}],"m":"<b>"}`
	if string(out) != want || !used["a.png"] {
		t.Fatalf("expand\n got %s\nwant %s", out, want)
	}
}

func TestPictureMistakesAreRefusedBeforeAnyCall(t *testing.T) {
	_, got, _ := fakeAPI(t, func(string) int { return 0 })
	dir := t.TempDir()
	frame := picture(t, dir, "frame.png", 300, 300)
	other := picture(t, filepath.Join(dir), "other.png", 300, 300)
	sub := filepath.Join(dir, "b")
	os.Mkdir(sub, 0o755)
	twin := picture(t, sub, "frame.png", 300, 300)
	webp := filepath.Join(dir, "x.webp")
	os.WriteFile(webp, []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), 0o644)
	thin := picture(t, dir, "thin.png", 2000, 100)
	named := json.RawMessage(`{"photo":{"image":"frame.png"}}`)
	box := map[string]json.RawMessage{"face": json.RawMessage(`{"type":"box","instructions":"the face in photo"}`)}
	for name, c := range map[string]struct {
		in    input
		files []string
	}{
		"box without a picture": {input{Questions: box, State: json.RawMessage(`"x"`)}, nil},
		"box with criteria":     {input{Questions: map[string]json.RawMessage{"f": json.RawMessage(`{"type":"box","criteria":{"true":"a"}}`)}, State: named}, []string{frame}},
		"never named":           {input{Questions: isBug, State: named}, []string{frame, other}},
		"named, not handed":     {input{Questions: isBug, State: json.RawMessage(`{"photo":{"image":"nope.png"}}`)}, []string{frame}},
		"same name twice":       {input{Questions: isBug, State: named}, []string{frame, twin}},
		"not png or jpeg":       {input{Questions: isBug, State: json.RawMessage(`{"photo":{"image":"x.webp"}}`)}, []string{webp}},
		"too long and thin":     {input{Questions: isBug, State: json.RawMessage(`{"photo":{"image":"thin.png"}}`)}, []string{thin}},
	} {
		_, err := run(context.Background(), c.in, c.files...)
		var je jobError
		if err == nil || !errors.As(err, &je) || je.code != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("bad input reached the API: %d calls", len(*got))
	}
}
