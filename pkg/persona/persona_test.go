package persona

import (
	"strings"
	"testing"
)

const existing = `---
type: bot-persona
brand: acme
# Her desk.
desk:
  app: records  # the case files
  props: [folders, mug]
---

# Ada

Some identity notes.

## Avatar lock

A short coily crop and round glasses.

## Notes

Keep this.
`

func TestReadExisting(t *testing.T) {
	l := Read(existing)
	if l.Description != "A short coily crop and round glasses." {
		t.Errorf("description %q", l.Description)
	}
	if l.Desk == nil || strings.Join(l.Desk.Apps, ",") != "records" || len(l.Desk.Props) != 2 || l.Sprite != nil {
		t.Errorf("look %+v", l)
	}
}

func TestRenderKeepsTheRest(t *testing.T) {
	l := Read(existing)
	l.Description = "A silver pixie."
	l.Sprite = &Sprite{Skin: "#e2b896", HairStyle: "short", Accessory: "none"}
	l.Desk.Apps = []string{"charts", "inbox"}
	l.Desk.Screens = 2
	out, err := Render(existing, l)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type: bot-persona", "brand: acme", "# Her desk.", "app: [charts, inbox]", "screens: 2",
		`skin: "#e2b896"`, "hair_style: short", "Some identity notes.", "## Avatar lock\n\nA silver pixie.\n\n## Notes\n\nKeep this."} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if l2 := Read(out); true {
		l2.Desk.Side = "bin"
		again, _ := Render(strings.Replace(out, "app: [charts, inbox]", "app: [charts, inbox]  # kept", 1), l2)
		if !strings.Contains(again, "app: [charts, inbox]  # kept") || !strings.Contains(again, "side: bin") {
			t.Errorf("an unchanged value lost its comment:\n%s", again)
		}
	}
	if strings.Contains(out, "coily") {
		t.Errorf("old description kept:\n%s", out)
	}
	back := Read(out)
	if back.Description != "A silver pixie." || back.Sprite.Skin != "#e2b896" || back.Desk.Screens != 2 {
		t.Errorf("round trip %+v", back)
	}
	// Clearing the look drops the blocks, not the other keys.
	out, err = Render(out, Look{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sprite:") || strings.Contains(out, "desk:") || !strings.Contains(out, "brand: acme") {
		t.Errorf("cleared:\n%s", out)
	}
}

func TestNewAndValidate(t *testing.T) {
	out, err := New("Atlas", "atlas", Look{Description: "A calm figure.", Sprite: &Sprite{Shirt: "#1f3a6e"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "---\nsprite:\n") || !strings.Contains(out, "# Atlas\n") || !strings.Contains(out, "## Avatar lock\n\nA calm figure.\n") {
		t.Errorf("new:\n%s", out)
	}
	for _, bad := range []Look{
		{Sprite: &Sprite{Skin: "red"}},
		{Sprite: &Sprite{HairStyle: "mohawk"}},
		{Desk: &Desk{Props: []string{"a", "b", "c", "d", "e", "f", "g"}}},
		{Desk: &Desk{Side: "Not A Name"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
