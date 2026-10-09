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
	"github.com/camfinc/stormo/pkg/manifest"
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

// mergeCron: for a job in both, the baseline's definition wins (the repo is the source of truth)
// and the live record keeps its run state (last and next run, failures, runs completed); jobs the
// agent created at runtime are kept. Works on raw JSON so every job keeps its own key order.
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
	liveByID := map[string]job{}
	for _, j := range lJobs {
		liveByID[idOf(j)] = j
	}
	ids := map[string]bool{}
	merged := []job{}
	for _, j := range bJobs {
		ids[idOf(j)] = true
		if l, ok := liveByID[idOf(j)]; ok {
			m, err := overlayDefinition(l, j)
			if err != nil {
				return "", err
			}
			j = m
		}
		merged = append(merged, j)
	}
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

// jobKeys are a Hermes job record's keys in the order Hermes writes them. definitionKeys are the
// ones a schedule (agent.yaml) decides; the rest is run state the scheduler keeps.
var (
	jobKeys = []string{"id", "name", "prompt", "skills", "skill", "model", "provider", "base_url", "script", "no_agent",
		"monitor_script", "monitor_url", "monitor_state", "context_from", "schedule", "schedule_display", "repeat", "enabled",
		"state", "paused_at", "paused_reason", "created_at", "next_run_at", "last_run_at", "last_status", "last_error",
		"last_delivery_error", "deliver", "origin", "enabled_toolsets", "workdir", "failure_deliver"}
	definitionKeys = map[string]bool{"id": true, "name": true, "prompt": true, "skills": true, "skill": true, "model": true,
		"provider": true, "base_url": true, "script": true, "no_agent": true, "monitor_script": true, "monitor_url": true,
		"context_from": true, "schedule": true, "schedule_display": true, "enabled": true, "state": true, "paused_reason": true,
		"deliver": true, "enabled_toolsets": true, "workdir": true, "failure_deliver": true}
)

// renderJobs is cron/jobs.json for the agent's schedules: every key Hermes writes, run state empty
// (the scheduler computes the next run of a recurring job that has none).
func renderJobs(schedules []manifest.Schedule) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n  \"jobs\": [")
	for i, s := range schedules {
		var schedule any
		display := s.Cron
		if m := s.Minutes(); m > 0 {
			display = fmt.Sprintf("every %dm", m)
			schedule = ordered{{"kind", "interval"}, {"minutes", m}, {"display", display}}
		} else {
			schedule = ordered{{"kind", "cron"}, {"expr", s.Cron}, {"display", display}}
		}
		state := "scheduled"
		if !s.Enabled {
			state = "paused"
		}
		var times any
		if s.Times > 0 {
			times = s.Times
		}
		v := map[string]any{"id": s.ID, "name": s.Name, "prompt": s.Prompt, "skills": orEmpty(s.Skills), "skill": first(s.Skills),
			"model": null(s.Model), "provider": null(s.Provider), "script": null(s.Script), "no_agent": !s.Agent,
			"monitor_script": null(s.Monitor), "schedule": schedule, "schedule_display": display,
			"repeat": ordered{{"times", times}, {"completed", 0}}, "enabled": s.Enabled, "state": state,
			"paused_reason": null(s.Note), "deliver": null(s.Deliver), "enabled_toolsets": nullList(s.Tools),
			"failure_deliver": null(s.OnFailure)}
		job := ordered{}
		for _, k := range jobKeys {
			job = append(job, kv{k, v[k]})
		}
		raw, err := marshal(job)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n    ")
		b.Write(bytes.ReplaceAll(raw, []byte("\n"), []byte("\n    ")))
	}
	if len(schedules) > 0 {
		b.WriteString("\n  ")
	}
	b.WriteString("]\n}\n")
	return b.Bytes(), nil
}

// ReadSchedules reads a Hermes cron/jobs.json back as schedules: the definition only.
func (h *Hermes) ReadSchedules(body []byte) ([]manifest.Schedule, error) {
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
	out := []manifest.Schedule{}
	for _, j := range file.Jobs {
		str := func(k string) string {
			s, _ := j[k].(string)
			return s
		}
		s := manifest.Schedule{ID: fmt.Sprint(j["id"]), Name: str("name"), Prompt: str("prompt"), Script: str("script"),
			Monitor: str("monitor_script"), Model: str("model"), Provider: str("provider"), Deliver: str("deliver"),
			OnFailure: str("failure_deliver"), Note: str("paused_reason")}
		s.Agent = j["no_agent"] != true
		s.Enabled = j["enabled"] != false
		for _, k := range []string{"skills", "enabled_toolsets"} {
			list, _ := j[k].([]any)
			for _, x := range list {
				if t, ok := x.(string); ok {
					if k == "skills" {
						s.Skills = append(s.Skills, t)
					} else {
						s.Tools = append(s.Tools, t)
					}
				}
			}
		}
		if len(s.Skills) == 0 && str("skill") != "" {
			s.Skills = []string{str("skill")}
		}
		sch, _ := j["schedule"].(map[string]any)
		switch sch["kind"] {
		case "interval":
			if m, ok := sch["minutes"].(float64); ok {
				s.Every = fmt.Sprintf("%dm", int(m))
			}
		case "cron":
			s.Cron, _ = sch["expr"].(string)
		default:
			return nil, fmt.Errorf("job %s: schedule kind %v has no agent.yaml form (every, cron)", s.ID, sch["kind"])
		}
		if r, ok := j["repeat"].(map[string]any); ok {
			if t, ok := r["times"].(float64); ok {
				s.Times = int(t)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

type kv struct {
	k string
	v any
}

// ordered is a JSON object that keeps its key order.
type ordered []kv

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.k)
		b.Write(k)
		b.WriteByte(':')
		v, err := marshalCompact(p.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep "<" in prompts as written
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

// marshal is v as Hermes writes JSON: two-space indent, ensure_ascii off.
func marshal(v any) ([]byte, error) {
	raw, err := marshalCompact(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullList(l []string) any {
	if len(l) == 0 {
		return nil
	}
	return l
}

func first(l []string) any {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// overlayDefinition is the live job with the baseline's definition keys (and repeat.times) on it.
func overlayDefinition(live, base json.RawMessage) (json.RawMessage, error) {
	lKeys, lObj, err := orderedObject(live)
	if err != nil {
		return nil, err
	}
	bKeys, bObj, err := orderedObject(base)
	if err != nil {
		return nil, err
	}
	for _, k := range bKeys {
		if !definitionKeys[k] {
			continue
		}
		if _, ok := lObj[k]; !ok {
			lKeys = append(lKeys, k)
		}
		lObj[k] = bObj[k]
	}
	if br, ok := bObj["repeat"]; ok {
		var b, l map[string]json.RawMessage
		if json.Unmarshal(br, &b) == nil && json.Unmarshal(lObj["repeat"], &l) == nil && l != nil {
			l["times"] = b["times"]
			raw, _ := json.Marshal(ordered{{"times", l["times"]}, {"completed", l["completed"]}})
			lObj["repeat"] = raw
		} else if _, had := lObj["repeat"]; !had {
			lKeys = append(lKeys, "repeat")
			lObj["repeat"] = br
		}
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range lKeys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(lObj[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
