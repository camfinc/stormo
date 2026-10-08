package deploy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func load(t *testing.T, id string) (*instance.Instance, *manifest.Agent) {
	t.Helper()
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return inst, a
}

func containers(td TaskDef) map[string]Container {
	m := map[string]Container{}
	for _, c := range td.ContainerDefinitions {
		m[c.Name] = c
	}
	return m
}

func TestTaskDefOrdersContainersForTheFinalNap(t *testing.T) {
	inst, a := load(t, "atlas")
	eng := hermes.New(inst)
	td := RenderTaskDef(a, eng, DefaultTarget(inst))
	c := containers(td)
	if want := []DependsOn{{"rehydrate", "SUCCESS"}, {"nap", "START"}}; !reflect.DeepEqual(c["agent"].DependsOn, want) {
		t.Fatalf("agent dependsOn %v", c["agent"].DependsOn)
	}
	if c["agent"].Image != "nousresearch/hermes-agent:v2026.9.24" {
		t.Fatalf("image %s", c["agent"].Image)
	}
	if c["nap"].Secrets != nil {
		t.Fatal("sidecars must get no secrets")
	}
	names := func(td TaskDef) []string {
		out := []string{}
		for _, s := range *containers(td)["agent"].Secrets {
			out = append(out, s.Name)
		}
		return out
	}
	// An optional secret is injected only when the aws layer has it (else ECS fails the task at start).
	if slices.Contains(names(td), "CRM_API_TOKEN") {
		t.Fatal("absent optional secret injected")
	}
	all := manifest.ApplyOptional(a, append(append([]string{}, a.Secrets...), "CRM_API_TOKEN")).Agent
	if !slices.Contains(names(RenderTaskDef(all, eng, DefaultTarget(inst))), "CRM_API_TOKEN") {
		t.Fatal("present optional secret not injected")
	}
	if td.Family != "acme-stormo-atlas" {
		t.Fatalf("family %s", td.Family)
	}
	if got := RenderTaskPolicy(a, DefaultTarget(inst)).Statement[0].Resource; !reflect.DeepEqual(got, []string{"arn:aws:s3:::acme-stormo-naps/atlas/*"}) {
		t.Fatalf("policy resource %v", got)
	}
	b, _ := json.Marshal(td)
	if strings.Contains(string(b), "acme_") {
		t.Fatal("a token-shaped value leaked into the task definition")
	}
}

func efsTarget(inst *instance.Instance) Target {
	t := DefaultTarget(inst)
	t.Efs = instance.Efs{FileSystemID: "fs-123", AccessPoints: map[string]string{"group": "fsap-g", "sales": "fsap-t", "support": "fsap-s"}}
	return t
}

func TestAgentMountsGroupAndOwnUnitOnly(t *testing.T) {
	inst, a := load(t, "atlas")
	td := RenderTaskDef(a, hermes.New(inst), efsTarget(inst))
	got := [][2]string{}
	for _, v := range td.Volumes {
		if v.EfsVolumeConfiguration != nil {
			got = append(got, [2]string{v.Name, v.EfsVolumeConfiguration.AuthorizationConfig.AccessPointID})
		}
	}
	if want := [][2]string{{"shared-group", "fsap-g"}, {"shared-sales", "fsap-t"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("efs volumes %v", got)
	}
	c := containers(td)
	paths := func(c Container) []string {
		out := []string{}
		for _, m := range c.MountPoints {
			out = append(out, m.ContainerPath)
		}
		return out
	}
	if want := []string{"/opt/data", "/shared/group", "/shared/sales"}; !reflect.DeepEqual(paths(c["agent"]), want) {
		t.Fatalf("agent mounts %v", paths(c["agent"]))
	}
	if want := []string{"/data"}; !reflect.DeepEqual(paths(c["nap"]), want) {
		t.Fatalf("nap mounts %v", paths(c["nap"]))
	}
	b, _ := json.Marshal(td)
	if strings.Contains(string(b), "fsap-s") {
		t.Fatal("another unit's access point leaked")
	}
	env := map[string]string{}
	for _, kv := range c["agent"].Environment {
		env[kv.Name] = kv.Value
	}
	if env["HERMES_WRITE_SAFE_ROOT"] != "/opt/data:/shared" || env["SWARM_SHARED_DIR"] != "/shared" {
		t.Fatalf("env %v", env)
	}
}

func TestTaskRoleMountsExactlyItsAccessPoints(t *testing.T) {
	inst, a := load(t, "atlas")
	tg := efsTarget(inst)
	st := RenderTaskPolicy(a, tg).Statement[2]
	got := st.Condition["StringEquals"].(map[string]any)["elasticfilesystem:AccessPointArn"]
	want := []string{
		"arn:aws:elasticfilesystem:us-east-2:123456789012:access-point/fsap-g",
		"arn:aws:elasticfilesystem:us-east-2:123456789012:access-point/fsap-t",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("access points %v", got)
	}
	if u := Unresolved(a, tg); len(u) != 0 {
		t.Fatalf("unresolved %v", u)
	}
	if u := Unresolved(a, DefaultTarget(inst)); !reflect.DeepEqual(u, []string{"efs.fileSystemId", "efs.accessPoints.group", "efs.accessPoints.sales"}) {
		t.Fatalf("unresolved %v", u)
	}
}

func TestApplyAndSharedCommands(t *testing.T) {
	inst, a := load(t, "atlas")
	tg := DefaultTarget(inst)
	cmds := ApplyCommands(a, tg, "")
	if !strings.Contains(cmds[0], "file://dist/atlas/taskdef.json") || !strings.Contains(cmds[2], "--service-name acme-stormo-atlas") || !strings.Contains(cmds[2], "FARGATE") {
		t.Fatalf("apply %v", cmds)
	}
	sh := SharedAccessPointCommands([]string{"group", "sales"}, tg)
	if len(sh) != 6 || !strings.Contains(sh[3], "--file-system-id <fs-id>") || !strings.Contains(sh[4], "Value=acme-stormo-shared-sales") {
		t.Fatalf("shared %v", sh)
	}
}

func TestSidecarImage(t *testing.T) {
	inst, a := load(t, "atlas")
	tg := DefaultTarget(inst)
	tg.SidecarTag = "abc"
	if got := SidecarImage(a, tg); got != "ghcr.io/acme/acme-stormo-atlas:abc" {
		t.Fatal(got)
	}
	a.Deploy.Registry = "ecr"
	if got := SidecarImage(a, tg); got != "123456789012.dkr.ecr.us-east-2.amazonaws.com/acme-stormo-atlas:abc" {
		t.Fatal(got)
	}
}
