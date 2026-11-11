package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadCore(t *testing.T, core string) (*Instance, error) {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: Acme\nslug: acme\ncore:\n"+core), 0o644)
	return Load(root)
}

func TestCoreListenNeedsTLSOrAPrivateAddress(t *testing.T) {
	for core, want := range map[string]string{
		"  listen: 10.0.0.5\n":                                  "needs core.tls",
		"  listen: 10.0.0.5\n  tls: off\n":                      "",
		"  listen: 100.101.102.103\n  tls: off\n":               "", // Tailscale
		"  listen: 203.0.113.7\n  tls: off\n":                   "only allowed on a private address",
		"  listen: 0.0.0.0\n  tls: off\n":                       "every interface",
		"  listen: 0.0.0.0\n  tls: {cert: c.pem, key: k.pem}\n": "",
		"  listen: core.example\n  tls: off\n":                  "must be an IP address",
		"  tls: {cert: c.pem}\n":                                "both cert and key",
		"  url: core.example:18600\n":                           "http(s) URL",
		"  urls: {aws: ftp://x}\n":                              "core.urls.aws",
	} {
		_, err := loadCore(t, core)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", core, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: %v, want %q", core, err, want)
		}
	}
}

func TestCoreURLForPlacements(t *testing.T) {
	i, err := loadCore(t, "  url: https://core.example/\n  urls: {aws: \"https://core.internal:18600\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if u := i.CoreURLFor("aws"); u != "https://core.internal:18600" {
		t.Errorf("aws: %s", u)
	}
	if u := i.CoreURLFor("local"); u != "https://core.example" {
		t.Errorf("local: %s", u)
	}
}

func TestCoreStorageIsAnS3URI(t *testing.T) {
	if i, err := loadCore(t, "  storage: s3://acme-shared/workdir/\n"); err != nil || i.CoreStorage != "s3://acme-shared/workdir" {
		t.Errorf("%v %+v", err, i)
	}
	if _, err := loadCore(t, "  storage: /mnt/efs\n"); err == nil || !strings.Contains(err.Error(), "s3://bucket/prefix") {
		t.Errorf("a path: %v", err)
	}
}

