package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// A send with files is put to a person with the files named on the card and each one's sha256
// bound: what they approve is those bytes (slack.SendFiles refuses a file changed after).
func TestSendFilesApprovalCard(t *testing.T) {
	h := t.TempDir()
	os.MkdirAll(filepath.Join(h, "out"), 0o755)
	os.WriteFile(filepath.Join(h, "out", "cover.jpg"), []byte("cover"), 0o644)
	rt := &runtime{sched: sched.New(sched.Options{Home: h})}
	env := &tools.Env{Home: h, Sandbox: sandbox.Sandbox{Home: h}}
	b, _ := json.Marshal(map[string]any{"target": "slack:C0OTHER001", "message": "the cover", "files": []string{"out/cover.jpg"}})
	card, bind, err := rt.describeBound(env, messaging.SendTool, b)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := slack.SendFiles(t.Context(), env, []string{"out/cover.jpg"})
	if bind[slack.BindFile+"out/cover.jpg"] != files[0].SHA256 || !strings.Contains(card.Title, "1 file") ||
		!strings.Contains(card.Payload, "cover.jpg") || !strings.Contains(card.Payload, files[0].SHA256[:12]) {
		t.Errorf("card %+v, bind %v", card, bind)
	}
	b, _ = json.Marshal(map[string]any{"target": "slack:C0OTHER001", "files": []string{"state/x.db"}})
	if _, _, err := rt.describeBound(env, messaging.SendTool, b); err == nil {
		t.Error("a card for a file the agent may not read")
	}
}
