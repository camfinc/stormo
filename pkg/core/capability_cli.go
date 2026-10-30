package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/camfinc/stormo/pkg/capability"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/out"
	"github.com/camfinc/stormo/pkg/secrets"
)

// CapabilityUsage is the capability section of the CLI usage text.
const CapabilityUsage = `  capability (heavy calls the core runs for agents, docs/capabilities.md):
  capability list                        the instance's capabilities: worker (asleep/booting/awake), queue, grants, spend (asks the core)
  capability jobs [n]                    the latest n jobs, with scrubbed previews (asks the core)
  capability wake | sleep <name>         boot a service capability's worker now / put it to sleep (asks the core; operate)
  capability templates                   the templates the engine bundles (use one with capabilities: {<name>: {template: <t>}})
  capability build <name|template>       build a template's worker image on this host (the core builds it on first use otherwise)
  capability try <name> [input] [file…]  run one job here without the core: input is JSON or @file.json, files are local paths;
                                         what it writes lands in .swarm/capabilities/try/; the worker sleeps after`

// CapabilityCommand runs `stormo capability <sub>`.
func CapabilityCommand(inst *instance.Instance, sub string, args []string, o *out.Writer) error {
	activePort = inst.CorePort()
	if o == nil {
		o = &out.Writer{W: os.Stdout}
	}
	switch sub {
	case "", "list":
		var v struct {
			Capabilities []CapabilityView `json:"capabilities"`
			Spend        []Spend          `json:"spend"`
		}
		if err := ownerJSON(inst.Root, http.MethodGet, "/api/capabilities", nil, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			if len(v.Capabilities) == 0 {
				fmt.Fprintln(w, "no capabilities declared (stormo.yaml capabilities:; `stormo capability templates` lists the bundled ones)")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tWORKER\tQUEUED\tRUNNING\tFILES\tAGENTS")
			for _, c := range v.Capabilities {
				worker := c.Worker
				if worker == "" {
					worker = "per job"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", c.Name, c.Kind, worker, c.Queued, c.Running, c.Files, strings.Join(c.Agents, ","))
			}
			tw.Flush()
			for _, s := range v.Spend {
				fmt.Fprintf(w, "spend this month: %s %s: %d jobs (%d failed), $%.2f\n", s.Agent, s.Capability, s.Jobs, s.Failed, s.USD)
			}
		})
		return nil
	case "jobs":
		n := 20
		if len(args) > 0 {
			n, _ = strconv.Atoi(args[0])
		}
		var v struct {
			Jobs []JobView `json:"jobs"`
		}
		if err := ownerJSON(inst.Root, http.MethodGet, fmt.Sprintf("/api/capabilities/jobs?limit=%d", n), nil, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			for _, j := range v.Jobs {
				fmt.Fprintf(w, "%5d  %-8s %-18s %-10s %s  %s\n", j.ID, j.Status, j.Capability, j.Agent, time.UnixMilli(j.Created).Format("01-02 15:04"), firstLine(j.Error, j.Preview))
			}
		})
		return nil
	case "wake", "sleep":
		if len(args) == 0 {
			return fmt.Errorf("usage: stormo capability %s <name>", sub)
		}
		var v struct {
			Capability string            `json:"capability"`
			Service    link.ServiceState `json:"service"`
		}
		if err := ownerJSON(inst.Root, http.MethodPost, "/api/capabilities/"+args[0]+"/"+sub, nil, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			if sub == "wake" {
				fmt.Fprintf(w, "%s: waking (now %s); `stormo capability list` shows when it is awake\n", v.Capability, v.Service.State)
			} else {
				fmt.Fprintf(w, "%s: %s\n", v.Capability, v.Service.State)
			}
		})
		return nil
	case "templates":
		ts := capability.Templates()
		o.Result(map[string]any{"templates": ts}, func(w io.Writer) {
			for _, t := range ts {
				fmt.Fprintf(w, "%s  (%s; files %s)\n  %s\n", t.Name, capability.Image(t.Name), t.Files, t.Description)
				if t.Notes != "" {
					fmt.Fprintf(w, "  note: %s\n", t.Notes)
				}
				fmt.Fprintf(w, "  stormo.yaml:  capabilities: {<name>: {template: %s}}   then grant <name> in agent.yaml capabilities:\n\n", t.Name)
			}
		})
		return nil
	case "build":
		if len(args) == 0 {
			return errors.New("usage: stormo capability build <name|template>")
		}
		t := args[0]
		if c, ok := inst.CapabilityNamed(t); ok {
			if t, ok = c.TemplateImage(); !ok {
				return fmt.Errorf("%s runs %s, not a bundled template's image: build that image yourself", c.Name, c.Image)
			}
		}
		tag, err := capability.Build(t, o.Line)
		if err != nil {
			return err
		}
		o.Result(map[string]string{"template": t, "image": tag}, func(w io.Writer) { fmt.Fprintf(w, "built %s\n", tag) })
		return nil
	case "try":
		return tryCapability(inst, args, o)
	}
	return fmt.Errorf("unknown capability command %q\n%s", sub, CapabilityUsage)
}

