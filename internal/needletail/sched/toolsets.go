package sched

import (
	"slices"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// CronJobTool is the scheduling tool's name (Hermes': its toolset is "cronjob").
const CronJobTool = "cronjob_manage"

// FollowUpTool is the one-time follow-ups' tool (package followup): a scheduled run has no
// conversation to come back to.
const FollowUpTool = "follow_up"

// toolsets are Hermes' toolset names a job's enabled_toolsets (agent.yaml's schedules.tools)
// uses, with the tools each stands for here. A toolset with no tool here (browser,
// code_execution, vision, …) offers nothing; a tool's own name stands for itself.
var toolsets = map[string][]string{
	"terminal": {"terminal", "process_manage"},
	"file":     {"read_file", "write_file", "patch", "search_files"},
	"skills":   {"skills_list", "skill_view", "skill_manage"},
	"memory":   {"memory", "memory_search"},
	// session_search is its own toolset in Hermes
	"session_search": {"session_search"},
	"cronjob":        {CronJobTool},
	"messaging":      {messaging.SendTool},
	"debugging":      {"terminal", "process_manage", "read_file", "write_file", "patch", "search_files"},
	// the public web: a search (when the agent has a provider) and a page's text; both mark the
	// run, so a scheduled run that reads the web has its report held for the owner (§4.7)
	"web": {"web_search", "web_extract"},
}

// neverInCron are tools a scheduled run is never offered, as in Hermes: scheduling (a job that
// makes jobs loops), messaging (the run's reply is what gets delivered; no_reply has no
// conversation to stay out of) and follow-ups (no conversation to come back to).
var neverInCron = []string{CronJobTool, messaging.SendTool, messaging.NoReplyTool, FollowUpTool}

// onlyByName are tools a scheduled run gets only when its list names them: memory_manage
// changes the memory archive, and a job's fixed prompt has no new knowledge to file unless its
// author says so (the memory tool, Hermes', stays in the default set).
var onlyByName = []string{"memory_manage"}

// noTools is a tool list that names no tool: loop.Options.Tools empty means every tool.
var noTools = []string{"(no tools)"}

// ExpandToolsets is the tools a job may use: every tool when enabled is empty, else the tools of
// each toolset (or tool) named, plus the core's tools (mcp_core_*) unless "no_mcp" is named, as
// Hermes layers its MCP servers onto a job's list; never the tools a scheduled run is denied.
func ExpandToolsets(enabled, all []string) []string {
	want := map[string]bool{}
	named := map[string]bool{}
	if len(enabled) == 0 {
		for _, t := range all {
			want[t] = true
		}
	} else {
		mcp := true
		for _, name := range enabled {
			name = strings.TrimSpace(name)
			switch {
			case name == "all" || name == "*":
				for _, t := range all {
					want[t] = true
				}
			case name == "no_mcp":
				mcp = false
			case toolsets[name] != nil:
				for _, t := range toolsets[name] {
					want[t] = true
				}
			default:
				want[name] = true
				named[name] = true
			}
		}
		if mcp {
			for _, t := range all {
				if strings.HasPrefix(t, "mcp_core_") {
					want[t] = true
				}
			}
		}
	}
	out := []string{}
	for _, t := range all {
		if want[t] && !slices.Contains(neverInCron, t) && (named[t] || !slices.Contains(onlyByName, t)) {
			out = append(out, t)
		}
	}
	return out
}

// toolsFor is a run's loop.Options.Tools.
func (s *Scheduler) toolsFor(j *Job) []string {
	var all []string
	if s.o.Tools != nil {
		all = s.o.Tools()
	}
	out := ExpandToolsets(j.Strings("enabled_toolsets"), all)
	if len(out) == 0 {
		return noTools
	}
	return out
}
