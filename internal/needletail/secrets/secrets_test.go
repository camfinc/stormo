package secrets

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

func cfg() *wire.Config {
	return &wire.Config{
		Model: wire.ModelConfig{Primary: wire.Endpoint{KeyEnv: "SWARM_CORE_KEY"}, Fallbacks: []wire.Endpoint{{KeyEnv: "OPENROUTER_API_KEY"}}},
		Core:  &wire.CoreConfig{KeyEnv: "SWARM_CORE_KEY"},
		Luau:  wire.LuauConfig{HTTPSecrets: []wire.HTTPSecret{{Host: "api.example.com", Env: "LUAU_ONLY_TOKEN", Header: "Authorization"}}},
	}
}

func TestLoadScrubRedact(t *testing.T) {
	environ := []string{
		"API_SERVER_KEY=api-key-0123456789abcdef", "SWARM_CORE_KEY=core-key-0123456789", "OPENROUTER_API_KEY=sk-or-abcdefghijkl",
		"SLACK_BOT_TOKEN=xoxb-1111-2222", "LUAU_ONLY_TOKEN=luau-secret-value", "CRM_API_TOKEN=acme_abcdefgh123",
		"SWARM_AGENT=atlas", "SHORT_TOKEN=abc", "PATH=/usr/bin",
	}
	s := Load(cfg(), environ)
	if s.Get(APIKey) != "api-key-0123456789abcdef" || s.Get("OPENROUTER_API_KEY") != "sk-or-abcdefghijkl" || s.Get("CRM_API_TOKEN") != "" {
		t.Errorf("values = %v", s.values)
	}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	s.Scrub()
	for _, gone := range []string{"API_SERVER_KEY", "OPENROUTER_API_KEY", "SLACK_BOT_TOKEN"} {
		if _, ok := os.LookupEnv(gone); ok {
			t.Errorf("%s still in env", gone)
		}
	}
	for _, kept := range []string{"SWARM_CORE_KEY", "CRM_API_TOKEN", "SWARM_AGENT", "LUAU_ONLY_TOKEN"} {
		if _, ok := os.LookupEnv(kept); !ok {
			t.Errorf("%s scrubbed", kept)
		}
	}

	r := NewRedactor(s, environ)
	in := "key api-key-0123456789abcdef crm acme_abcdefgh123 core core-key-0123456789 luau luau-secret-value agent atlas short abc"
	out := r.Redact(in)
	for _, leaked := range []string{"api-key-0123456789abcdef", "acme_abcdefgh123", "core-key-0123456789", "luau-secret-value"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %s: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "atlas") || !strings.Contains(out, "short abc") {
		t.Errorf("over-redacted: %s", out)
	}
	r.Add("later-added-secret")
	var buf bytes.Buffer
	_, _ = r.Writer(&buf).Write([]byte("log later-added-secret\n"))
	if buf.String() != "log "+Mask+"\n" {
		t.Errorf("writer = %q", buf.String())
	}
	var nilR *Redactor
	if nilR.Redact("x") != "x" {
		t.Error("nil redactor")
	}
}

// Text that is already JSON (a tool call's arguments, an encoded result) carries a secret in its
// escaped form; that form is masked too, with and without HTML escaping.
func TestRedactJSONForms(t *testing.T) {
	weird := `pa"ss<w>&ord\x` + "\t9"
	r := NewRedactor(nil, []string{"WEIRD_TOKEN=" + weird, "PLAIN_TOKEN=plain-secret-value"})
	html, _ := json.Marshal(map[string]string{"command": "echo " + weird + " plain-secret-value"})
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"command": "echo " + weird})
	for _, in := range []string{string(html), b.String()} {
		out := r.Redact(in)
		if strings.Contains(out, "pa") || strings.Contains(out, "plain-secret") || !strings.Contains(out, Mask) || !json.Valid([]byte(out)) {
			t.Errorf("%s -> %s", in, out)
		}
	}
	if r.Longest() < len(weird) {
		t.Errorf("longest = %d", r.Longest())
	}
}

// Clean is environ without the runtime's own secrets (passthrough and http_secrets envs stay):
// the terminal's env, whatever this process' env holds by then.
func TestClean(t *testing.T) {
	environ := []string{"API_SERVER_KEY=a", "SWARM_CORE_KEY=b", "OPENROUTER_API_KEY=c", "SLACK_APP_TOKEN=d", "LUAU_ONLY_TOKEN=e", "CRM_API_TOKEN=f", "PATH=/bin", "NOEQUALS"}
	got := strings.Join(Load(cfg(), environ).Clean(environ), " ")
	if got != "SWARM_CORE_KEY=b LUAU_ONLY_TOKEN=e CRM_API_TOKEN=f PATH=/bin NOEQUALS" {
		t.Errorf("clean = %s", got)
	}
}

func TestLearningOwnerKeyNeverShared(t *testing.T) {
	c := cfg()
	c.Luau.HTTPSecrets = append(c.Luau.HTTPSecrets, wire.HTTPSecret{Env: LearningKey})
	s := Load(c, []string{LearningKey + "=owner-secret-0123456789"})
	if s.Get(LearningKey) == "" {
		t.Fatal("owner credential unavailable")
	}
	if strings.Contains(strings.Join(s.Clean([]string{LearningKey + "=owner-secret-0123456789"}), " "), LearningKey) {
		t.Fatal("owner credential exposed to command")
	}
}

// With voice notes transcribed, the speech-to-text key is the runtime's: read, redacted, and out of
// the terminal's env; without, it is left alone.
func TestSTTKeyOwned(t *testing.T) {
	environ := []string{"ELEVENLABS_API_KEY=el-key-0123456789abcdef", "PATH=/bin"}
	off := Load(&wire.Config{}, environ)
	if off.Get(STTKey) != "" || len(off.Clean(environ)) != 2 {
		t.Errorf("stt off: owned %q, env %v", off.Get(STTKey), off.Clean(environ))
	}
	on := Load(&wire.Config{STT: &wire.STTConfig{Enabled: true}}, environ)
	if on.Get(STTKey) != "el-key-0123456789abcdef" || len(on.Clean(environ)) != 1 {
		t.Errorf("stt on: owned %q, env %v", on.Get(STTKey), on.Clean(environ))
	}
}

// With a web search provider, its key is the runtime's (read, redacted, out of the terminal's
// env); without one, it is left alone.
func TestSearchKeyOwned(t *testing.T) {
	environ := []string{"BRAVE_SEARCH_API_KEY=brave-key-0123456789", "TAVILY_API_KEY=tvly-key-0123456789", "PATH=/bin"}
	off := Load(&wire.Config{}, environ)
	if off.Get("BRAVE_SEARCH_API_KEY") != "" || len(off.Clean(environ)) != 3 {
		t.Errorf("no provider: env %v", off.Clean(environ))
	}
	on := Load(&wire.Config{Web: &wire.WebConfig{Search: &wire.WebSearchConfig{Provider: "tavily"}}}, environ)
	if on.Get("TAVILY_API_KEY") != "tvly-key-0123456789" || on.Get("BRAVE_SEARCH_API_KEY") != "" || len(on.Clean(environ)) != 2 {
		t.Errorf("tavily: env %v", on.Clean(environ))
	}
}
