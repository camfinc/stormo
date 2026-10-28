package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file store (files/: people's uploads, client data, per conversation) is the runtime's own:
// the file tools neither read, search nor write it; view_file is the way to an upload.
func TestFileStoreIsPrivate(t *testing.T) {
	r, env := setup(t)
	env.Session = "convA"
	_ = os.MkdirAll(filepath.Join(env.Home, "files", "convA"), 0o700)
	_ = os.WriteFile(filepath.Join(env.Home, "files", "convA", "F1-notes.txt"), []byte("uploaded secret\n"), 0o600)
	for _, p := range []string{"files/convA/F1-notes.txt", "Files/convA/F1-notes.txt"} {
		if m, failed := call(t, r, env, "read_file", map[string]any{"path": p}); !failed || !strings.Contains(m["error"].(string), "view_file") {
			t.Errorf("%s read: %v", p, m)
		}
		if m, failed := call(t, r, env, "write_file", map[string]any{"path": p, "content": "x"}); !failed {
			t.Errorf("%s written: %v", p, m)
		}
	}
	if m, _ := call(t, r, env, "search_files", map[string]any{"pattern": "uploaded"}); m["count"] != float64(0) {
		t.Errorf("home search found the store: %v", m)
	}
	if m, failed := call(t, r, env, "search_files", map[string]any{"pattern": "uploaded", "path": "files"}); !failed {
		t.Errorf("files/ searched: %v", m)
	}
}
