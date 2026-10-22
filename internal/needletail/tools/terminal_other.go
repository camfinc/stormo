//go:build !unix

package tools

import "os/exec"

func start(*exec.Cmd, *User) {}
