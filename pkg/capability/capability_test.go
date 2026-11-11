package capability

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestBundledTemplates(t *testing.T) {
	names := Names()
	if strings.Join(names, ",") != "browser,device-gateway,hyperframes,image,media,transcribe,video" {
		t.Fatalf("templates: %v", names)
	}
	tag := regexp.MustCompile(`^stormo-cap-[a-z-]+:[0-9a-f]{12}$`)
	for _, tp := range Templates() {
		if tp.Description == "" || tp.Docs == "" || tp.Port == 0 || tp.Health == "" {
			t.Errorf("%s: incomplete %+v", tp.Name, tp)
		}
		// A worker for agents says what it takes; a way in from outside takes no jobs, and is always
		// on and read-only.
		if !tp.Ingress && (tp.InputSchema["type"] != "object" || len(tp.Examples) == 0) {
			t.Errorf("%s: no input schema or examples", tp.Name)
		}
		if tp.Ingress && (!tp.AlwaysOn || !tp.ReadOnly || tp.Files != "none" || tp.InputSchema != nil) {
			t.Errorf("%s: an ingress template is always on, read-only and takes no files or input: %+v", tp.Name, tp)
		}
		switch tp.Files {
		case "none", "in", "out", "both":
		default:
			t.Errorf("%s: files %q", tp.Name, tp.Files)
		}
		if img := Image(tp.Name); !tag.MatchString(img) || img != Image(tp.Name) {
			t.Errorf("%s: image %q", tp.Name, img)
		}
		ctx, err := Context(tp.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Stat(ctx, "Dockerfile"); err != nil {
			t.Errorf("%s: no Dockerfile", tp.Name)
		}
	}
	if _, err := Context("nope"); err == nil {
		t.Error("an unknown template has no context")
	}
}
