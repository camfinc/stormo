package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/elevenlabs"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// voiceInstance is examples/minimal with a voice connection to base keyed by TEST_VOICE_KEY (its
// shared value set when key is not empty), and atlas speaking with voice Val1 through it.
func voiceInstance(t *testing.T, base, key string) *instance.Instance {
	t.Helper()
	root := t.TempDir()
	src := "../../examples/minimal"
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	add := func(rel, text string) {
		f, err := os.OpenFile(filepath.Join(root, rel), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	add("stormo.yaml", "\nconnections:\n  - {name: test-voice, kind: elevenlabs, base_url: \""+base+"\", key: TEST_VOICE_KEY}\n")
	add("agents/atlas/agent.yaml", "\nvoice: {provider: test-voice, id: Val1, name: Ada}\n")
	if key != "" {
		if err := os.WriteFile(filepath.Join(root, "secrets.local.yaml"), []byte("shared:\n  TEST_VOICE_KEY: "+key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// connections voices lists the account's voices with the shared key, each with the agents that
// speak with it; a model connection or an unset key is refused before anything is asked.
func TestConnectionVoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "k-test" || r.URL.Path != "/v2/voices" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"voices":[{"voice_id":"Val1","name":"Ada","labels":{"gender":"female"}},{"voice_id":"Ale2","name":"Ben","labels":{"gender":"male"}}]}`))
	}))
	defer srv.Close()
	inst := voiceInstance(t, srv.URL, "k-test")
	rows, err := connectionVoices(inst, "test-voice", "", false, elevenlabs.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !slices.Equal(rows[0].UsedBy, []string{"atlas"}) || rows[1].UsedBy == nil || len(rows[1].UsedBy) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	if rows, err := connectionVoices(inst, "test-voice", "", false, elevenlabs.Query{Gender: "male"}); err != nil || len(rows) != 1 || rows[0].ID != "Ale2" {
		t.Errorf("male = %+v %v", rows, err)
	}
	if _, err := voiceClient(inst, "openrouter", ""); err == nil || !strings.Contains(err.Error(), "serves models") {
		t.Errorf("a model connection: %v", err)
	}
	if _, err := voiceClient(voiceInstance(t, srv.URL, ""), "test-voice", ""); err == nil || !strings.Contains(err.Error(), "TEST_VOICE_KEY has no value") {
		t.Errorf("no key: %v", err)
	}
}

// voice show is agent.yaml's voice and whether its key has a value; voice try speaks a sample in
// it (the agent introducing itself unless a text is given) and saves it under .swarm/voice/.
func TestVoiceShowAndTry(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("character-cost", "30")
		_, _ = w.Write([]byte("ID3 sample"))
	}))
	defer srv.Close()
	inst := voiceInstance(t, srv.URL, "k-test")
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	show, err := voiceShow(inst, a)
	if err != nil || show.Voice == nil || show.Voice.ID != "Val1" || !show.KeySet || show.Proposed != nil {
		t.Fatalf("show = %+v %v", show, err)
	}
	r, err := voiceTry(inst, a, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.Path)
	if string(b) != "ID3 sample" || r.Chars != 30 || !strings.HasPrefix(r.Path, filepath.Join(inst.Root, ".swarm", "voice")) ||
		!strings.Contains(r.Text, "Atlas") || !strings.HasPrefix(asked, "/v1/text-to-speech/Val1?") {
		t.Errorf("try = %+v (asked %s)", r, asked)
	}
	if _, err := voiceTry(inst, a, strings.Repeat("x", 301)); err == nil {
		t.Error("an overlong sample spoken")
	}
	a.Voice.ID = ""
	if _, err := voiceTry(inst, a, ""); err == nil || !strings.Contains(err.Error(), "no voice chosen") {
		t.Errorf("no voice: %v", err)
	}
}
