//go:build !linux

package privsep

import "errors"

// Keep is the capability mask the runtime runs with (Linux only).
const Keep = 0

// Drop is Linux only: elsewhere the runtime runs as one user (development).
func Drop(Users) error { return errors.New("the uid split runs on Linux only") }

// Verify is Linux only.
func Verify(Users) error { return nil }

// Bounding is Linux only.
func Bounding() (uint64, bool) { return 0, true }
