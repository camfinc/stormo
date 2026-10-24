//go:build unix

package tools

import "syscall"

// Open flags of the file tools: never follow a symlink in the final component (one swapped in
// after the path was resolved), and never block in open (a FIFO would wait for a writer).
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonblock = syscall.O_NONBLOCK
)
