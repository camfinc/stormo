package needletail

import (
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestImagePinned(t *testing.T) {
	n := New(&instance.Instance{})
	for _, v := range []string{"", "latest", "has space", "v1.2.3:x"} {
		if n.ValidVersion(v) == nil {
			t.Errorf("%q accepted", v)
		}
	}
	for _, v := range []string{"0.1.0", "v0.1.0", "1.2.3-rc.1+build.5", "v2026.10.12"} {
		if err := n.ValidVersion(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	a := &manifest.Agent{ID: "atlas"}
	a.Engine.Version = "0.1.0"
	if got := n.Image(a); got != Repository+":v0.1.0" {
		t.Errorf("image = %s", got)
	}
	for v, want := range map[string]string{"v0.1.0": ":v0.1.0", "v2026.10.12": ":v2026.10.12", "stable": ":stable"} {
		a.Engine.Version = v
		if got := n.Image(a); got != Repository+want {
			t.Errorf("version %s: image = %s", v, got)
		}
	}
	a.Engine.ImageTag = "latest"
	if engine.CheckVersion(n, a) == nil {
		t.Error("image_tag latest passed the build check")
	}
	a.Engine.ImageTag = "v0.1.1"
	if err := engine.CheckVersion(n, a); err != nil || n.Image(a) != Repository+":v0.1.1" {
		t.Errorf("pinned tag: %v, %s", err, n.Image(a))
	}
}
