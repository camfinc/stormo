//go:build !linux

package cli

// undumpable has nothing to do outside Linux: there is no /proc/<pid>/environ to hide (the
// runtime runs on Linux in its container; elsewhere is development).
func undumpable() error { return nil }
