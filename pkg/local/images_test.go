package local

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/engine/needletail"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// fakeDocker replaces docker for EnsureEngineImage: present images, and every command it was asked.
func fakeDocker(t *testing.T, present []string, pullErr error) *[]string {
	t.Helper()
	var calls []string
	h, r := hasImage, runDocker
	t.Cleanup(func() { hasImage, runDocker = h, r })
	hasImage = func(tag string) bool { return slices.Contains(present, tag) }
	runDocker = func(argv ...string) error {
		calls = append(calls, strings.Join(argv, " "))
		if argv[1] == "pull" {
			return pullErr
		}
		return nil
	}
	return &calls
}

func TestEnsureEngineImagePullsAMissingPublishedImage(t *testing.T) {
	inst := &instance.Instance{}
	a := &manifest.Agent{ID: "atlas"}
	a.Engine.Version, a.Engine.ImageTag = "0.1.0-dev.abc1234", "dev-abc1234"

	calls := fakeDocker(t, nil, nil)
	if err := EnsureEngineImage(needletail.New(inst), a, func(string) {}); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker pull ghcr.io/camfinc/needletail:dev-abc1234", "docker tag ghcr.io/camfinc/needletail:dev-abc1234 needletail:dev-abc1234"}
	if !slices.Equal(*calls, want) {
		t.Errorf("missing image: %q, want %q", *calls, want)
	}

	// A local build (or an earlier pull) is used as it is.
	calls = fakeDocker(t, []string{"needletail:dev-abc1234"}, nil)
	if err := EnsureEngineImage(needletail.New(inst), a, func(string) {}); err != nil || len(*calls) != 0 {
		t.Errorf("present image: %v, %q", err, *calls)
	}

	// A tag that was never published says what to do and tags nothing.
	calls = fakeDocker(t, nil, errors.New("manifest unknown"))
	err := EnsureEngineImage(needletail.New(inst), a, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "needletail:dev-abc1234 is not here") || len(*calls) != 1 {
		t.Errorf("unpublished tag: %v, %q", err, *calls)
	}

	// An engine that names a registry image itself is left to compose.
	calls = fakeDocker(t, nil, nil)
	if err := EnsureEngineImage(hermes.New(inst), a, func(string) {}); err != nil || len(*calls) != 0 {
		t.Errorf("hermes: %v, %q", err, *calls)
	}
}
