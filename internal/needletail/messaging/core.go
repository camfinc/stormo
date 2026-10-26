package messaging

// ReadOnlyCoreTool is the closed set of core operations a quarantined reader may
// invoke. Unknown/new tools are writes until explicitly reviewed here.
func ReadOnlyCoreTool(name string) bool {
	switch name {
	case "msg_inbox", "msg_read", "msg_thread", "fs_ls", "fs_info", "fs_locks", "cap_list", "cap_status":
		return true
	default:
		return false
	}
}
