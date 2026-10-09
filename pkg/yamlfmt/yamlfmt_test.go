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