func firstLine(s ...string) string {
	for _, v := range s {
		if v != "" {
			l, _, _ := strings.Cut(v, "\n")
			return clip(l, 100)
		}
	}
	return ""
}

// tryCapability runs one job of a local container or service capability on this host, as the
// broker would, without the core: the owner's way to see a worker answer before granting it.
func tryCapability(inst *instance.Instance, args []string, o *out.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: stormo capability try <name> [input-json|@file.json] [file…]")
	}
	c, ok := inst.CapabilityNamed(args[0])
	if !ok {
		return fmt.Errorf("no capability %q in stormo.yaml", args[0])
	}
	if c.Kind == "http" || c.Placement != "local" {
		return fmt.Errorf("%s runs elsewhere (%s): try runs local containers and services", c.Name, c.Kind)
	}
	input := json.RawMessage("{}")
	rest := args[1:]
	if len(rest) > 0 && (strings.HasPrefix(rest[0], "{") || strings.HasPrefix(rest[0], "@")) {
		raw := []byte(rest[0])
		if strings.HasPrefix(rest[0], "@") {
			b, err := os.ReadFile(rest[0][1:])
			if err != nil {
				return err
			}
			raw = b
		}
		if !json.Valid(raw) {
			return errors.New("input is a JSON object")
		}
		input, rest = raw, rest[1:]
	}
	if len(rest) > 0 && (c.Files == "none" || c.Files == "out") {
		return fmt.Errorf("%s takes no files", c.Name)
	}
	if t, ok := c.TemplateImage(); ok {
		if _, err := capability.EnsureImage(t, o.Line); err != nil {
			return err
		}
	}
	env := map[string]string{}
	if len(c.Secrets) > 0 {
		f, err := secrets.Load(secrets.Path(inst.Root))
		if err != nil {
			return err
		}
		for _, n := range c.Secrets {
			if v, ok := secrets.SharedValue(f, n); ok {
				env[n] = v
			}
		}
	}
	stamp := time.Now().Format("20060102-150405")
	jobs := filepath.Join(inst.Root, ".swarm", "capabilities", "try")
	dir := filepath.Join(jobs, jobDirName(c.Name), stamp)
	for _, d := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o777); err != nil {
			return err
		}
		_ = os.Chmod(filepath.Join(dir, d), 0o777)
	}
	files := []string{}
	for _, p := range rest {
		if err := stageLocal(p, filepath.Join(dir, "in", filepath.Base(p))); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		files = append(files, filepath.Base(p))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutMinutes+c.BootMinutes)*time.Minute)
	defer cancel()
	start := time.Now()
	var answer []byte
	var err error
	if c.Kind == "container" {
		answer, err = link.RunContainerIn(ctx, c.Image, c.Command, env, input, dir)
	} else {
		for k, v := range c.Env {
			if _, set := env[k]; !set {
				env[k] = v
			}
		}
		svc := link.NewServices(inst.Slug + "-try")
		svc.Logf = func(f string, a ...any) { o.Step(f, a...) }
		defer svc.StopAll()
		mount := "/jobs/" + stamp
		in := []string{}
		for _, f := range files {
			in = append(in, mount+"/in/"+f)
		}
		outDir := ""
		if c.Files == "out" || c.Files == "both" {
			outDir = mount + "/out"
		}
		body, _ := json.Marshal(map[string]any{"job": 0, "agent": "owner", "input": input, "files": in, "out": outDir})
		o.Step("booting %s (%s)", c.Name, c.Image)
		answer, err = svc.Call(ctx, link.ServiceSpec{Name: c.Name, Image: c.Image, Command: c.Command, Port: c.Port, Health: c.Health, Env: env,
			MemoryMB: c.MemoryMB, ShmMB: c.ShmMB, CPUs: c.CPUs, JobsDir: filepath.Join(jobs, jobDirName(c.Name)),
			Cache: link.NewServices(inst.Slug).Container(c.Name) + "-cache", // the core's: models fetched once
			Idle:  time.Duration(c.IdleMinutes) * time.Minute, Boot: time.Duration(c.BootMinutes) * time.Minute}, body)
	}
	_ = os.RemoveAll(filepath.Join(dir, "in"))
	if err != nil {
		return err
	}
	written := []string{}
	_ = filepath.WalkDir(filepath.Join(dir, "out"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			written = append(written, p)
		}
		return nil
	})
	sort.Strings(written)
	var pretty any = json.RawMessage(answer)
	if !json.Valid(answer) {
		pretty = string(answer)
	}
	o.Result(map[string]any{"capability": c.Name, "seconds": time.Since(start).Seconds(), "answer": pretty, "files": written}, func(w io.Writer) {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Fprintf(w, "%s answered in %.1fs:\n%s\n", c.Name, time.Since(start).Seconds(), clip(string(b), 4000))
		for _, f := range written {
			fmt.Fprintf(w, "wrote %s\n", f)
		}
	})
	return nil
}
