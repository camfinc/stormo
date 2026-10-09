package loop

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// End-to-end: build → rehydrate → agent learns → nap → dream → review → rebuild → rehydrate.

type spyStore struct {
	Store
	reads []string
}

func (s *spyStore) Get(k string) ([]byte, error) {
	s.reads = append(s.reads, k)
	return s.Store.Get(k)
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyTree(src, dst, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

func TestLearningLoop(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	example := "../../examples/minimal"
	copyDir(t, filepath.Join(example, "units"), filepath.Join(root, "units"))
	copyDir(t, filepath.Join(example, "agents", "atlas"), filepath.Join(root, "agents", "atlas"))
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	for id, unit := range map[string]string{"ann": "sales", "ben": "support"} {
		write(t, filepath.Join(root, "agents", id, "agent.yaml"), "id: "+id+"\nname: "+id+"\nunit: "+unit+"\nrole: test\nengine: {kind: hermes, version: 0.21.5, model: x/y}\n")
		write(t, filepath.Join(root, "agents", id, "SOUL.md"), "# "+id+"\n")
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	eng := hermes.New(inst)
	store := FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	home1, home2 := filepath.Join(tmp, "home1"), filepath.Join(tmp, "home2")
	agent := func() *manifest.Agent {
		a, err := manifest.Load(root, "atlas", inst.Names.Secret)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	baseline := func(name string) string {
		r, err := build.Agent(inst, "atlas", build.Options{Out: filepath.Join(tmp, name)})
		if err != nil {
			t.Fatal(err)
		}
		return r.Out
	}
	rehydrate := func(home, base string) *RehydrateReport {
		r, err := Rehydrate(RehydrateOptions{Agent: agent(), Engine: eng, Home: home, BaselineDir: base, Store: store, Scrubber: scrubber})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("first boot: rehydrate from baseline with an empty store", func(t *testing.T) {
		r := rehydrate(home1, baseline("baseline"))
		if r.Nap != nil {
			t.Fatalf("nap = %v", *r.Nap)
		}
		for _, p := range []string{"SOUL.md", "skills/crm/crm-api/SKILL.md", "acme-state/news-radar"} {
			if !exists(filepath.Join(home1, p)) {
				t.Errorf("missing %s", p)
			}
		}
		if read(t, filepath.Join(home1, ".env")) != "" {
			t.Error(".env not empty")
		}
	})

	t.Run("agent learns at runtime, nap captures it once", func(t *testing.T) {
		h := home1
		write(t, filepath.Join(h, "memories/MEMORY.md"), strings.Join([]string{"Dana prefers deals grouped by close date.", "Client John Doe phone +1 415 555 0123 wants morning calls."}, "\n§\n")) // Hermes' memory format
		write(t, filepath.Join(h, "memories/USER.md"), "The owner wants voice replies in DMs.")
		skill := filepath.Join(h, "skills/crm/crm-api/SKILL.md")
		write(t, skill, read(t, skill)+"\n## Learned\nUse orderBy=segmentDate for check-in sweeps.\n")
		write(t, filepath.Join(h, "skills/crm/order-lookup/SKILL.md"), "---\nname: order-lookup\ndescription: find a deal by order id\n---\n# Order lookup\nAsk for order AB12CD34 first.\n")
		write(t, filepath.Join(h, "skills/.bundled_manifest"), "airtable:abc\n")
		write(t, filepath.Join(h, "skills/productivity/airtable/SKILL.md"), "---\nname: airtable\ndescription: bundled\n---\nbody\n")
		write(t, filepath.Join(h, "logs/agent.log"), "noise")

		radar, err := sql.Open("sqlite", filepath.Join(h, "acme-state/news-radar/news_radar.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"create table seen(id text)", "insert into seen values ('post-1')"} {
			if _, err := radar.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		radar.Close()
		sessions, err := sql.Open("sqlite", filepath.Join(h, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		sessions.SetMaxOpenConns(1)
		for _, q := range []string{"pragma journal_mode=wal", "create table messages(body text)", "insert into messages values ('client: my card is 4111 1111 1111 1111')"} {
			if _, err := sessions.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		// left open on purpose: the snapshot must work while the engine holds the DB
		opts := NapOptions{Agent: "atlas", Unit: "sales", Engine: eng, Home: h, Store: store, Instance: "task-a", BaselineDir: filepath.Join(tmp, "baseline"), Now: time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)}
		m, err := NapOnce(opts)
		sessions.Close()
		if err != nil || m == nil {
			t.Fatalf("nap: %v %v", m, err)
		}
		byPath := map[string]engine.FileClass{}
		for _, f := range m.Files {
			byPath[f.Path] = f.Class
		}
		want := map[string]engine.FileClass{"state.db": engine.Raw, "acme-state/news-radar/news_radar.db": engine.State, "memories/MEMORY.md": engine.Learning}
		for p, c := range want {
			if byPath[p] != c {
				t.Errorf("%s: class %q, want %q", p, byPath[p], c)
			}
		}
		if _, ok := byPath["logs/agent.log"]; ok {
			t.Error("logs captured")
		}
		opts.Now = opts.Now.Add(15 * time.Minute)
		if again, err := NapOnce(opts); err != nil || again != nil {
			t.Errorf("second nap = %v, %v; want nothing new", again, err)
		}
	})

	t.Run("dream proposes scrubbed learnings and never reads raw history", func(t *testing.T) {
		spy := &spyStore{Store: store}
		r, err := Dream(inst, "atlas", spy)
		if err != nil {
			t.Fatal(err)
		}
		if r.NewLearnings != 3 {
			t.Errorf("new learnings = %d", r.NewLearnings)
		}
		for _, k := range spy.reads {
			if strings.Contains(k, "/raw/") {
				t.Errorf("dream read %s", k)
			}
		}
		ledger, _ := learning.LoadLedger(root, "atlas")
		body, _ := learning.MarshalCompact(ledger)
		if strings.Contains(string(body), "555 0123") {
			t.Error("phone leaked into the ledger")
		}
		var phone *learning.Learning
		for _, e := range ledger {
			if strings.Contains(e.Text, "[PHONE]") {
				phone = e
			}
		}
		if phone == nil || !slices.Contains(phone.PII, "phone") || phone.Status != learning.Proposed {
			t.Fatalf("phone entry = %+v", phone)
		}
		props, _ := ListSkillProposals(root, "atlas")
		got := []string{}
		for _, p := range props {
			got = append(got, p.Skill)
		}
		if !slices.Equal(got, []string{"crm/crm-api", "crm/order-lookup"}) {
			t.Errorf("skill proposals = %v", got)
		}
		if !strings.Contains(read(t, filepath.Join(root, "agents/atlas/learnings/proposals/skills/crm/order-lookup/SKILL.md")), "[ORDER-ID]") {
			t.Error("order id not scrubbed in the proposal")
		}
		if again, _ := Dream(inst, "atlas", store); len(again.Naps) != 0 {
			t.Errorf("second dream folded %v", again.Naps)
		}
	})

	t.Run("review: accept, reject, promotion guards and unit isolation", func(t *testing.T) {
		ledger, _ := learning.LoadLedger(root, "atlas")
		var clean, phone, user *learning.Learning
		for _, e := range ledger {
			switch {
			case strings.HasPrefix(e.Text, "Dana"):
				clean = e
			case len(e.PII) > 0:
				phone = e
			case e.Kind == learning.KindUser:
				user = e
			}
		}
		accepted, rejected, unit := learning.Accepted, learning.Rejected, manifest.ScopeUnit
		if _, err := Decide(root, "atlas", []string{clean.ID, user.ID}, Change{Status: &accepted}); err != nil {
			t.Fatal(err)
		}
		if _, err := Decide(root, "atlas", []string{phone.ID}, Change{Status: &rejected}); err != nil {
			t.Fatal(err)
		}
		if _, err := Decide(root, "atlas", []string{user.ID}, Change{Scope: &unit}); err == nil {
			t.Error("promoted a user-profile memory")
		} else if _, ok := err.(*learning.PromotionError); !ok {
			t.Errorf("err = %v", err)
		}
		if _, err := Decide(root, "atlas", []string{clean.ID}, Change{Scope: &unit}); err != nil {
			t.Fatal(err)
		}
		ann, _ := learning.VisibleLearnings(inst, "ann")
		if len(ann) != 1 || ann[0].ID != clean.ID {
			t.Errorf("ann sees %v", ann)
		}
		if ben, _ := learning.VisibleLearnings(inst, "ben"); len(ben) != 0 {
			t.Errorf("ben (other unit) sees %v", ben)
		}
		if _, err := DecideSkill(root, "atlas", "crm/order-lookup", "accepted"); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(root, "agents/atlas/skills/crm/order-lookup/SKILL.md")) {
			t.Error("accepted skill not copied")
		}
	})

	t.Run("second boot: rejected memory purged, state restored, unreviewed patch kept", func(t *testing.T) {
		r := rehydrate(home2, baseline("baseline2"))
		mem := read(t, filepath.Join(home2, "memories/MEMORY.md"))
		if !strings.Contains(mem, "Dana prefers") || strings.Contains(mem, "555 0123") || r.PurgedMemories != 1 {
			t.Errorf("memory = %q, purged %d", mem, r.PurgedMemories)
		}
		// crm-api was not changed in the repo, so the runtime patch survives the restart.
		if !strings.Contains(read(t, filepath.Join(home2, "skills/crm/crm-api/SKILL.md")), "orderBy=segmentDate for check-in") || !slices.Contains(r.RestoredSkills, "crm/crm-api") {
			t.Errorf("runtime patch not restored: %v", r.RestoredSkills)
		}
		// Bundled skills belong to the image; the manifest is not restored so Hermes re-seeds them.
		if exists(filepath.Join(home2, "skills/productivity/airtable")) || exists(filepath.Join(home2, "skills/.bundled_manifest")) {
			t.Error("bundled skill or manifest restored")
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(home2, "acme-state/news-radar/news_radar.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := db.QueryRow("select id from seen").Scan(&id); err != nil || id != "post-1" {
			t.Errorf("radar row = %q, %v", id, err)
		}
		db.Close()
		if !exists(filepath.Join(home2, "state.db")) {
			t.Error("state.db not restored")
		}
		if !strings.Contains(read(t, filepath.Join(home2, "skills/acme-knowledge/references/agent.md")), "Dana prefers") {
			t.Error("accepted lesson missing from the knowledge skill")
		}
	})

	t.Run("a repo change to a skill wins over the stale runtime patch", func(t *testing.T) {
		repoSkill := filepath.Join(root, "agents/atlas/skills/crm/crm-api/SKILL.md")
		write(t, repoSkill, read(t, repoSkill)+"\n## Reviewed\nRepo edit.\n")
		h := filepath.Join(tmp, "home3")
		r := rehydrate(h, baseline("baseline3"))
		body := read(t, filepath.Join(h, "skills/crm/crm-api/SKILL.md"))
		if !slices.Contains(r.KeptRepoSkills, "crm/crm-api") || !strings.Contains(body, "Repo edit.") || strings.Contains(body, "orderBy=segmentDate for check-in") {
			t.Errorf("kept=%v body=%q", r.KeptRepoSkills, body)
		}
	})
}

func TestChownTree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "memories/MEMORY.md"), "x")
	seen := []string{}
	n, err := ChownTree(dir, 10000, 10000, func(p string, u, g int) error {
		rel := strings.TrimPrefix(p, dir)
		if rel == "" {
			rel = "/"
		}
		seen = append(seen, rel)
		return nil
	})
	if err != nil || n != 3 || !slices.Equal(seen, []string{"/", "/memories", "/memories/MEMORY.md"}) {
		t.Errorf("n=%d seen=%v err=%v", n, seen, err)
	}
}

func TestLocalStoreLivesInTheAgentsFolder(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, ".swarm", "store", "atlas")
	write(t, filepath.Join(legacy, "latest.json"), `{"nap":"n1"}`)
	s := LocalStore{Root: root}
	// Until it moves, the format-0 place is read (and written).
	if b, _ := s.Get("atlas/latest.json"); string(b) != `{"nap":"n1"}` {
		t.Fatalf("legacy read: %q", b)
	}
	if moved, err := MigrateLocalStore(root, "atlas"); err != nil || !moved {
		t.Fatal(moved, err)
	}
	if b, _ := s.Get("atlas/latest.json"); string(b) != `{"nap":"n1"}` {
		t.Fatalf("after the move: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "atlas", "data", "store", "latest.json")); err != nil {
		t.Fatal(err)
	}
	if gi, _ := os.ReadFile(filepath.Join(root, "agents", "atlas", "data", ".gitignore")); !strings.Contains(string(gi), "*") {
		t.Error("data/ is not ignored by git")
	}
	// A new agent's store starts in its folder; keys keep their <agent>/ prefix.
	if err := s.Put("nova/naps/n2.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List("nova/naps/"); !slices.Equal(keys, []string{"nova/naps/n2.json"}) {
		t.Errorf("list = %v", keys)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "nova", "data", "store", "naps", "n2.json")); err != nil {
		t.Error(err)
	}
	if moved, err := MigrateLocalStore(root, "nova"); err != nil || moved {
		t.Errorf("nothing to move: %v %v", moved, err)
	}
}
