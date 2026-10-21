package sched

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// File is cron/jobs.json as read: {"jobs": [...]} (the shape Hermes and the adapter write) or a
// bare list. Every job is kept as its raw object in its own key order, so keys the scheduler does
// not model (monitor_url, context_from, origin, a Hermes key added later) survive every write,
// and a file written back unchanged is the file that was read.
type File struct {
	keys []string // top-level keys in order (object form)
	obj  map[string]json.RawMessage
	list bool // a bare list
	Jobs []*Job
}

// Job is one job record: an ordered JSON object.
type Job struct {
	keys []string
	raw  map[string]json.RawMessage
}

// ReadFile reads path; a missing file is an empty one.
func ReadFile(path string) (*File, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{keys: []string{"jobs"}, obj: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

// Parse reads a jobs file's body.
func Parse(body []byte) (*File, error) {
	f := &File{}
	var arr []json.RawMessage
	if len(bytes.TrimSpace(body)) == 0 {
		return &File{keys: []string{"jobs"}, obj: map[string]json.RawMessage{}}, nil
	}
	if json.Unmarshal(body, &arr) == nil {
		f.list = true
	} else {
		keys, obj, err := orderedObject(body)
		if err != nil {
			return nil, fmt.Errorf("cron jobs file: %w", err)
		}
		f.keys, f.obj = keys, obj
		if raw, ok := obj["jobs"]; ok && string(bytes.TrimSpace(raw)) != "null" {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, fmt.Errorf("cron jobs file: jobs: %w", err)
			}
		}
	}
	for i, raw := range arr {
		keys, obj, err := orderedObject(raw)
		if err != nil {
			return nil, fmt.Errorf("cron jobs file: job %d: %w", i, err)
		}
		f.Jobs = append(f.Jobs, &Job{keys: keys, raw: obj})
	}
	return f, nil
}

// Encode is the file as it is written: two-space indent, non-ASCII and "<" kept as written, a
// trailing newline.
func (f *File) Encode() ([]byte, error) {
	jobs := make([]json.RawMessage, 0, len(f.Jobs))
	for _, j := range f.Jobs {
		jobs = append(jobs, j.encode())
	}
	var b bytes.Buffer
	if f.list {
		writeList(&b, jobs)
	} else {
		keys := f.keys
		if _, ok := f.obj["jobs"]; !ok && !contains(keys, "jobs") {
			keys = append(keys, "jobs")
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			if k == "jobs" {
				writeList(&b, jobs)
			} else {
				b.Write(f.obj[k])
			}
		}
		b.WriteByte('}')
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Write replaces path atomically, its temporary file in tmpDir (the home's unclaimed temp dir).
func (f *File) Write(path, tmpDir string) error {
	body, err := f.Encode()
	if err != nil {
		return err
	}
	return fsx.WriteAtomicIn(path, tmpDir, body, 0o644)
}

func writeList(b *bytes.Buffer, items []json.RawMessage) {
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
}

// Find is the job with id, or nil.
func (f *File) Find(id string) *Job {
	for _, j := range f.Jobs {
		if j.ID() == id {
			return j
		}
	}
	return nil
}

// Remove drops the job with id; false when there was none.
func (f *File) Remove(id string) bool {
	for i, j := range f.Jobs {
		if j.ID() == id {
			f.Jobs = append(f.Jobs[:i:i], f.Jobs[i+1:]...)
			return true
		}
	}
	return false
}

func (j *Job) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range j.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(j.raw[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// NewJob is an empty record with keys in the order Hermes writes them, every value null.
func NewJob() *Job {
	j := &Job{raw: map[string]json.RawMessage{}}
	for _, k := range JobKeys {
		j.keys = append(j.keys, k)
		j.raw[k] = json.RawMessage("null")
	}
	return j
}

// JobKeys are a job record's keys in the order Hermes (and the adapter's render) writes them.
var JobKeys = []string{"id", "name", "prompt", "skills", "skill", "model", "provider", "base_url", "script", "no_agent",
	"monitor_script", "monitor_url", "monitor_state", "context_from", "schedule", "schedule_display", "repeat", "enabled",
	"state", "paused_at", "paused_reason", "created_at", "next_run_at", "last_run_at", "last_status", "last_error",
	"last_delivery_error", "deliver", "origin", "enabled_toolsets", "workdir", "failure_deliver", "running"}

// Set stores v under k (appended when the record has no such key).
func (j *Job) Set(k string, v any) {
	raw, err := marshal(v)
	if err != nil {
		return
	}
	if _, ok := j.raw[k]; !ok {
		j.keys = append(j.keys, k)
	}
	j.raw[k] = raw
}

// Has is whether the record has key k (even null).
func (j *Job) Has(k string) bool { _, ok := j.raw[k]; return ok }

func marshal(v any) (json.RawMessage, error) {
	if r, ok := v.(json.RawMessage); ok {
		return r, nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

func (j *Job) decode(k string, v any) bool {
	raw, ok := j.raw[k]
	return ok && json.Unmarshal(raw, v) == nil
}

// Str is a string value ("" when absent, null or not a string).
func (j *Job) Str(k string) string {
	var s string
	if !j.decode(k, &s) {
		return ""
	}
	return s
}

// Bool is a boolean value (false when absent or not a boolean).
func (j *Job) Bool(k string) bool {
	var b bool
	j.decode(k, &b)
	return b
}

// Int is a number value (0 when absent).
func (j *Job) Int(k string) int {
	var f float64
	j.decode(k, &f)
	return int(f)
}

// Strings is a list of strings; a lone string is a list of one.
func (j *Job) Strings(k string) []string {
	var l []string
	if j.decode(k, &l) {
		return l
	}
	if s := j.Str(k); s != "" {
		return []string{s}
	}
	return nil
}

// ID is the job's id (a number is shown as written).
func (j *Job) ID() string {
	if s := j.Str("id"); s != "" {
		return s
	}
	raw := strings.TrimSpace(string(j.raw["id"]))
	if raw == "null" {
		return ""
	}
	return raw
}

// Name is the job's name, else its id.
func (j *Job) Name() string {
	if n := j.Str("name"); n != "" {
		return n
	}
	return j.ID()
}

// Schedule is the job's timing.
func (j *Job) Schedule() wire.JobSchedule {
	var s wire.JobSchedule
	j.decode("schedule", &s)
	return s
}

// Repeat is the job's run count: times (0 forever) and completed.
func (j *Job) Repeat() (times, completed int) {
	var r struct {
		Times     *float64 `json:"times"`
		Completed float64  `json:"completed"`
	}
	if j.decode("repeat", &r) {
		if r.Times != nil {
			times = int(*r.Times)
		}
		completed = int(r.Completed)
	}
	return times, completed
}

// SetCompleted stores repeat.completed, keeping times as written.
func (j *Job) SetCompleted(n int) {
	var r map[string]json.RawMessage
	if !j.decode("repeat", &r) || r == nil {
		r = map[string]json.RawMessage{"times": json.RawMessage("null")}
	}
	times := r["times"]
	if times == nil {
		times = json.RawMessage("null")
	}
	j.Set("repeat", json.RawMessage(fmt.Sprintf(`{"times":%s,"completed":%d}`, times, n)))
}

// Skills are the job's skills: skills (a list, or one string), else the legacy single skill when
// skills is absent or null, as Hermes reads them.
func (j *Job) Skills() []string {
	names := j.Strings("skills")
	if raw, ok := j.raw["skills"]; !ok || strings.TrimSpace(string(raw)) == "null" {
		names = j.Strings("skill")
	}
	out := []string{}
	for _, s := range names {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Enabled is whether the job runs on its schedule: enabled, and not paused or completed.
func (j *Job) Enabled() bool {
	if raw, ok := j.raw["enabled"]; ok && strings.TrimSpace(string(raw)) == "false" {
		return false
	}
	st := j.Str("state")
	return st != "paused" && st != "completed"
}

// monitorState is Hermes' monitor_state: the hash of the output that last woke the agent.
type monitorState struct {
	LastOutputHash string `json:"last_output_hash"`
	LastChangedAt  string `json:"last_changed_at"`
}

func (j *Job) monitor() (monitorState, bool) {
	var m monitorState
	if !j.decode("monitor_state", &m) || m.LastOutputHash == "" {
		return m, false
	}
	return m, true
}

// orderedObject decodes a JSON object keeping its key order (a repeated key keeps its first
// place and its last value, as Python's json does).
func orderedObject(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("not a JSON object")
	}
	keys := []string{}
	obj := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		var c bytes.Buffer
		if err := json.Compact(&c, v); err != nil {
			return nil, nil, err
		}
		k := kt.(string)
		if _, seen := obj[k]; !seen {
			keys = append(keys, k)
		}
		obj[k] = c.Bytes()
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, nil, errors.New("not a JSON object")
	}
	return keys, obj, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
