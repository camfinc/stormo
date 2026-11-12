package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/capability"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/version"
)

// KeepRecent is how many of the newest images of each kind a prune keeps besides those in use,
// pinned or current: a rollback, and what another instance on this machine may still start.
const KeepRecent = 2

// dockerOutput runs a docker command for PruneImages and gives its standard output, also when it
// fails (a seam for tests).
var dockerOutput = func(argv ...string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		what := strings.Join(argv[:min(len(argv), 3)], " ")
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return string(out), fmt.Errorf("%s: %s", what, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("%s: %w", what, err)
	}
	return string(out), nil
}

// PrunedImage is an image a prune removed, or would remove: its names, or none for an image a
// newer build of the same name replaced.
type PrunedImage struct {
	Kind    string    `json:"kind"` // the repository it counts under; empty when it has no name
	ID      string    `json:"id"`
	Tags    []string  `json:"tags"`
	Created time.Time `json:"created"`
}

// PruneReport is what PruneImages did.
type PruneReport struct {
	DryRun  bool          `json:"dryRun"`
	Removed []PrunedImage `json:"removed"` // with DryRun, what it would remove
	Kept    int           `json:"kept"`    // Stormo's images it kept
	Freed   int64         `json:"freedBytes"`
	Failed  []string      `json:"failed,omitempty"` // removals docker refused, and why
}

// PruneImages removes the images Stormo made that nothing needs any more. They accumulate as the
// engine moves: a sidecar per engine version (SidecarImage), an engine image per pinned tag,
// pulled or built (engine.PublishedImage), and an image per revision of each capability template
// (capability.Image). An image stays when a container uses it (running or stopped: by its ID and
// by the name it was started from), when an agent of the instance pins it, when it is this
// engine's sidecar or a template's current image, or when it is one of the KeepRecent newest of
// its kind. An image that also carries a name Stormo did not give it is someone else's and is left
// alone, and so is an image without a name unless a Stormo Dockerfile labelled it (stormo.image,
// stormo.template). What it removes can come back: a sidecar is built again from the engine source
// (a release pulls it), a capability image from its template, and an engine image is pulled again
// or, for an unpublished commit, built again from it (docs/instances.md).
func PruneImages(inst *instance.Instance, dryRun bool) (PruneReport, error) {
	rep := PruneReport{DryRun: dryRun, Removed: []PrunedImage{}}
	r, err := pruneRulesFor(inst)
	if err != nil {
		return rep, err
	}
	if err := r.addContainers(); err != nil {
		return rep, err
	}
	images, err := r.images()
	if err != nil {
		return rep, err
	}
	remove, kept := plan(images, r)
	rep.Kept = kept
	if dryRun {
		rep.Removed = remove
		return rep, nil
	}
	before, measured := imagesSize()
	for _, im := range remove {
		names := im.Tags
		if len(names) == 0 {
			names = []string{im.ID}
		}
		if _, err := dockerOutput(append([]string{"docker", "image", "rm"}, names...)...); err != nil {
			rep.Failed = append(rep.Failed, err.Error())
			continue
		}
		rep.Removed = append(rep.Removed, im)
	}
	if after, ok := imagesSize(); ok && measured && after < before {
		rep.Freed = before - after
	}
	return rep, nil
}

// pruneRules are what a prune knows before it looks at the images.
type pruneRules struct {
	kinds map[string]string // a repository Stormo names images in → the kind they count under
	keep  map[string]bool   // names kept: pinned, current, or a container's
	inUse map[string]bool   // image IDs a container runs on
}

func pruneRulesFor(inst *instance.Instance) (pruneRules, error) {
	r := pruneRules{
		kinds: map[string]string{SidecarRepository: SidecarRepository, Published: SidecarRepository},
		keep:  map[string]bool{SidecarImage(): true, Published + ":" + version.String(): true},
		inUse: map[string]bool{},
	}
	for _, name := range capability.Names() {
		if tag := capability.Image(name); tag != "" {
			r.keep[tag] = true
		}
	}
	// Every agent's pin, not only the ones being started: a fleet moves to a new tag one agent at
	// a time.
	for _, id := range manifest.AgentIDs(inst.Root) {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return r, fmt.Errorf("agent %s does not load, so the image it pins is unknown: %w", id, err)
		}
		eng, err := engines.Get(a.Engine.Kind, inst)
		if err != nil {
			return r, err
		}
		pub, ok := eng.(engine.PublishedImage)
		if !ok {
			continue // a registry image that compose pulls: not Stormo's to remove
		}
		local, remote := eng.Image(a), pub.PublishedImage(a)
		r.keep[local], r.keep[remote] = true, true
		r.kinds[repository(local)] = repository(local)
		r.kinds[repository(remote)] = repository(local)
	}
	return r, nil
}

// kind is the kind a repository's images count under, when Stormo names images there.
func (r pruneRules) kind(repo string) (string, bool) {
	if k, ok := r.kinds[repo]; ok {
		return k, true
	}
	if strings.HasPrefix(repo, capability.ImagePrefix) {
		return repo, true // also a template this engine no longer ships
	}
	return "", false
}

