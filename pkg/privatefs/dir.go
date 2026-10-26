// Package privatefs creates directories for credentials and other private state.
package privatefs

import "os"

// MkdirAll keeps POSIX mode semantics and applies an inheritable current-user/SYSTEM
// DACL on Windows, where chmod(0700) cannot express access control.
func MkdirAll(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return ProtectWindows(path)
}