func TestCapabilitiesAreDeclaredWithAWorker(t *testing.T) {
	root := t.TempDir()
	write := func(body string) (*Instance, error) {
		os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: Acme\nslug: acme\n"+body), 0o644)
		return Load(root)
	}
	i, err := write("placements:\n  gpu: {kind: runner}\ncapabilities:\n  media.bake: {kind: http, url: \"https://render.example/jobs\", headers: {Authorization: \"Bearer ${RENDER_KEY}\"}, cost_usd: 0.4}\n  gpu.render: {kind: container, image: \"acme/render:1\", placement: gpu, secrets: [RENDER_KEY]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := i.CapabilityNamed("gpu.render"); !ok || c.Concurrency != 1 || c.TimeoutMinutes != 15 || c.Placement != "gpu" {
		t.Errorf("gpu.render: %+v", c)
	}
	for body, want := range map[string]string{
		"capabilities:\n  x: {kind: http}\n":                                                               "needs url",
		"capabilities:\n  x: {kind: container, image: a, placement: remote}\n":                             "not local or a runner",
		"capabilities:\n  Bad: {kind: http, url: \"https://a\"}\n":                                         "lowercase",
		"capabilities:\n  x: {kind: lambda}\n":                                                             "http, container or service",
		"capabilities:\n  x: {kind: container, image: a, secrets: [key]}\n":                                "env var NAME",
		"capabilities:\n  x: {kind: container, image: a, port: 80}\n":                                      "for kind service",
		"capabilities:\n  x: {kind: http, url: \"https://a\", files: in}\n":                                "files work only",
		"placements:\n  gpu: {kind: runner}\ncapabilities:\n  x: {template: transcribe, placement: gpu}\n": "files work only",
		"capabilities:\n  x: {kind: service, image: a, files: some}\n":                                     "none, in, out or both",
		"capabilities:\n  x: {kind: service, image: a, env: {lower: x}}\n":                                 "env var NAME",
	} {
		if _, err := write(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

func TestCapabilitiesFromTemplates(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: Acme\nslug: acme\ncapabilities:\n"+
		"  audio.transcribe: {template: transcribe, env: {WHISPER_MODEL: small}, idle_minutes: 3}\n"+
		"  web.browse: {template: browser, description: Browse for research}\n"+
		"  echo.svc: {kind: service, image: \"acme/echo:1\"}\n"), 0o644)
	i, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := i.CapabilityNamed("audio.transcribe")
	if c.Kind != "service" || !strings.HasPrefix(c.Image, "stormo-cap-transcribe:") || c.Files != "both" || c.Port != 8080 || c.Health != "/health" ||
		c.IdleMinutes != 3 || c.BootMinutes != 20 || c.Env["WHISPER_MODEL"] != "small" || c.Env["WHISPER_THREADS"] != "0" ||
		c.Docs == "" || c.InputSchema["type"] != "object" || len(c.Examples) == 0 || c.Placement != "local" {
		t.Errorf("transcribe: %+v", c)
	}
	if tpl, ok := c.TemplateImage(); !ok || tpl != "transcribe" {
		t.Errorf("TemplateImage: %q %v", tpl, ok)
	}
	if b, _ := i.CapabilityNamed("web.browse"); b.Description != "Browse for research" || b.ShmMB != 1024 || b.Files != "both" {
		t.Errorf("browser: %+v", b)
	}
	if e, _ := i.CapabilityNamed("echo.svc"); e.Port != 8080 || e.IdleMinutes != 10 || e.BootMinutes != 5 || e.Files != "none" {
		t.Errorf("plain service: %+v", e)
	} else if _, ok := e.TemplateImage(); ok {
		t.Error("a plain service is no template's")
	}
}

// A template this engine does not ship (a newer stormo.yaml read by an older binary, an agent's
// sidecar) is kept as unavailable, not a failed load.
func TestUnknownTemplateIsUnavailableNotFatal(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: Acme\nslug: acme\ncapabilities:\n  x.y: {template: from-the-future}\n  a.b: {template: media}\n"), 0o644)
	i, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := i.CapabilityNamed("x.y"); !strings.Contains(c.Unavailable, "from-the-future") {
		t.Errorf("x.y: %+v", c)
	}
	if c, _ := i.CapabilityNamed("a.b"); c.Unavailable != "" {
		t.Errorf("a.b: %+v", c)
	}
}

func TestIngressCapabilityRules(t *testing.T) {
	core := "core:\n  loopback_trust: false\n  devices: {idle_days: 7}\n"
	gw := func(extra string) string {
		return core + "capabilities:\n  devices.gateway: {template: device-gateway, publish: \"127.0.0.1:18650\"" + extra + "}\n"
	}
	i, err := load(t, gw(", env: {CLIENT_IP_HEADER: Cf-Connecting-Ip}"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := i.CapabilityNamed("devices.gateway")
	if !c.Ingress || !c.AlwaysOn || !c.ReadOnly || c.Publish != "127.0.0.1:18650" || c.Health != "/device/v1/health" || c.Kind != "service" ||
		c.Placement != "local" || c.Files != "none" || c.Env["CLIENT_IP_HEADER"] != "Cf-Connecting-Ip" || c.Env["TOKEN_MINUTES"] != "60" || i.CoreDevicesIdleDays != 7 {
		t.Errorf("gateway: %+v idle=%d", c, i.CoreDevicesIdleDays)
	}
	if i, _ := load(t, ""); i.CoreDevicesIdleDays != 30 {
		t.Errorf("idle_days default: %d", i.CoreDevicesIdleDays)
	}
	for _, bad := range []struct{ yaml, want string }{
		{"capabilities:\n  devices.gateway: {template: device-gateway, publish: \"127.0.0.1:18650\"}\n", "loopback_trust: false"},
		{gw(", always_on: false"), "always on and read-only"},
		{gw(", read_only: false"), "always on and read-only"},
		{gw(", image: \"evil:1\""), "its template's image"},
		{gw(", secrets: [API_KEY]"), "no secrets"},
		{gw(", files: in"), "no files"},
		{gw(", kind: container"), "for kind service"},
		{core + "capabilities:\n  devices.gateway: {template: device-gateway}\n", "needs publish"},
		{core + "capabilities:\n  devices.gateway: {template: device-gateway, publish: \"0.0.0.0:18650\"}\n", "loopback address"},
		{core + "capabilities:\n  devices.gateway: {template: device-gateway, publish: \"127.0.0.1:80\"}\n", "1024 to 65535"},
		{core + "capabilities:\n  x.y: {kind: service, image: \"a:1\", ingress: true}\n", "comes from a template"},
		{core + "capabilities:\n  x.y: {kind: service, image: \"a:1\", publish: \"127.0.0.1:18650\"}\n", "publish is for an ingress"},
		{core + "capabilities:\n  x.y: {kind: http, url: \"https://a.example\", always_on: true}\n", "for kind service"},
		{"core:\n  devices: {idle_days: -1}\n", "idle_days"},
	} {
		if _, err := load(t, bad.yaml); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: %v, want %q", bad.yaml, err, bad.want)
		}
	}
	// Any service may be kept always on; only an ingress one is published.
	i, err = load(t, "capabilities:\n  audio.transcribe: {template: transcribe, always_on: true}\n")
	if c, _ := i.CapabilityNamed("audio.transcribe"); err != nil || !c.AlwaysOn || c.Ingress || c.ReadOnly {
		t.Errorf("always-on transcribe: %+v %v", c, err)
	}
}
