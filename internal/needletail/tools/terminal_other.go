//go:build !unix

package tools

import "os/exec"

func command(*exec.Cmd, *User) {}
