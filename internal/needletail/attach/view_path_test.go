package attach

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// view_file path: an image of the agent's own (home or shared space) is shown, under read_file's
// rules; uploads stay reachable only by id, and private runtime data never.
func TestViewToolPath(t *testing.T) {
	home, group := t.TempDir(), t.TempDir()
	s := &Store{Home: home}
	_, _ = s.Save("convA", Entry{ID: "F1", Name: "pic.png", MIME: "image/png", Kind: KindImage}, pngOf(t, 8, 8))
	os.MkdirAll(filepath.Join(group, "renders"), 0o755)
	os.WriteFile(filepath.Join(group, "renders", "sheet.png"), pngOf(t, 40, 20), 0o644)
	os.WriteFile(filepath.Join(group, "renders", "notes.txt"), []byte("text"), 0o644)
	os.MkdirAll(filepath.Join(home, "state"), 0o755)
	os.WriteFile(filepath.Join(home, "state", "x.png"), pngOf(t, 4, 4), 0o644)
	var shown []string
	env := &tools.Env{Session: "convA", Home: home, Sandbox: sandbox.Sandbox{Home: home, Shared: []string{group}},
		Show: func(name, mime string, _ []byte) error { shown = append(shown, name+" "+mime); return nil }, Sees: func() bool { return true }}
	tool := s.Tool(nil)
	call := func(p string) (map[string]any, error) {
		b, _ := json.Marshal(map[string]string{"path": p})
		out, err := tool.Run(context.Background(), env, b)
		if err != nil {
			return nil, err
		}
		m, _ := out.(map[string]any)
		return m, nil
	}
	if out, err := call(filepath.Join(group, "renders", "sheet.png")); err != nil || out["shown"] != true || out["width"] != 40 {
		t.Fatalf("sheet: %v %v", out, err)
	}
	if len(shown) != 1 || !strings.HasSuffix(shown[0], "image/png") {
		t.Errorf("shown: %v", shown)
	}
	if out, err := call(filepath.Join(group, "renders", "notes.txt")); err != nil || out["shown"] != false {
		t.Errorf("text: %v %v", out, err)
	}
	for _, bad := range []string{"files/convA/F1-pic.png", "state/x.png", "/etc/hosts"} {
		if _, err := call(bad); err == nil {
			t.Errorf("%s shown", bad)
		}
	}
	env.Sees = func() bool { return false }
	if out, _ := call(filepath.Join(group, "renders", "sheet.png")); out["shown"] != false {
		t.Errorf("a model without images: %v", out)
	}
}
