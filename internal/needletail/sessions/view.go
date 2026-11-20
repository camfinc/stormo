package sessions

import (
	"fmt"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// SwarmReads are the core's tools that read swarm messages: their results stay in context after
// taint decay (for a conversation the swarm woke, the message is the work), so the screen judges
// them instead.
var SwarmReads = []string{"mcp_core_msg_read", "mcp_core_msg_inbox", "mcp_core_msg_thread"}

// Out is whether row i of rows (a session's, whose Evicted is evicted; names its tool calls by id)
// is one taint decay keeps out of context: before evicted, a tool result but a swarm message read
// (by position, whatever its mark: an endorsement or a compaction may have cleared it), or a
// marked person row (an outsider's words).
func Out(rows []wire.Message, i, evicted int, names map[string]string) bool {
	if i >= evicted {
		return false
	}
	switch r := rows[i]; r.Role {
	case "tool":
		name := names[r.ToolCallID]
		for _, s := range SwarmReads {
			if name == s {
				return false
			}
		}
		return true
	case "user":
		return r.Untrusted
	}
	return false
}

// ToolNames are rows' tool calls' names by id.
func ToolNames(rows []wire.Message) map[string]string {
	names := map[string]string{}
	for _, r := range rows {
		for _, c := range r.ToolCalls {
			names[c.ID] = c.Name
		}
	}
	return names
}

// View is rows as the model sees them (docs/needletail.md §7, "Taint decay"): the rows Out keeps
// out of context replaced by a stub that says what was there, the rest as stored. rows is left
// alone; with evicted 0 it is returned as it is.
func View(rows []wire.Message, evicted int) []wire.Message {
	if evicted <= 0 {
		return rows
	}
	names := ToolNames(rows)
	var out []wire.Message
	for i := range rows {
		if !Out(rows, i, evicted, names) {
			continue
		}
		if out == nil {
			out = append([]wire.Message{}, rows...)
		}
		out[i].Content = stub(rows[i], names)
	}
	if out == nil {
		return rows
	}
	return out
}

func stub(r wire.Message, names map[string]string) string {
	at := r.At
	if at == "" {
		at = "earlier"
	}
	if r.Role == "user" {
		return fmt.Sprintf("[Taken out of context by the runtime (taint decay): a message from someone who may not instruct you, %s.]", at)
	}
	name := strings.TrimSpace(names[r.ToolCallID])
	if name == "" {
		name = "a tool"
	}
	return fmt.Sprintf("[Taken out of context by the runtime (taint decay): the result of %s, %s. Call it again if you need it.]", name, at)
}
