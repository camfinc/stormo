package fsx

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// RenameBelow moves an entry between descriptor-opened parents without replacing
// an existing destination. Links in either parent path are refused. The source
// itself must be a directory, never a symbolic link.
func RenameBelow(root, src, dst string) error {
	from, name, err := parent(root, src, false)
	if err != nil {
		return err
	}
	defer unix.Close(from)
	to, target, err := parent(root, dst, false)
	if err != nil {
		return err
	}
	defer unix.Close(to)
	var st unix.Stat_t
	if err = unix.Fstatat(from, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("rename source is not a directory")
	}
	if err = renameExclusive(from, name, to, target); err != nil {
		return err
	}
	if err = unix.Fsync(from); err != nil {
		return err
	}
	return unix.Fsync(to)
}
