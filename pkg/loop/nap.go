package loop

import (
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
)

// A nap is a cheap, frequent snapshot of everything an agent instance would lose when its task
// stops. It runs in a sidecar on the shared volume, so it is engine-agnostic: the engine only
// supplies the classification rules.

// NapFile is one file of a nap. Field order is the manifest format.
type NapFile struct {
	Path   string           `json:"path"`
	Class  engine.FileClass `json:"class"`
	Sha256 string           `json:"sha256"`
	Size   int              `json:"size"`
}

// NapBaseline records which baseline the nap's instance booted from.
type NapBaseline struct {
	GitSha string              `json:"gitSha"`
	Skills build.OrderedHashes `json:"skills"`
}

// Nap is <agent>/naps/<id>.json.
type Nap struct {
	ID       string       `json:"id"`
	Agent    string       `json:"agent"`
	Unit     string       `json:"unit"`
	Engine   string       `json:"engine"`
	Instance string       `json:"instance"`
	TakenAt  string       `json:"takenAt"`
	Reason   string       `json:"reason"` // interval | shutdown | manual
	Baseline *NapBaseline `json:"baseline"`
	Files    []NapFile    `json:"files"`
}

func BlobKey(agent string, cls engine.FileClass, sha string) string {
	if cls == engine.Raw {
		return agent + "/raw/" + sha
	}
	return agent + "/blobs/" + sha
}

func NapKey(agent, id string) string { return agent + "/naps/" + id + ".json" }

// ReadJSON decodes a key, returning false when it does not exist.
func ReadJSON(s Store, key string, v any) (bool, error) {
	body, err := s.Get(key)
	if err != nil || body == nil {
		return false, err
	}
	return true, json.Unmarshal(body, v)
}

// Latest is the nap rehydrate would use, or nil.
func Latest(s Store, agent string) (*Nap, error) {
	var ptr struct {
		Nap string `json:"nap"`
	}
	if ok, err := ReadJSON(s, agent+"/latest.json", &ptr); !ok || err != nil {
		return nil, err
	}
	var n Nap
	if ok, err := ReadJSON(s, NapKey(agent, ptr.Nap), &n); !ok || err != nil {
		return nil, err
	}
	return &n, nil
}

// fingerprint ignores order: naps written by different engine versions sort paths differently.
func fingerprint(files []NapFile) string {
	lines := make([]string, len(files))
	for i, f := range files {
		lines[i] = f.Path + ":" + f.Sha256
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// IsoMillis is an RFC 3339 UTC timestamp with milliseconds, the format of every stored time.
func IsoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// NapOptions describe one agent instance to snapshot.
type NapOptions struct {
	Agent, Unit string
	Engine      engine.Engine
	Home        string
	Store       Store
	// Instance names the running task/container (part of the nap id).
	Instance    string
	BaselineDir string
	Reason      string
	Now         time.Time
}

// NapOnce snapshots the home; returns nil when nothing changed since the latest nap.
func NapOnce(o NapOptions) (*Nap, error) {
	files, err := TakeSnapshot(o.Home, o.Engine.Layout().Rules)
	if err != nil {
		return nil, err
	}
	meta := make([]NapFile, len(files))
	for i, f := range files {
		meta[i] = NapFile{f.Path, f.Class, f.Sha256, f.Size}
	}
	prev, err := Latest(o.Store, o.Agent)
	if err != nil {
		return nil, err
	}
	if prev != nil && fingerprint(prev.Files) == fingerprint(meta) {
		return nil, nil // nothing new to save
	}
	for _, f := range files {
		key := BlobKey(o.Agent, f.Class, f.Sha256)
		ok, err := o.Store.Exists(key)
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := o.Store.Put(key, f.Body); err != nil {
				return nil, err
			}
		}
	}
	var baseline *NapBaseline
	if o.BaselineDir != "" {
		if body, err := os.ReadFile(filepath.Join(o.BaselineDir, build.InfoPath)); err == nil {
			var info build.Info
			if err := json.Unmarshal(body, &info); err != nil {
				return nil, err
			}
			baseline = &NapBaseline{GitSha: info.GitSha, Skills: info.Skills}
		}
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	takenAt := IsoMillis(now)
	reason := o.Reason
	if reason == "" {
		reason = "interval"
	}
	n := &Nap{
		ID:    strings.NewReplacer("-", "", ":", "", ".", "").Replace(takenAt) + "-" + o.Instance,
		Agent: o.Agent, Unit: o.Unit, Engine: o.Engine.Kind(), Instance: o.Instance,
		TakenAt: takenAt, Reason: reason, Baseline: baseline, Files: meta,
	}
	body, err := learning.MarshalIndent(n)
	if err != nil {
		return nil, err
	}
	if err := o.Store.Put(NapKey(o.Agent, n.ID), body); err != nil {
		return nil, err
	}
	ptr, _ := learning.MarshalCompact(map[string]string{"nap": n.ID})
	return n, o.Store.Put(o.Agent+"/latest.json", ptr)
}

// NapLoop naps every interval, and once more on SIGTERM. ECS caps a container's stopTimeout at
// 120 s, so the shutdown nap is bounded by shutdownBudget.
func NapLoop(o NapOptions, interval, shutdownBudget time.Duration) {
	run := func(reason string) {
		o.Reason = reason
		o.Now = time.Time{}
		n, err := NapOnce(o)
		switch {
		case err != nil:
			log.Printf("[nap] %s failed: %v", reason, err)
		case n != nil:
			log.Printf("[nap] %s %s (%d files)", reason, n.ID, len(n.Files))
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	run("interval")
	for {
		select {
		case <-ticker.C:
			run("interval")
		case <-sig:
			done := make(chan struct{})
			go func() { run("shutdown"); close(done) }()
			select {
			case <-done:
			case <-time.After(shutdownBudget):
				log.Printf("[nap] shutdown nap exceeded %s", shutdownBudget)
			}
			os.Exit(0)
		}
	}
}
