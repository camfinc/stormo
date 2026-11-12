package manifest

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/camfinc/stormo/pkg/instance"
)

// Voice is the agent's voice (agent.yaml voice:): the voice connection it speaks and listens
// through, the provider's voice, and when it uses them. Engine-neutral: each engine renders what
// it supports. Nil when the agent has none.
type Voice struct {
	// Provider is a voice connection (stormo.yaml connections:; the implicit elevenlabs).
	Provider string `json:"provider"`
	// ID is the provider's voice id; empty: none chosen yet (the agent may pick one).
	ID string `json:"id,omitempty"`
	// Name is the voice's display name, kept in step by whoever sets ID.
	Name string `json:"name,omitempty"`
	// Model is the provider's speech model; empty: the runtime's default.
	Model    string         `json:"model,omitempty"`
	Settings *VoiceSettings `json:"settings,omitempty"`
	// Speak is auto (the agent decides when a reply is a voice note) or never.
	Speak string `json:"speak"`
	// Listen transcribes the voice notes people send.
	Listen bool `json:"listen"`
	// Language is the notes' language for listening, an ISO 639 code; empty: detected.
	Language string `json:"language,omitempty"`
	// MaxSeconds is the longest one spoken note may be.
	MaxSeconds int `json:"max_seconds"`
}

// VoiceSettings tune the provider's voice (each 0 to 1; nil: the voice's own).
type VoiceSettings struct {
	Stability  *float64 `json:"stability,omitempty"`
	Similarity *float64 `json:"similarity,omitempty"`
}

// Voice speak values.
const (
	SpeakAuto  = "auto"
	SpeakNever = "never"
)

// Bounds of voice.max_seconds, and its default.
const (
	DefaultVoiceSeconds = 120
	MinVoiceSeconds     = 10
	MaxVoiceSeconds     = 300
)

var (
	voiceProviderRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	voiceIDRe       = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	voiceModelRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	voiceLanguageRe = regexp.MustCompile(`^[a-z]{2,3}$`)
)

// voiceOf reads voice:, nil when the manifest has none.
func voiceOf(m raw, where string) (*Voice, error) {
	if m.Voice == nil {
		return nil, nil
	}
	known := map[string]bool{"provider": true, "id": true, "name": true, "model": true, "settings": true, "speak": true,
		"listen": true, "language": true, "max_seconds": true}
	for k := range m.Voice {
		if err := req(known[k], "%s: voice: unknown key %q", where, k); err != nil {
			return nil, err
		}
	}
	j := m.Voice
	v := &Voice{Provider: str(j["provider"]), ID: str(j["id"]), Name: str(j["name"]), Model: str(j["model"]), Speak: str(j["speak"]),
		Language: str(j["language"]), Listen: true, MaxSeconds: DefaultVoiceSeconds}
	if v.Provider == "" {
		v.Provider = instance.KindElevenLabs
	}
	if v.Speak == "" {
		v.Speak = SpeakAuto
	}
	okBools := true
	if b := optBool(j["listen"], &okBools); b != nil {
		v.Listen = *b
	}
	checks := []error{
		req(okBools, "%s: voice.listen is true or false", where),
		req(voiceProviderRe.MatchString(v.Provider), "%s: voice.provider %q is a connection name", where, v.Provider),
		req(v.ID == "" || voiceIDRe.MatchString(v.ID), "%s: voice.id %q is not a voice id (letters and digits)", where, v.ID),
		req(utf8.RuneCountInString(v.Name) <= 80 && !strings.ContainsAny(v.Name, "\r\n"), "%s: voice.name is one line of at most 80 characters", where),
		req(v.Model == "" || voiceModelRe.MatchString(v.Model), "%s: voice.model %q is not a model id", where, v.Model),
		req(v.Speak == SpeakAuto || v.Speak == SpeakNever, "%s: voice.speak is %s or %s", where, SpeakAuto, SpeakNever),
		req(v.Language == "" || voiceLanguageRe.MatchString(v.Language), "%s: voice.language %q is an ISO 639-1 or 639-3 code (es, en, spa)", where, v.Language),
	}
	for _, e := range checks {
		if e != nil {
			return nil, e
		}
	}
	if x, ok := j["max_seconds"]; ok && x != nil {
		n, isNum := num(x)
		if err := req(isNum && n == math.Trunc(n) && n >= MinVoiceSeconds && n <= MaxVoiceSeconds,
			"%s: voice.max_seconds must be a whole number of seconds, %d to %d", where, MinVoiceSeconds, MaxVoiceSeconds); err != nil {
			return nil, err
		}
		v.MaxSeconds = int(n)
	}
	if x, ok := j["settings"]; ok && x != nil {
		s, isMap := x.(map[string]any)
		if err := req(isMap, "%s: voice.settings is a map (stability, similarity)", where); err != nil {
			return nil, err
		}
		v.Settings = &VoiceSettings{}
		for k, val := range s {
			var dst **float64
			switch k {
			case "stability":
				dst = &v.Settings.Stability
			case "similarity":
				dst = &v.Settings.Similarity
			default:
				return nil, &Error{fmt.Sprintf("%s: voice.settings: unknown key %q", where, k)}
			}
			n, isNum := num(val)
			if err := req(isNum && n >= 0 && n <= 1, "%s: voice.settings.%s must be 0 to 1", where, k); err != nil {
				return nil, err
			}
			*dst = &n
		}
	}
	return v, nil
}
