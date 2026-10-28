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
// A tool needs name and description; a schedule script only format (and the caps it uses); an
// event handler (skills/<skill>/handlers/<name>.luau) name, description and the events it
// handles (with on_schedule, the jobs it serves), and no args (an event's shape is the
// runtime's).
type Header struct {
	Format      int
	Name        string
	Description string
	// Args is the arguments' JSON schema (an object schema), as the model is given it.
	Args    json.RawMessage
	Caps    []string
	Timeout time.Duration
	// Events are the events a handler handles (on_message, on_mention, on_schedule,
	// before_send); Jobs the jobs (ids or names, "*" for every job) its on_schedule serves.
	Events []string
	Jobs   []string
}

type rawHeader struct {
	Format      int            `yaml:"format"`
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Args        map[string]any `yaml:"args"`
	Caps        []string       `yaml:"caps"`
	Timeout     string         `yaml:"timeout"`
	Events      []string       `yaml:"events"`
	Jobs        []string       `yaml:"jobs"`
}

// Kind is what a .luau file is: its header's rules follow it.
type Kind int

// Kinds of .luau files.
const (
	KindScript  Kind = iota // a schedule's script (scripts/…): format and caps
	KindTool                // skills/<skill>/tools/<name>.luau: name, description, args
	KindHandler             // skills/<skill>/handlers/<name>.luau: name, description, events
)

// ParseHeader reads src's header; tool requires a tool's fields (else a script's).
func ParseHeader(src []byte, tool bool) (*Header, error) {
	if tool {
		return ParseHeaderOf(src, KindTool)
	}
	return ParseHeaderOf(src, KindScript)
}

// ParseHeaderOf reads src's header as kind's.
func ParseHeaderOf(src []byte, kind Kind) (*Header, error) {
	tool := kind == KindTool || kind == KindHandler
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
	// The header is a level-0 long comment ("--[["), which Luau ends at its first "]]" wherever it
	// stands: one before the closing line would end it there, and the rest would run as code
	// while this parser reads it as YAML. ("]=]" and longer closers end only a comment opened at
	// their level, which the header's opening line never is.)
	if bytes.Contains(body, []byte(headerClose)) {
		return nil, fmt.Errorf("the header may not contain %q before its closing line (Luau ends the comment at the first one)", headerClose)
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
	if err := h.events(raw, kind); err != nil {
		return nil, err
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
	if kind == KindHandler && raw.Args != nil {
		return nil, errors.New("header: a handler takes no args: it is called with its event (docs/needletail.md §5.5)")
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

// Events a handler may handle.
const (
	OnMessage  = "on_message"
	OnMention  = "on_mention"
	OnSchedule = "on_schedule"
	BeforeSend = "before_send"
)

// EventNames is every event, in the order the reference lists them.
var EventNames = []string{OnMessage, OnMention, OnSchedule, BeforeSend}

// maxJobs bounds a handler's jobs list.
const maxJobs = 64

// events checks a handler's events and jobs (and that no other kind has them).
func (h *Header) events(raw rawHeader, kind Kind) error {
	if kind != KindHandler {
		if len(raw.Events) > 0 || len(raw.Jobs) > 0 {
			return errors.New("header: events and jobs belong to a handler (skills/<skill>/handlers/<name>.luau)")
		}
		return nil
	}
	if len(raw.Events) == 0 {
		return fmt.Errorf("header: a handler names the events it handles: events: [%s]", strings.Join(EventNames, ", "))
	}
	seen := map[string]bool{}
	for _, e := range raw.Events {
		e = strings.TrimSpace(e)
		known := false
		for _, k := range EventNames {
			known = known || e == k
		}
		if !known {
			return fmt.Errorf("header: events: %q is not an event (%s)", e, strings.Join(EventNames, ", "))
		}
		if !seen[e] {
			seen[e] = true
			h.Events = append(h.Events, e)
		}
	}
	switch {
	case seen[OnSchedule] && len(raw.Jobs) == 0:
		return errors.New("header: on_schedule needs jobs: the ids or names of the jobs it serves (or [\"*\"] for every job)")
	case !seen[OnSchedule] && len(raw.Jobs) > 0:
		return errors.New("header: jobs only goes with on_schedule")
	case len(raw.Jobs) > maxJobs:
		return fmt.Errorf("header: jobs lists more than %d jobs", maxJobs)
	}
	for _, j := range raw.Jobs {
		j = strings.TrimSpace(j)
		if j == "" || len(j) > 200 || strings.ContainsFunc(j, func(r rune) bool { return r < ' ' }) {
			return fmt.Errorf("header: jobs: %q is not a job id or name", j)
		}
		h.Jobs = append(h.Jobs, j)
	}
	return nil
}

// Handles is whether the handler handles event.
func (h *Header) Handles(event string) bool {
	for _, e := range h.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Serves is whether a handler's on_schedule serves the job (by id or name, or "*").
func (h *Header) Serves(id, name string) bool {
	for _, j := range h.Jobs {
		if j == "*" || j == id || name != "" && j == name {
			return true
		}
	}
	return false
}
