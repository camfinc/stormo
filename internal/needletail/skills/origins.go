package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// OriginsFile is private runtime provenance (Raw snapshot data, never dream input).
// Version 1 is additive: no receipt means unknown ownership, never agent ownership.
const OriginsFile = "state/needletail/skill-origins-v1.json"

type skillOrigin struct {
	Name          string `json:"name"`
	CreatedAt     string `json:"created_at"`
	InitialSHA256 string `json:"initial_sha256"`
}
type skillOrigins struct {
	Version int                    `json:"version"`
	Skills  map[string]skillOrigin `json:"skills"`
}

func (l *Library) readOrigins() (skillOrigins, error) {
	out := skillOrigins{Version: 1, Skills: map[string]skillOrigin{}}
	body, cut, err := l.readRegular(filepath.Join(l.Home, filepath.FromSlash(OriginsFile)), MaxFile)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if cut || json.Unmarshal(body, &out) != nil || out.Version != 1 || out.Skills == nil || len(out.Skills) > 4096 {
		return skillOrigins{}, fmt.Errorf("invalid skill ownership provenance")
	}
	return out, nil
}

func (o skillOrigins) owns(s Skill, e usageEntry) bool {
	r, ok := o.Skills[s.Dir]
	b, err := hex.DecodeString(r.InitialSHA256)
	return ok && r.Name == s.Name && r.CreatedAt != "" && r.CreatedAt == e["created_at"] && err == nil && len(b) == sha256.Size
}

// recordOrigin runs only after Create succeeds, while the Library mutex is held.
// A failed receipt leaves ownership unknown and cannot make a skill disposable.
func (l *Library) recordOrigin(s Skill, content string) {
	o, err := l.readOrigins()
	if err != nil {
		return
	}
	u, err := l.readUsage()
	if err != nil {
		return
	}
	at, _ := u[s.Name]["created_at"].(string)
	if at == "" {
		return
	}
	h := sha256.Sum256([]byte(content))
	o.Skills[s.Dir] = skillOrigin{Name: s.Name, CreatedAt: at, InitialSHA256: hex.EncodeToString(h[:])}
	if len(o.Skills) > 4096 {
		return
	}
	body, err := json.Marshal(o)
	if err == nil && len(body)+1 <= MaxFile {
		_ = fsx.WriteAtomicBelow(l.Home, OriginsFile, l.tmp(), append(body, '\n'), 0o600)
	}
}
