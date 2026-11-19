package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"box":               {Questions: q(`{"type":"box","instructions":"the logo"}`), State: json.RawMessage(`"x"`)},
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
