package fsx

import "golang.org/x/sys/unix"

func renameExclusive(from int, name string, to int, target string) error {
	return unix.Renameat2(from, name, to, target, unix.RENAME_NOREPLACE)
}
