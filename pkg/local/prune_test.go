package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo"
	"github.com/camfinc/stormo/pkg/capability"
	"github.com/camfinc/stormo/pkg/instance"
)

func day(n int) time.Time { return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC) }

func removedNames(rm []PrunedImage) []string {
	var out []string
	for _, im := range rm {
		if len(im.Tags) == 0 {
			out = append(out, im.ID)
		}
		out = append(out, im.Tags...)
	}
	slices.Sort(out)
	return out
}

func TestPlanKeepsWhatIsInUsePinnedCurrentAndRecent(t *testing.T) {
	r := pruneRules{
		kinds: map[string]string{"stormo-sidecar": "stormo-sidecar", "eng": "eng", "registry.example/eng": "eng"},
		keep:  map[string]bool{"stormo-sidecar:s2": true, "eng:pinned": true},
		inUse: map[string]bool{"id-s1": true, "id-e1": true, "id-u1": true},
	}
	images := []pruneImage{
		{id: "id-s1", tags: []string{"stormo-sidecar:s1"}, created: day(1)},                         // a container runs it
		{id: "id-s2", tags: []string{"stormo-sidecar:s2"}, created: day(2)},                         // current
		{id: "id-s3", tags: []string{"stormo-sidecar:s3"}, created: day(3)},                         // nothing needs it
		{id: "id-s4", tags: []string{"stormo-sidecar:s4"}, created: day(4)},                         // the second newest
		{id: "id-s5", tags: []string{"stormo-sidecar:s5"}, created: day(5)},                         // the newest
		{id: "id-x", tags: []string{"stormo-sidecar:x", "mine:latest"}, created: day(1)},            // someone else named it too
		{id: "id-e0", tags: []string{"eng:pinned", "registry.example/eng:pinned"}, created: day(1)}, // pinned (pulled: two names)
		{id: "id-e1", tags: []string{"eng:running"}, created: day(2)},
		{id: "id-e2", tags: []string{"eng:old", "registry.example/eng:old"}, created: day(3)},
		{id: "id-e3", tags: []string{"eng:dev-b"}, created: day(4)},
		{id: "id-e4", tags: []string{"eng:dev-c"}, created: day(5)},
		{id: "id-c1", tags: []string{capability.ImagePrefix + "gone:aaa"}, created: day(1)}, // a template no longer shipped
		{id: "id-c2", tags: []string{capability.ImagePrefix + "gone:bbb"}, created: day(2)},
		{id: "id-c3", tags: []string{capability.ImagePrefix + "gone:ccc"}, created: day(3)},
		{id: "id-u1", labeled: true, created: day(1)},                // replaced by a newer build of its name, still running
		{id: "id-u2", labeled: true, created: day(9)},                // replaced and idle: no rollback for a nameless image
		{id: "id-u3", created: day(1)},                               // nameless and not Stormo's
		{id: "id-o", tags: []string{"golang:1.26"}, created: day(1)}, // not Stormo's
	}
	remove, kept := plan(images, r)
	want := []string{"eng:old", "id-u2", "registry.example/eng:old", capability.ImagePrefix + "gone:aaa", "stormo-sidecar:s3"}
	if got := removedNames(remove); !slices.Equal(got, want) {
		t.Errorf("removed %q, want %q", got, want)
	}
	if kept != 11 {
		t.Errorf("kept %d, want 11 (4 sidecars, 4 engine images, 2 capability images, 1 nameless)", kept)
	}
	for _, im := range remove {
		if im.Tags != nil && im.Kind == "" || im.ID == "id-u2" && im.Kind != "" {
			t.Errorf("kind of %s: %q", im.ID, im.Kind)
		}
	}
}

func TestRepositoryAndParseSize(t *testing.T) {
	for ref, want := range map[string]string{
		"stormo-sidecar:abc":            "stormo-sidecar",
		"registry.example/eng:dev-1":    "registry.example/eng",
		"localhost:5000/eng":            "localhost:5000/eng",
		"localhost:5000/eng:v1":         "localhost:5000/eng",
		"stormo-cap-browser:5e5a9deeb9": "stormo-cap-browser",
	} {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
	for s, want := range map[string]int64{"17.25GB": 17_250_000_000, "512.3kB": 512_300, "0B": 0, "4.764MB": 4_764_000} {
		if got, ok := parseSize(s); !ok || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", s, got, ok, want)
		}
	}
	if _, ok := parseSize("lots"); ok {
		t.Error("parseSize read a size without a number")
	}
}

// pruneDocker replaces docker for PruneImages: what it lists, and every command it was asked.
type pruneDocker struct {
	ls, ps, containers, inspect string
	containersErr               error
	size                        []string // successive `system df` image totals
	calls                       []string
}

func fakePruneDocker(t *testing.T, d *pruneDocker) {
	t.Helper()
	old := dockerOutput
	t.Cleanup(func() { dockerOutput = old })
	dockerOutput = func(argv ...string) (string, error) {
		d.calls = append(d.calls, strings.Join(argv, " "))
		switch strings.Join(argv[:3], " ") {
		case "docker ps -aq":
			return d.ps, nil
		case "docker container inspect":
			return d.containers, d.containersErr
		case "docker image ls":
			return d.ls, nil
		case "docker image inspect":
			return d.inspect, nil
		case "docker system df":
			s := d.size[0]
			d.size = d.size[1:]
			return "Images\t" + s + "\nBuild Cache\t1GB\n", nil
		case "docker image rm":
			return "", nil
		}
		return "", fmt.Errorf("unexpected docker call %q", argv)
	}
}