// addContainers keeps what every container uses. A container whose name now points at a newer
// build keeps both: the build it runs and the one it would be recreated from.
func (r pruneRules) addContainers() error {
	out, err := dockerOutput("docker", "ps", "-aq", "--no-trunc")
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil
	}
	out, err = dockerOutput(append([]string{"docker", "container", "inspect"}, ids...)...)
	var containers []struct {
		Image  string
		Config struct{ Image string }
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &containers)
	}
	if err != nil {
		return fmt.Errorf("which images the containers use is unknown, so none is removed: %w", err)
	}
	for _, c := range containers {
		r.inUse[c.Image] = true
		name := c.Config.Image
		if name != "" && !strings.Contains(name, "@") && repository(name) == name {
			name += ":latest"
		}
		r.keep[name] = true
	}
	return nil
}

// pruneImage is one image docker holds: its names, when it was made and whether a Stormo
// Dockerfile labelled it.
type pruneImage struct {
	id      string
	tags    []string
	created time.Time
	labeled bool
}

// images lists the images that could be Stormo's: those with one of its names, and those without
// a name (a label then decides).
func (r pruneRules) images() ([]pruneImage, error) {
	out, err := dockerOutput("docker", "image", "ls", "--no-trunc", "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return nil, err
	}
	byID := map[string]*pruneImage{}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		im := byID[f[0]]
		if im == nil {
			im = &pruneImage{id: f[0]}
			byID[f[0]] = im
			ids = append(ids, f[0])
		}
		if f[1] != "<none>" && f[2] != "<none>" && !slices.Contains(im.tags, f[1]+":"+f[2]) {
			im.tags = append(im.tags, f[1]+":"+f[2])
		}
	}
	var look []string
	for _, id := range ids {
		im := byID[id]
		if len(im.tags) == 0 || slices.ContainsFunc(im.tags, func(t string) bool { _, ok := r.kind(repository(t)); return ok }) {
			look = append(look, id)
		}
	}
	if len(look) == 0 {
		return nil, nil
	}
	// An image removed meanwhile fails the inspect but not the others: what was read is used, and
	// an image that was not read is left alone.
	out, _ = dockerOutput(append([]string{"docker", "image", "inspect"}, look...)...)
	var inspected []struct {
		ID      string `json:"Id"`
		Created time.Time
		Config  struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil {
		return nil, fmt.Errorf("docker image inspect: %w", err)
	}
	var images []pruneImage
	for _, x := range inspected {
		if byID[x.ID] == nil {
			continue
		}
		im := *byID[x.ID]
		im.created = x.Created
		im.labeled = x.Config.Labels["stormo.image"] != "" || x.Config.Labels["stormo.template"] != ""
		images = append(images, im)
	}
	return images, nil
}

// plan splits Stormo's images into those to remove and the number kept; anyone else's image is in
// neither.
func plan(images []pruneImage, r pruneRules) (remove []PrunedImage, kept int) {
	remove = []PrunedImage{}
	byKind := map[string][]pruneImage{}
	for _, im := range images {
		kind, ours := "", len(im.tags) > 0 || im.labeled
		for _, t := range im.tags {
			k, ok := r.kind(repository(t))
			if !ok {
				ours = false
				break
			}
			kind = k
		}
		if ours {
			byKind[kind] = append(byKind[kind], im)
		}
	}
	for kind, ims := range byKind {
		sort.SliceStable(ims, func(i, j int) bool { return ims[i].created.After(ims[j].created) })
		for i, im := range ims {
			// A nameless image is a build that a newer one of its name replaced: no rollback.
			recent := kind != "" && i < KeepRecent
			if recent || r.inUse[im.id] || slices.ContainsFunc(im.tags, func(t string) bool { return r.keep[t] }) {
				kept++
				continue
			}
			remove = append(remove, PrunedImage{Kind: kind, ID: im.id, Tags: im.tags, Created: im.created})
		}
	}
	sort.Slice(remove, func(i, j int) bool {
		if remove[i].Kind != remove[j].Kind {
			return remove[i].Kind < remove[j].Kind
		}
		return remove[i].Created.After(remove[j].Created)
	})
	return remove, kept
}

// repository is a reference without its tag ("stormo-sidecar:abc" → "stormo-sidecar"); a
// registry's port is not a tag.
func repository(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// imagesSize is the images' total as docker counts it (shared layers once).
func imagesSize() (int64, bool) {
	out, err := dockerOutput("docker", "system", "df", "--format", "{{.Type}}\t{{.Size}}")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		if typ, size, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && typ == "Images" {
			return parseSize(size)
		}
	}
	return 0, false
}

// parseSize reads docker's sizes ("17.25GB", "512.3kB", "0B"): decimal units.
func parseSize(s string) (int64, bool) {
	i := strings.IndexFunc(s, func(c rune) bool { return (c < '0' || c > '9') && c != '.' })
	if i <= 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15}[s[i:]]
	if unit == 0 {
		return 0, false
	}
	return int64(math.Round(n * unit)), true
}
