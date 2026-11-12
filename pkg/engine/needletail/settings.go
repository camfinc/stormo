package needletail

import (
	"fmt"

	"github.com/camfinc/stormo/internal/needletail/voice"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The settings a needletail agent changes at runtime, all in voice.SettingsFile (the runtime
// writes it; Compile does not, so a boot restores it from the nap and the agent keeps its pick):
//   - voice.id, voice.name: the voice it picked for itself (the voice tool's choose), when its
//     agent.yaml voice: names none. Opaque: a provider's id and catalog name, not text the
//     scrubber should read. Accepting one writes agent.yaml voice.id (voice.name).
const (
	voiceIDKey   = "voice.id"
	voiceNameKey = "voice.name"
)

// Settings reads the runtime-changeable settings from a settings file.
func (n *Needletail) Settings(config []byte) ([]engine.Setting, error) {
	p, err := voice.ParseSettings(config)
	if err != nil || p == nil {
		return []engine.Setting{}, err
	}
	out := []engine.Setting{{Key: voiceIDKey, Value: p.Voice, Opaque: true}}
	if p.Name != "" {
		out = append(out, engine.Setting{Key: voiceNameKey, Value: p.Name, Opaque: true})
	}
	return out, nil
}

// RepoSettings are the same settings as agent.yaml sets them now.
func (n *Needletail) RepoSettings(a *manifest.Agent, _ string) ([]engine.Setting, error) {
	out := []engine.Setting{}
	if v := a.Voice; v != nil {
		if v.ID != "" {
			out = append(out, engine.Setting{Key: voiceIDKey, Value: v.ID, Opaque: true})
		}
		if v.Name != "" {
			out = append(out, engine.Setting{Key: voiceNameKey, Value: v.Name, Opaque: true})
		}
	}
	return out, nil
}

// SettingEdit: a picked voice goes to agent.yaml voice: (which an agent with a voice has).
func (n *Needletail) SettingEdit(a *manifest.Agent, s engine.Setting) (*engine.SettingEdit, error) {
	if a.Voice == nil {
		return nil, fmt.Errorf("agents/%s: agent.yaml has no voice: for %s", a.ID, s.Key)
	}
	switch s.Key {
	case voiceIDKey:
		return &engine.SettingEdit{File: "agent.yaml", Path: []string{"voice", "id"}, Value: s.Value}, nil
	case voiceNameKey:
		return &engine.SettingEdit{File: "agent.yaml", Path: []string{"voice", "name"}, Value: s.Value}, nil
	}
	return nil, fmt.Errorf("%s is not a setting needletail writes back", s.Key)
}

var _ engine.RuntimeSettings = (*Needletail)(nil)