// exampleInstance is the example instance in a new temporary directory: its agents run needletail
// pinned to dev-2084e89.
func exampleInstance(t *testing.T) *instance.Instance {
	t.Helper()
	root := filepath.Join(t.TempDir(), "acme")
	if err := os.CopyFS(root, stormo.Example()); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestPruneImagesKeepsTheInstancesPinsAndWhatContainersRun(t *testing.T) {
	inst := exampleInstance(t)
	browser := capability.Image("browser")
	rows := [][3]string{
		{"sha256:pin", "needletail", "dev-2084e89"}, // every agent pins it, nothing runs it, and it is the oldest
		{"sha256:pin", "ghcr.io/camfinc/needletail", "dev-2084e89"},
		{"sha256:e1", "needletail", "dev-1111111"},
		{"sha256:e2", "needletail", "dev-2222222"},
		{"sha256:e3", "needletail", "dev-3333333"},
		{"sha256:e4", "needletail", "dev-4444444"},
		{"sha256:old", "stormo-sidecar", "aaaaaaaaaaaa"},
		{"sha256:moved", "stormo-sidecar", "bbbbbbbbbbbb-dirty"}, // a nap runs the build its name had before
		{"sha256:s3", "stormo-sidecar", "cccccccccccc"},
		{"sha256:s4", "stormo-sidecar", "dddddddddddd"},
		{"sha256:cur", repository(browser), strings.TrimPrefix(browser, repository(browser)+":")},
		{"sha256:cap-mid", repository(browser), "111111111111"},
		{"sha256:cap-old", repository(browser), "000000000000"},
		{"sha256:ran", "<none>", "<none>"},
		{"sha256:base", "golang", "<none>"},
		{"sha256:other", "postgres", "17"},
	}
	var ls strings.Builder
	for _, r := range rows {
		ls.WriteString(strings.Join(r[:], "\t") + "\n")
	}
	created := map[string]int{"sha256:pin": 1, "sha256:e1": 2, "sha256:e2": 3, "sha256:e3": 4, "sha256:e4": 5,
		"sha256:old": 1, "sha256:moved": 2, "sha256:s3": 3, "sha256:s4": 4, "sha256:cur": 3, "sha256:cap-mid": 2, "sha256:cap-old": 1,
		"sha256:ran": 1, "sha256:base": 1}
	var inspect []string
	for id, n := range created {
		labels := "null"
		if id == "sha256:ran" {
			labels = `{"stormo.image":"sidecar"}`
		}
		inspect = append(inspect, fmt.Sprintf(`{"Id":%q,"Created":%q,"Config":{"Labels":%s}}`, id, day(n).Format(time.RFC3339Nano), labels))
	}
	d := &pruneDocker{
		ls: ls.String(), ps: "c1\nc2\n",
		containers: `[{"Image":"sha256:ran","Config":{"Image":"stormo-sidecar:bbbbbbbbbbbb-dirty"}},
			{"Image":"sha256:other","Config":{"Image":"postgres:17"}}]`,
		inspect: "[" + strings.Join(inspect, ",") + "]",
		size:    []string{"10GB", "8.5GB"},
	}
	fakePruneDocker(t, d)

	rep, err := PruneImages(inst, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"needletail:dev-1111111", "needletail:dev-2222222", repository(browser) + ":000000000000", "stormo-sidecar:aaaaaaaaaaaa"}
	if got := removedNames(rep.Removed); !slices.Equal(got, want) {
		t.Errorf("dry run would remove %q, want %q", got, want)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "docker image rm") {
			t.Errorf("a dry run removed something: %q", c)
		}
	}
	if !rep.DryRun || rep.Kept != 9 {
		t.Errorf("dry run report: %+v (kept %d, want 9)", rep, rep.Kept)
	}

	d.calls = nil
	rep, err = PruneImages(inst, false)
	if err != nil {
		t.Fatal(err)
	}
	var rms []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, "docker image rm ") {
			rms = append(rms, strings.TrimPrefix(c, "docker image rm "))
		}
	}
	slices.Sort(rms)
	wantRm := want
	if !slices.Equal(rms, wantRm) {
		t.Errorf("removed %q, want %q", rms, wantRm)
	}
	if rep.Freed != 1_500_000_000 || len(rep.Removed) != 4 || len(rep.Failed) != 0 {
		t.Errorf("report: %+v", rep)
	}
}

func TestPruneImagesRemovesNothingWhenContainersCannotBeRead(t *testing.T) {
	d := &pruneDocker{ps: "c1\n", containersErr: errors.New("no such container: c1")}
	fakePruneDocker(t, d)
	if _, err := PruneImages(exampleInstance(t), false); err == nil || !strings.Contains(err.Error(), "none is removed") {
		t.Errorf("err = %v", err)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "docker image") {
			t.Errorf("went on to %q", c)
		}
	}
}

func TestPruneImagesRefusesAnInstanceWithAnAgentThatDoesNotLoad(t *testing.T) {
	inst := exampleInstance(t)
	if err := os.WriteFile(filepath.Join(inst.Root, "agents", "atlas", "agent.yaml"), []byte("engine: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &pruneDocker{}
	fakePruneDocker(t, d)
	if _, err := PruneImages(inst, false); err == nil || !strings.Contains(err.Error(), "atlas") {
		t.Errorf("err = %v", err)
	}
	if len(d.calls) != 0 {
		t.Errorf("asked docker %q", d.calls)
	}
}
