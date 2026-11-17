package messaging

// ReadOnlyCoreTool is the closed set of core operations a quarantined reader may
// invoke. Unknown/new tools are writes until explicitly reviewed here.
func ReadOnlyCoreTool(name string) bool {
	switch name {
	case "msg_inbox", "msg_read", "msg_thread", "fs_ls", "fs_info", "fs_locks", "cap_list", "cap_describe", "cap_status", "cap_jobs":
		return true
	default:
		return false
	}
}

// CoordinationCoreTool is the closed set of core writes that move no content, which a run
// holding untrusted input may call without a person: an acknowledgement in its own inbox, a lock
// on a shared path (its reason, at most 200 characters, is the only text), a note of who changed
// a shared file.
func CoordinationCoreTool(name string) bool {
	switch name {
	case "msg_ack", "fs_lock", "fs_unlock", "fs_renew", "fs_note":
		return true
	default:
		return false
	}
}

// MarkCarryingCoreTool is a core write a run holding untrusted input may call because the core
// keeps the run's mark on what it writes (the call's _meta), and whoever reads it is marked in
// turn: a message to another agent of the swarm. Talk inside the swarm needs no person; what a
// marked reader then sends out of it does.
func MarkCarryingCoreTool(name string) bool { return name == "msg_send" }

// MarkedRunCoreTool is whether a run holding untrusted input may call the core's tool name
// without a person.
func MarkedRunCoreTool(name string) bool {
	return ReadOnlyCoreTool(name) || CoordinationCoreTool(name) || MarkCarryingCoreTool(name)
}

// InternalCoreTool is a core write a run holding untrusted input may call without a person when
// the agent's approvals.marked is internal, because what it carries reaches only an audience the
// owner configured: a capability job (a worker of the swarm's own, granted in agent.yaml) or a
// WhatsApp message (the core sends only to the channel's allowed_users and allowed_groups).
// fs_write is not one: a shared file keeps no mark, so its readers would take it as trusted.
func InternalCoreTool(name string) bool {
	switch name {
	case "cap_start", "cap_run", "cap_cancel", "whatsapp_send":
		return true
	default:
		return false
	}
}

// MarkedRunMay is MarkedRunCoreTool with the agent's approvals.marked: internal adds
// InternalCoreTool.
func MarkedRunMay(name string, internal bool) bool {
	return MarkedRunCoreTool(name) || internal && InternalCoreTool(name)
}
