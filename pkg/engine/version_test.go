package engine

import (
	"errors"
	"testing"

	"github.com/camfinc/stormo/pkg/manifest"
)

// stub is an engine that only has a version rule (the rest of Engine is never called).
type stub struct{ Engine }

func (stub) ValidVersion(v string) error {
	if v == "latest" {
		return errors.New("not pinned")
	}
	return nil
}

func TestCheckVersion(t *testing.T) {
	a := &manifest.Agent{ID: "atlas"}
	a.Engine.Version = "1.0.0"
	if err := CheckVersion(stub{}, a); err != nil {
		t.Errorf("valid: %v", err)
	}
	a.Engine.ImageTag = "latest"
	if err := CheckVersion(stub{}, a); err == nil {
		t.Error("image_tag latest accepted")
	}
	a.Engine.Version, a.Engine.ImageTag = "latest", ""
	if err := CheckVersion(stub{}, a); err == nil {
		t.Error("version latest accepted")
	}
	// An engine without a rule accepts anything.
	if err := CheckVersion(struct{ Engine }{}, a); err != nil {
		t.Errorf("no validator: %v", err)
	}
}
