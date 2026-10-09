package hermes

import (
	"regexp"
	"strings"
)

// Skills are written in Stormo's form (the Agent Skills format: metadata tags and related skills
// directly under `metadata:`, `${SKILL_DIR}` for the skill's own directory); renderSkill turns one
// SKILL.md into Hermes': metadata nested under `metadata.hermes`, `${HERMES_SKILL_DIR}`. A skill
// already in Hermes' form is left as it is.
func renderSkill(body string) string {
	body = strings.ReplaceAll(body, "${SKILL_DIR}", "${HERMES_SKILL_DIR}")
	fm := frontmatter.FindStringIndex(body)
	if fm == nil {
		return body
	}
	head := body[:fm[1]]
	m := metadataBlock.FindStringSubmatchIndex(head)
	if m == nil {
		return body
	}
	block := head[m[2]:m[3]]
	if strings.HasPrefix(block, "  hermes:") {
		return body
	}
	nested := "  hermes:\n" + indented.ReplaceAllString(block, "  $1")
	return head[:m[2]] + nested + head[m[3]:] + body[fm[1]:]
}

var (
	frontmatter   = regexp.MustCompile(`(?s)\A---\r?\n.*?\r?\n---[ \t]*(?:\r?\n|\z)`)
	metadataBlock = regexp.MustCompile(`(?m)^metadata:[ \t]*\n((?:[ \t]+.*\n)+)`)
	indented      = regexp.MustCompile(`(?m)^([ \t]+)`)
)
