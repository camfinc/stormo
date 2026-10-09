package core

import (
	"path/filepath"
	"testing"
)

// The default connection keeps the original files (no migration); another gets its own tokens and
// shares the registration.
func TestChatGPTPaths(t *testing.T) {
	root := "/i"
	def := ChatGPTPaths(root, "chatgpt")
	if def.Connection != filepath.Join(root, ".swarm/core/auth/chatgpt.json") || def != ChatGPTPaths(root, "") {
		t.Errorf("default = %+v", def)
	}
	work := ChatGPTPaths(root, "work-gpt")
	if work.Connection != filepath.Join(root, ".swarm/core/auth/work-gpt/chatgpt.json") || work.Registration != def.Registration {
		t.Errorf("work-gpt = %+v", work)
	}
}
