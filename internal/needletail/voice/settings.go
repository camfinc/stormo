package voice

import (
	"encoding/json"
	"errors"
	"regexp"
)

// settingsV1 is SettingsFile's format, version 1 (a persisted contract: the dream reads it).
type settingsV1 struct {
	Version int   `json:"version"`
	Voice   *Pick `json:"voice,omitempty"`
}

var voiceIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// ParseSettings reads SettingsFile's body: the voice in it, nil when it has none.
func ParseSettings(b []byte) (*Pick, error) {
	var s settingsV1
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 {
		return nil, errors.New("not settings version 1")
	}
	if s.Voice == nil || s.Voice.Voice == "" {
		return nil, nil
	}
	if !voiceIDRe.MatchString(s.Voice.Voice) {
		return nil, errors.New("the voice is not a voice id")
	}
	return s.Voice, nil
}

// MarshalSettings is SettingsFile's body holding p.
func MarshalSettings(p *Pick) ([]byte, error) {
	return json.MarshalIndent(settingsV1{Version: 1, Voice: p}, "", "  ")
}
