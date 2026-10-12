//go:build !windows

package privatefs

// ProtectWindows leaves existing Unix permissions unchanged.
func ProtectWindows(_ string) error { return nil }
