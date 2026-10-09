package shared

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestSharedDocsScriptCreatesAndListsDocuments(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	files := Skill(inst, a)
	skill := files["acme-shared-docs/SKILL.md"]
	if !strings.Contains(skill, "/shared/sales") || !strings.Contains(skill, "${HERMES_SKILL_DIR}/scripts/shared_docs.py") || !strings.Contains(skill, "every Acme agent") {
		t.Errorf("skill text:\n%s", skill)
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "shared_docs.py")
	os.WriteFile(script, []byte(files["acme-shared-docs/scripts/shared_docs.py"]), 0o755)
	root := filepath.Join(tmp, "shared")
	os.MkdirAll(filepath.Join(root, "group"), 0o755)
	os.MkdirAll(filepath.Join(root, "sales"), 0o755)
	run := func(stdin string, args ...string) (int, string) {
		cmd := exec.Command("python3", append([]string{"-I", script}, args...)...)
		cmd.Env = append(os.Environ(), "SWARM_SHARED_DIR="+root, "SWARM_AGENT=atlas", "SWARM_UNIT=sales")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, strings.TrimSpace(string(out))
	}
	ca, a1 := run("Body text.", "new", "sales", "Q4 Plan", "--title", "Q4 plan", "--tags", "planning,brief")
	_, b1 := run("Second.", "new", "sales", "Q4 Plan", "--title", "Q4 plan v2")
	if ca != 0 || a1 == b1 {
		t.Fatalf("new: %d %q %q", ca, a1, b1)
	}
	body, _ := os.ReadFile(a1)
	if !regexp.MustCompile(`^---\ntitle: "Q4 plan"\nauthor: atlas\nunit: sales\ncreated: .+\ntags: \[planning, brief\]\n---\n\nBody text\.\n$`).Match(body) {
		t.Errorf("document:\n%s", body)
	}
	if _, h := run("Need a banner.", "new", "group", "visual-request", "--title", "Banner for Q4", "--to", "nova"); !strings.Contains(h, filepath.Join("group", "handoffs", "nova")) {
		t.Errorf("handoff path %q", h)
	}
	if c, _ := run("", "new", "support", "x", "--title", "nope"); c == 0 {
		t.Error("wrote into another unit's directory")
	}
	if _, list := run("", "list"); len(strings.Split(list, "\n")) != 3 {
		t.Errorf("list:\n%s", list)
	}
	if _, l := run("", "list", "--to", "nova"); !strings.Contains(l, "Banner for Q4") {
		t.Errorf("list --to: %s", l)
	}
}

func TestLayersAreGroupAndOwnUnitOnly(t *testing.T) {
	l := Layers(&manifest.Agent{Unit: "sales"})
	if len(l) != 2 || l[0].Path != "/shared/group" || l[1].Path != "/shared/sales" {
		t.Errorf("layers = %+v", l)
	}
}

func TestMigrateLocalMovesLayersAndLeavesSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, l := range []string{"group", "sales", "media"} {
		if err := os.MkdirAll(filepath.Join(root, ".swarm", "shared", l, "handoffs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".swarm", "shared", l, "doc.md"), []byte(l), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// media already exists in workdir/: left alone, reported.
	if err := os.MkdirAll(filepath.Join(LocalDir(root), "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved, conflicts, err := MigrateLocal(root)
	if err != nil || strings.Join(moved, ",") != "group,sales" || strings.Join(conflicts, ",") != "media" {
		t.Fatalf("moved %v conflicts %v err %v", moved, conflicts, err)
	}
	if b, _ := os.ReadFile(filepath.Join(LocalDir(root), "sales", "doc.md")); string(b) != "sales" {
		t.Errorf("moved content %q", b)
	}
	// The old path still works (an agent running on the old mount), through the symlink.
	if b, _ := os.ReadFile(filepath.Join(root, ".swarm", "shared", "group", "doc.md")); string(b) != "group" {
		t.Errorf("through the old path %q", b)
	}
	if st, _ := os.Lstat(filepath.Join(root, ".swarm", "shared", "group")); st.Mode()&os.ModeSymlink == 0 {
		t.Error("old path is not a symlink")
	}
	// Idempotent.
	moved, conflicts, err = MigrateLocal(root)
	if err != nil || len(moved) != 0 || strings.Join(conflicts, ",") != "media" {
		t.Errorf("second run: %v %v %v", moved, conflicts, err)
	}
	if _, _, err := MigrateLocal(t.TempDir()); err != nil {
		t.Errorf("nothing to migrate: %v", err)
	}
}

func TestLocalSkillAddsTheWorkdirHelper(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	files := LocalSkill(inst, a)
	skill := files["acme-shared-docs/SKILL.md"]
	if !strings.HasPrefix(skill, Skill(inst, a)["acme-shared-docs/SKILL.md"]) || !strings.Contains(skill, "scripts/workdir.py") || !strings.Contains(skill, "mcp_core_fs_") {
		t.Errorf("skill:\n%s", skill)
	}
	script := files["acme-shared-docs/scripts/workdir.py"]
	if regexp.MustCompile(`(?i)zz[a-z]+zz`).MatchString(script + skill) {
		t.Error("unreplaced marker")
	}
	if _, ok := Skill(inst, a)["acme-shared-docs/scripts/workdir.py"]; ok {
		t.Error("the AWS skill has no workdir helper")
	}

	// Run the helper against a fake core.
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-core-key-0123456789" {
			w.WriteHeader(401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		params := req["params"].(map[string]any)
		var res map[string]any
		switch params["name"] {
		case "fs_lock":
			res = map[string]any{"lock": map[string]any{"path": params["arguments"].(map[string]any)["path"], "kind": "hard", "expires": "2026-10-09T12:00:00.000Z"},
				"warnings": []string{"nova holds a soft lock"}}
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": true,
				"content": []map[string]any{{"type": "text", "text": "locked: atlas holds a hard lock"}}}})
			return
		}
		text, _ := json.Marshal(res)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": false,
			"content": []map[string]any{{"type": "text", "text": string(text)}}, "structuredContent": res}})
	}))
	defer srv.Close()
	dir := t.TempDir()
	py := filepath.Join(dir, "workdir.py")
	if err := os.WriteFile(py, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("python3", append([]string{"-I", py}, args...)...)
		cmd.Dir = "/"
		cmd.Env = append(os.Environ(), "SWARM_CORE_URL="+srv.URL, "SWARM_CORE_KEY=test-core-key-0123456789")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("lock", "/shared/sales/q4.md", "--reason", "drafting")
	if err != nil || !strings.Contains(out, "locked /shared/sales/q4.md (hard)") || !strings.Contains(out, "note: nova holds a soft lock") {
		t.Fatalf("lock: %v\n%s", err, out)
	}
	mu.Lock()
	first := got[0]
	mu.Unlock()
	args := first["params"].(map[string]any)["arguments"].(map[string]any)
	if first["method"] != "tools/call" || args["path"] != "/shared/sales/q4.md" || args["reason"] != "drafting" || args["kind"] != "hard" {
		t.Errorf("request %v", first)
	}
	if out, err := run("unlock", "/shared/sales/q4.md"); err == nil || !strings.Contains(out, "locked: atlas holds") {
		t.Errorf("a refusal exits 1 with the reason: %v\n%s", err, out)
	}
}
