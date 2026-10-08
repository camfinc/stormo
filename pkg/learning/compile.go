package learning

import (
	_ "embed"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
)

// Two tiers instead of Hermes' single 2.2k-char memory:
//   - hot:  the highest-ranked accepted learnings, packed into seed_fill of the char budget and
//     merged into MEMORY.md / USER.md at boot (always in the prompt);
//   - cold: every visible accepted learning plus human-authored unit/group knowledge, shipped as the
//     knowledge skill (stormo.yaml names.knowledge_skill) the agent loads on demand.

//go:embed templates/knowledge-SKILL.md
var knowledgeTemplate string

var newlines = regexp.MustCompile(`\n+`)

func section(title string, entries []*Learning) string {
	if len(entries) == 0 {
		return ""
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = "- " + newlines.ReplaceAllString(e.Text, " ") + "  `" + e.ID + "`"
	}
	return "## " + title + "\n\n" + strings.Join(lines, "\n") + "\n\n"
}

func humanKnowledge(root, unit string) (string, error) {
	dir := filepath.Join(root, "units", unit, "knowledge")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && e.Name() != "README.md" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, f := range names {
		body, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		b.WriteString("### " + strings.TrimSuffix(f, ".md") + "\n\n" + Trim(string(body)) + "\n\n")
	}
	return b.String(), nil
}

func orNone(s string) string {
	if s == "" {
		return "_none yet_\n"
	}
	return s
}

// CompileLearning builds the hot-tier seed and the cold-tier skills for an agent.
func CompileLearning(inst *instance.Instance, agentID string) (engine.CompileContext, error) {
	var ctx engine.CompileContext
	agent, err := manifest.Load(inst.Root, agentID, inst.Names.Secret)
	if err != nil {
		return ctx, err
	}
	visible, err := VisibleLearnings(inst, agentID)
	if err != nil {
		return ctx, err
	}
	budget := func(limit int) int { return int(math.Floor(float64(limit) * agent.Learning.SeedFill)) }
	hot := func(kind Kind, limit int) []string {
		texts := []string{}
		// Own lessons compete for the hot tier; shared ones only when a reviewer pinned them.
		for _, e := range visible {
			if e.Kind == kind && (e.Agent == agentID || e.IsPinned()) {
				texts = append(texts, e.Text)
			}
		}
		kept, _ := Pack(texts, budget(limit))
		return kept
	}
	var own, unit, group []*Learning
	for _, e := range visible {
		switch {
		case e.Agent == agentID && e.Kind == KindMemory:
			own = append(own, e)
		}
		if e.Scope == manifest.ScopeUnit && e.Agent != agentID {
			unit = append(unit, e)
		}
		if e.Scope == manifest.ScopeGroup && e.Agent != agentID {
			group = append(group, e)
		}
	}
	unitDocs, err := humanKnowledge(inst.Root, agent.Unit)
	if err != nil {
		return ctx, err
	}
	groupDocs, err := humanKnowledge(inst.Root, "group")
	if err != nil {
		return ctx, err
	}
	ks := inst.Names.KnowledgeSkill
	r := strings.NewReplacer(
		"zzknowledgezz", ks,
		"zzresourcezz", inst.Names.Resource,
		"zzslugzz", inst.Slug,
		"ZZORGZZ", inst.Org,
		"ZZAGENTNAMEZZ", agent.Name,
		"zzunitzz", agent.Unit,
	)
	knowledge := map[string]string{
		ks + "/SKILL.md":            r.Replace(knowledgeTemplate),
		ks + "/references/agent.md": "# " + agent.Name + "\n\n" + orNone(section("Accepted lessons", own)),
		ks + "/references/unit.md":  "# Unit: " + agent.Unit + "\n\n" + orNone(unitDocs+section("Promoted lessons", unit)),
		ks + "/references/group.md": "# " + inst.Org + " group\n\n" + orNone(groupDocs+section("Promoted lessons", group)),
	}
	for p, body := range shared.Skill(inst, agent) {
		knowledge[p] = body
	}
	ctx.Instance = inst
	ctx.Seed.Memory = hot(KindMemory, agent.Learning.MemoryCharLimit)
	ctx.Seed.User = hot(KindUser, agent.Learning.UserCharLimit)
	ctx.Knowledge = knowledge
	return ctx, nil
}
