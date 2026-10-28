package approvals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Canonical is a tool call's arguments as one unambiguous object (docs/needletail.md §7,
// "Approvals"): the card is rendered from it, its hash taken over it, and exactly it runs. The
// tools decode arguments into Go structs, which match keys ignoring case and keep the last of
// duplicates, while a card reads keys as written: so arguments with a key twice, or two keys
// equal ignoring case (at any depth), are refused, and so is a top-level key the tool's schema
// does not name (properties, nil: any). The answer is re-encoded with sorted keys, numbers as
// written and no HTML escaping.
func Canonical(raw json.RawMessage, properties []string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("the arguments are not a JSON object")
	}
	if err := checkObject(dec, properties, "", 0); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the arguments hold more than one JSON value")
	}
	var v any
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("the arguments are not valid JSON: %v", err)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSpace(b.Bytes())), nil
}

// maxDepth bounds the nesting Canonical walks.
const maxDepth = 64

// checkObject reads an object's members after its '{' (top: only the top level checks names
// against properties).
func checkObject(dec *json.Decoder, properties []string, path string, depth int) error {
	if depth > maxDepth {
		return errors.New("the arguments are nested too deep")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("the arguments are not valid JSON: %v", err)
		}
		key, _ := tok.(string)
		for _, k := range keys {
			if k == key {
				return fmt.Errorf("the argument %q is given twice", path+key)
			}
			if strings.EqualFold(k, key) {
				return fmt.Errorf("the arguments %q and %q differ only in case (one would be shown, the other run)", path+k, path+key)
			}
		}
		keys = append(keys, key)
		if depth == 0 && properties != nil && !slices.Contains(properties, key) {
			return fmt.Errorf("the argument %q is not one the tool takes (%s)", key, strings.Join(properties, ", "))
		}
		if err := checkValue(dec, path+key+".", depth+1); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

func checkValue(dec *json.Decoder, path string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("the arguments are not valid JSON: %v", err)
	}
	switch tok {
	case json.Delim('{'):
		return checkObject(dec, nil, path, depth)
	case json.Delim('['):
		if depth > maxDepth {
			return errors.New("the arguments are nested too deep")
		}
		for dec.More() {
			if err := checkValue(dec, path, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	return nil
}

// SchemaProperties are the top-level argument names a JSON schema allows: its properties, or nil
// (any) when it names none or allows others (additionalProperties true or a schema).
func SchemaProperties(schema json.RawMessage) []string {
	var s struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties json.RawMessage            `json:"additionalProperties"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	if a := strings.TrimSpace(string(s.AdditionalProperties)); a != "" && a != "false" {
		return nil
	}
	out := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
