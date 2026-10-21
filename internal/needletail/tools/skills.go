package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/skills"
)

// The skill tools (docs/needletail.md §4.5, §4.6), Hermes' names and arguments: skills_list,
// skill_view (load a skill progressively, ${SKILL_DIR} resolved) and skill_manage (the agent's own
// skills). Their use is counted in skills/.usage.json.

func skillsOf(env *Env) (*skills.Library, error) {
	if env == nil || env.Skills == nil {
		return nil, errors.New("skills are not available")
	}
	return env.Skills, nil
}

func skillsListTool() Tool {
	return Tool{
		Name:        "skills_list",
		Description: "List your skills: name, description and directory (under skills/). Load one with skill_view.",
		Schema: schema(`{"type":"object","properties":{
"category":{"type":"string","description":"Only skills under this directory of skills/ (e.g. crm)."}}}`),
		Run: func(_ context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Category string `json:"category"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			lib, err := skillsOf(env)
			if err != nil {
				return nil, err
			}
			type entry struct {
				Name        string `json:"name"`
				Description string `json:"description,omitempty"`
				Dir         string `json:"dir"`
				ReadOnly    bool   `json:"read_only,omitempty"`
			}
			out := []entry{}
			cat := strings.Trim(a.Category, "/")
			for _, s := range lib.Index() {
				if cat == "" || strings.HasPrefix(s.Dir, cat+"/") {
					out = append(out, entry{s.Name, s.Description, s.Dir, lib.ReadOnly(s.Dir)})
				}
			}
			return map[string]any{"skills": out, "count": len(out)}, nil
		},
	}
}

func skillViewTool() Tool {
	return Tool{
		Name: "skill_view",
		Description: "Load a skill before a task it covers: its SKILL.md instructions and the list of its other files " +
			"(references, templates, scripts). Pass file_path to load one of those files. ${SKILL_DIR} in the text is " +
			"already resolved to the skill's directory; run its scripts with the terminal (pass skill so SKILL_DIR is set).",
		Schema: schema(`{"type":"object","properties":{
"name":{"type":"string","description":"The skill's name or its directory under skills/ (e.g. crm/crm-api)."},
"file_path":{"type":"string","description":"A file of the skill, relative to its directory (e.g. references/api.md); omit for SKILL.md."}},
"required":["name"]}`),
		Run: func(_ context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Name     string `json:"name"`
				FilePath string `json:"file_path"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			lib, err := skillsOf(env)
			if err != nil {
				return nil, err
			}
			return lib.View(env.Session, a.Name, a.FilePath)
		},
	}
}

func skillManageTool() Tool {
	return Tool{
		Name: "skill_manage",
		Description: "Create and improve your own skills under skills/, so what you learned to do well is there next time " +
			"(a person reviews your changes before they reach the agent's repository). Actions: " +
			"create (name, optional category, content: the whole SKILL.md, starting with front matter ---\\nname: <name>\\ndescription: <when to use it>\\n---); " +
			"edit (name, content: the whole new SKILL.md); " +
			"patch (name, old_string, new_string, optional file_path, replace_all; old_string must match exactly once unless replace_all); " +
			"write_file (name, file_path relative to the skill, file_content: a reference, template or script); " +
			"remove_file (name, file_path); delete (name: the whole skill; one from the agent's repository is not deleted at runtime). " +
			"Write ${SKILL_DIR} for the skill's own directory (its absolute path, as a view shows it, is put back as ${SKILL_DIR}) and env var names (never values) for credentials. " +
			"Skills the build generates (the knowledge and shared-docs skills) cannot be changed.",
		Schema: schema(`{"type":"object","properties":{
"action":{"type":"string","enum":["create","edit","patch","write_file","remove_file","delete"]},
"name":{"type":"string","description":"The skill's name (create: lowercase letters, digits, - and _), or its directory under skills/."},
"category":{"type":"string","description":"create: the directory under skills/ to put it in (e.g. crm)."},
"content":{"type":"string","description":"create, edit: the complete SKILL.md."},
"file_path":{"type":"string","description":"patch, write_file, remove_file: a file relative to the skill's directory (patch: omit for SKILL.md)."},
"file_content":{"type":"string","description":"write_file: the file's complete content."},
"old_string":{"type":"string","description":"patch: the exact text to replace."},
"new_string":{"type":"string","description":"patch: the text to put in its place."},
"replace_all":{"type":"boolean","description":"patch: replace every occurrence."}},
"required":["action","name"]}`),
		Run: runSkillManage,
	}
}

type manageArgs struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Content     string `json:"content"`
	FilePath    string `json:"file_path"`
	FileContent string `json:"file_content"`
	OldString   string `json:"old_string"`
	NewString   string `json:"new_string"`
	ReplaceAll  bool   `json:"replace_all"`
}

// errSecret is the refusal of text holding a secret value the runtime knows: a skill is reviewed
// into git and read by people, so it names env vars instead.
var errSecret = errors.New("the text holds a secret value: write the env var's name (e.g. $CRM_API_TOKEN) instead")

// SecretCheck is a skills.Library's Check for a runtime's redactor: it refuses text holding a
// secret value redact knows.
func SecretCheck(redact func(string) string) func(string) error {
	return func(text string) error {
		if redact != nil && text != "" && redact(text) != text {
			return errSecret
		}
		return nil
	}
}

// noSecrets refuses a call whose arguments hold a secret value the runtime knows (the library's
// Check sees the files they would leave).
func noSecrets(env *Env, texts ...string) error {
	check := SecretCheck(env.Redact)
	for _, t := range texts {
		if err := check(t); err != nil {
			return err
		}
	}
	return nil
}

func runSkillManage(_ context.Context, env *Env, raw json.RawMessage) (any, error) {
	var a manageArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	lib, err := skillsOf(env)
	if err != nil {
		return nil, err
	}
	if err := noSecrets(env, a.Name, a.Category, a.FilePath, a.Content, a.FileContent, a.NewString); err != nil {
		return nil, err
	}
	done := func(action string, s skills.Skill, extra map[string]any) map[string]any {
		out := map[string]any{"success": true, "action": action, "name": s.Name, "dir": s.Dir, "path": s.Path}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	switch a.Action {
	case "create":
		s, err := lib.Create(strings.TrimSpace(a.Name), strings.Trim(strings.TrimSpace(a.Category), "/"), a.Content)
		if err != nil {
			return nil, err
		}
		return done("create", s, map[string]any{"skill_dir": lib.Abs(s)}), nil
	case "edit":
		s, err := lib.Edit(a.Name, a.Content)
		if err != nil {
			return nil, err
		}
		return done("edit", s, nil), nil
	case "patch":
		s, file, n, err := lib.Patch(a.Name, a.FilePath, a.OldString, a.NewString, a.ReplaceAll)
		if err != nil {
			return nil, err
		}
		return done("patch", s, map[string]any{"file": file, "replacements": n}), nil
	case "write_file":
		content := a.FileContent
		if content == "" {
			content = a.Content
		}
		s, file, err := lib.WriteFile(a.Name, a.FilePath, content)
		if err != nil {
			return nil, err
		}
		return done("write_file", s, map[string]any{"file": file}), nil
	case "remove_file":
		s, file, err := lib.RemoveFile(a.Name, a.FilePath)
		if err != nil {
			return nil, err
		}
		return done("remove_file", s, map[string]any{"file": file}), nil
	case "delete":
		s, err := lib.Delete(a.Name)
		if err != nil {
			return nil, err
		}
		return done("delete", s, nil), nil
	}
	return nil, fmt.Errorf("action must be create, edit, patch, write_file, remove_file or delete")
}
