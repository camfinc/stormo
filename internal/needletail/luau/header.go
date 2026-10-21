package luau

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// HeaderFormat is the tool header's version (contract v1, §5.4 and §8).
const HeaderFormat = 1

// Header bounds.
const (
	maxHeader      = 16 << 10
	maxDescription = 1024
	MaxTimeout     = 10 * time.Minute
)

const headerOpen, headerClose = "--[[needletail", "]]"

// NameRe is a tool's name in its header (and its file name, without .luau).
var NameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// Header is a .luau file's header: a block comment that opens the file.
//
//	--[[needletail
//	format: 1
//	name: lookup_fare
//	description: Looks up a fare.
//	args: {type: object, properties: {from: {type: string}}, required: [from]}
//	caps: [http:api.example.com, fs.read:home]
//	timeout: 20s
//	]]
//	return function(args) … end
//
// A tool needs name and description; a schedule script only format (and the caps it uses).
type Header struct {
	Format      int
	Name        string
	Description string
	// Args is the arguments' JSON schema (an object schema), as the model is given it.
	Args    json.RawMessage
	Caps    []string
	Timeout time.Duration
}

type rawHeader struct {
	Format      int            `yaml:"format"`
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Args        map[string]any `yaml:"args"`
	Caps        []string       `yaml:"caps"`
	Timeout     string         `yaml:"timeout"`
}

// ParseHeader reads src's header; tool requires a tool's fields.
func ParseHeader(src []byte, tool bool) (*Header, error) {
	src = bytes.TrimPrefix(src, []byte("\ufeff"))
	first, rest, _ := bytes.Cut(src, []byte("\n"))
	if strings.TrimRight(string(first), " \t\r") != headerOpen {
		return nil, fmt.Errorf("the file must start with a %q header block (format, name, description, args, caps, timeout) closed by %q", headerOpen, headerClose)
	}
	var body []byte
	found := false
	for len(rest) > 0 && len(body) <= maxHeader {
		line, more, _ := bytes.Cut(rest, []byte("\n"))
		if strings.TrimRight(string(line), " \t\r") == headerClose {
			found = true
			break
		}
		body = append(body, line...)
		body = append(body, '\n')
		rest = more
	}
	if len(body) > maxHeader {
		return nil, fmt.Errorf("the header is over %d bytes", maxHeader)
	}
	if !found {
		return nil, fmt.Errorf("the header is not closed by a %q line", headerClose)
	}
	if !utf8.Valid(body) {
		return nil, errors.New("the header is not UTF-8")
	}
	var raw rawHeader
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("header: %v", err)
	}
	h := &Header{Format: raw.Format, Name: raw.Name, Description: strings.TrimSpace(raw.Description)}
	if h.Format != HeaderFormat {
		return nil, fmt.Errorf("header: format must be %d", HeaderFormat)
	}
	if tool || h.Name != "" {
		if !NameRe.MatchString(h.Name) {
			return nil, fmt.Errorf("header: name %q must match %s", h.Name, NameRe)
		}
	}
	if tool && h.Description == "" {
		return nil, errors.New("header: a description is required")
	}
	if utf8.RuneCountInString(h.Description) > maxDescription {
		return nil, fmt.Errorf("header: the description is over %d characters", maxDescription)
	}
	seen := map[string]bool{}
	for _, c := range raw.Caps {
		c = strings.TrimSpace(c)
		if _, err := ParseCap(c); err != nil {
			return nil, fmt.Errorf("header: caps: %v", err)
		}
		if !seen[c] {
			seen[c] = true
			h.Caps = append(h.Caps, c)
		}
	}
	if raw.Timeout != "" {
		d, err := time.ParseDuration(raw.Timeout)
		if err != nil || d <= 0 || d > MaxTimeout {
			return nil, fmt.Errorf("header: timeout %q must be a duration up to %s (20s, 2m)", raw.Timeout, MaxTimeout)
		}
		h.Timeout = d
	}
	args := raw.Args
	if args == nil {
		args = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if t, _ := args["type"].(string); t != "object" {
		return nil, errors.New("header: args must be a JSON schema of type object")
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("header: args: %v", err)
	}
	h.Args = b
	return h, nil
}
