package sched

import (
	"fmt"
	"regexp"
	"strings"
)

// The scan of a job's prompt at create and update, ported from Hermes' strict cron prompt scan
// (tools/cronjob_prompt_scan.py and the invisible characters of tools/threat_patterns.py in the
// hermes-agent image v2026.9.24; Copyright (c) 2025 Nous Research, MIT License). A cron prompt
// runs unattended with the agent's tools, so a directive-shaped payload in it (an injection, a
// command that reads or leaks secrets) is refused outright. Hermes' looser scan of the assembled
// prompt (skill bodies included) is not ported (docs/schedules.md §8).

// secretVar is a shell variable that looks like a credential.
const secretVar = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)\w*\}?`

type threat struct {
	re *regexp.Regexp
	id string
}

func threats(pairs ...string) []threat {
	out := []threat{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, threat{regexp.MustCompile(`(?i)` + pairs[i]), pairs[i+1]})
	}
	return out
}

var (
	promptThreats = threats(
		`ignore\s+(?:\w+\s+)*(?:previous|all|above|prior)\s+(?:\w+\s+)*instructions`, "prompt_injection",
		`do\s+not\s+tell\s+the\s+user`, "deception_hide",
		`system\s+prompt\s+override`, "sys_prompt_override",
		`disregard\s+(your|all|any)\s+(instructions|rules|guidelines)`, "disregard_rules",
		`cat\s+[^\n]*(\.env|credentials|\.netrc|\.pgpass|id_rsa|id_ed25519|id_ecdsa)`, "read_secrets",
		`authorized_keys`, "ssh_backdoor",
		`/etc/sudoers|visudo`, "sudoers_mod",
		`rm\s+-rf\s+/`, "destructive_root_rm",
	)
	exfilThreats = threats(
		`curl\s+[^\n]*https?://[^\s"'`+"`"+`]*`+secretVar, "exfil_curl_url",
		`wget\s+[^\n]*https?://[^\s"'`+"`"+`]*`+secretVar, "exfil_wget_url",
		`curl\s+[^\n]*(?:--data(?:-raw|-binary|-urlencode)?|-d|--form|-F)\s+[^\n]*`+secretVar, "exfil_curl_data",
		`wget\s+[^\n]*--post-(?:data|file)=[^\n]*`+secretVar, "exfil_wget_post",
		`curl\s+[^\n]*(?:-H|--header)\s+["']Authorization:\s*(?:Bearer|token)\s+`+secretVar+`["']`, "exfil_curl_auth_header",
	)
	// githubCurl is the one Authorization-header curl a prompt may hold: a GitHub token to
	// api.github.com exactly (a lookalike host falls through and is scanned).
	githubCurl = regexp.MustCompile(`(?i)curl\s+[^\n;&|$` + "`" + `]*(?:-H|--header)\s+["']Authorization:\s*token\s+` + secretVar + `["']` +
		`\s+["']?https://api\.github\.com(?::\d+)?(?:/|\s|$|["'])[^\s;&|$` + "`" + `]*`)
)

// invisibleChars are the zero-width and bidirectional controls a prompt may not hold, in the
// order they are reported.
var invisibleChars = []rune{'\u200b', '\u200c', '\u200d', '\u2060', '\u2062', '\u2063', '\u2064', '\ufeff',
	'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069'}

// ScanPrompt is why a job's prompt is refused, or "" (Hermes' _scan_cron_prompt).
func ScanPrompt(prompt string) string {
	p := githubCurl.ReplaceAllString(prompt, "curl https://api.github.com/user")
	found := invisibleIn(p)
	for _, c := range invisibleChars {
		if found[c] {
			return fmt.Sprintf("Blocked: prompt contains invisible unicode U+%04X (possible injection).", c)
		}
	}
	for _, set := range [][]threat{promptThreats, exfilThreats} {
		for _, t := range set {
			if t.re.MatchString(p) {
				return fmt.Sprintf("Blocked: prompt matches threat pattern '%s'. Cron prompts must not contain injection or exfiltration payloads.", t.id)
			}
		}
	}
	return ""
}

// invisibleIn is the invisible characters in s; a zero-width joiner between two emoji (a family,
// a flag) is part of the emoji and not counted.
func invisibleIn(s string) map[rune]bool {
	rs := []rune(s)
	out := map[rune]bool{}
	for i, r := range rs {
		if !strings.ContainsRune(string(invisibleChars), r) {
			continue
		}
		if r == '\u200d' && emojiAround(rs, i) {
			continue
		}
		out[r] = true
	}
	return out
}

func emojiAround(rs []rune, i int) bool {
	l, r := i-1, i+1
	for l >= 0 && rs[l] == '\ufe0f' {
		l--
	}
	for r < len(rs) && rs[r] == '\ufe0f' {
		r++
	}
	return l >= 0 && r < len(rs) && emoji(rs[l]) && emoji(rs[r])
}

func emoji(r rune) bool {
	return r >= 0x1F000 && r <= 0x1FFFF || r >= 0x2600 && r <= 0x27BF || r >= 0x2300 && r <= 0x23FF || r == 0x20E3
}
