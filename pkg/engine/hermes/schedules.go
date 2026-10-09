package hermes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
)

// Hermes keeps scheduled jobs in cron/jobs.json ({"jobs": [...]}, or a bare list), compiled from
// agents/<id>/hermes/cron.jobs.json and changed at runtime by the scheduler and the agent.

// MergeSchedules keeps the repo's jobs and the ones the agent made at runtime (mergeCron).
func (h *Hermes) MergeSchedules(baseline, live []byte) ([]byte, error) {
	out, err := mergeCron(string(baseline), string(live))
	return []byte(out), err
}

// Schedules reads cron/jobs.json.
func (h *Hermes) Schedules(body []byte) ([]engine.ScheduleStatus, error) {
	var file struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		var list []map[string]any
		if json.Unmarshal(body, &list) != nil {
			return nil, err
		}
		file.Jobs = list
	}
	out := []engine.ScheduleStatus{}
	for _, j := range file.Jobs {
		str := func(k string) string {
			if v, ok := j[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
		s := engine.ScheduleStatus{ID: str("id"), Name: str("name"), LastStatus: str("last_status"),
			LastError: str("last_error"), DeliveryError: str("last_delivery_error")}
		s.Enabled, _ = j["enabled"].(bool)
		if s.Name == "" {
			s.Name = s.ID
		}
		// "silent" and "no_change" are Hermes' successful runs that had nothing to say.
		s.Failed = s.LastStatus != "" && s.LastStatus != "ok" && s.LastStatus != "silent" && s.LastStatus != "no_change"
		if n, ok := j["failure_streak"].(float64); ok {
			s.FailureStreak = int(n)
		}
		if t, err := time.Parse(time.RFC3339Nano, str("next_run_at")); err == nil {
			s.NextRunAt = t
		}
		out = append(out, s)
	}
	return out, nil
}

// LogNoise is Hermes log output already understood.
func (h *Hermes) LogNoise() []engine.LogNoise {
	return []engine.LogNoise{
		{Re: regexp.MustCompile(`platform '(teams|google_chat)' has no valid toolsets`), Why: "unused platform toolsets in the ported config"},
		{Re: regexp.MustCompile(`API server is network-accessible .* terminal backend is 'local'`), Why: "by design: the task is the sandbox"},
		{Re: regexp.MustCompile(`journal_mode was delete and has been switched to WAL`), Why: "Hermes state.db on task-local disk (wal is the swarm override)"},
		{Re: regexp.MustCompile("As you gave `client` as well, `token` will be unused"), Why: "slack_bolt notice"},
	}
}

// mergeCron: baseline jobs win by id (the repo is the source of truth); jobs the agent created at
// runtime are kept. Works on raw JSON so every job keeps its own key order.
func mergeCron(baseline, live string) (string, error) {
	if strings.TrimSpace(live) == "" {
		return baseline, nil
	}
	if strings.TrimSpace(baseline) == "" {
		return live, nil
	}
	type job = json.RawMessage
	jobsOf := func(s string) ([]job, []string, map[string]json.RawMessage, bool, error) {
		var arr []job
		if json.Unmarshal([]byte(s), &arr) == nil {
			return arr, nil, nil, true, nil
		}
		keys, obj, err := orderedObject([]byte(s))
		if err != nil {
			return nil, nil, nil, false, err
		}
		if raw, ok := obj["jobs"]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, nil, nil, false, err
			}
		}
		return arr, keys, obj, false, nil
	}
	bJobs, bKeys, bObj, bArr, err := jobsOf(baseline)
	if err != nil {
		return "", err
	}
	lJobs, _, _, _, err := jobsOf(live)
	if err != nil {
		return "", err
	}
	idOf := func(j job) string {
		var x struct {
			ID any `json:"id"`
		}
		_ = json.Unmarshal(j, &x)
		b, _ := json.Marshal(x.ID)
		return string(b)
	}
	ids := map[string]bool{}
	for _, j := range bJobs {
		ids[idOf(j)] = true
	}
	merged := append([]job{}, bJobs...)
	for _, j := range lJobs {
		if !ids[idOf(j)] {
			merged = append(merged, j)
		}
	}
	var mb bytes.Buffer
	enc := json.NewEncoder(&mb)
	enc.SetEscapeHTML(false) // keep "<" in prompts as written
	if err := enc.Encode(merged); err != nil {
		return "", err
	}
	mergedRaw := bytes.TrimSpace(mb.Bytes())
	var out []byte
	if bArr {
		out = mergedRaw
	} else {
		if _, ok := bObj["jobs"]; !ok {
			bKeys = append(bKeys, "jobs")
		}
		bObj["jobs"] = mergedRaw
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range bKeys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			b.Write(bObj[k])
		}
		b.WriteByte('}')
		out = b.Bytes()
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		return "", err
	}
	return pretty.String(), nil
}

// orderedObject decodes a JSON object keeping its key order.
func orderedObject(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("cron jobs file is neither a list nor an object")
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
		k := kt.(string)
		if _, seen := obj[k]; !seen {
			keys = append(keys, k)
		}
		obj[k] = v
	}
	return keys, obj, nil
}
