package yamlfmt

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestEncodeKeepsTheFileAsWritten(t *testing.T) {
	original := "# head\nagent:\n  max_turns: 60    # aligned\n\n  persona: Duuude 🤙 cowabunga\n  list:\n    - plain 🔥\n    - \"quoted\"\n\nterminal:\n  timeout: 180\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(original), &doc); err != nil {
		t.Fatal(err)
	}
	got, err := Encode(&doc, []byte(original))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("got\n%s\nwant\n%s", got, original)
	}
	// A changed value is written as the encoder writes it; its neighbours stay as they were.
	doc.Content[0].Content[1].Content[1].Value = "80"
	got, _ = Encode(&doc, []byte(original))
	want := "# head\nagent:\n  max_turns: 80 # aligned\n\n  persona: Duuude 🤙 cowabunga\n  list:\n    - plain 🔥\n    - \"quoted\"\n\nterminal:\n  timeout: 180\n"
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A line holding a flow collection keeps its spelling when its value did not change (the encoder
// respaces `{ a: 1 }` as `{a: 1}`), and takes the new one when it did.
func TestFlowLinesKeepTheirSpelling(t *testing.T) {
	original := []byte("office:\n  clocks:\n    - { city: Lisbon, tz: Europe/Lisbon }   # home\n    - { city: Caracas, tz: America/Caracas }\nlist: [ a, b ]\nname: x\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	root.Content[len(root.Content)-1].Value = "y"                             // name: y
	root.Content[1].Content[1].Content[1].Content[3].Value = "America/Bogota" // the second clock's tz
	out, err := Encode(&doc, original)
	if err != nil {
		t.Fatal(err)
	}
	want := "office:\n  clocks:\n    - { city: Lisbon, tz: Europe/Lisbon }   # home\n    - {city: Caracas, tz: America/Bogota}\nlist: [ a, b ]\nname: y\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}
