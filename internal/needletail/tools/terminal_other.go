//go:build !unix

package tools

import (
	"errors"
	"os"
	"os/exec"
)

func start(*exec.Cmd, *User) {}

func killGroup(int, bool) error { return errors.New("process groups need unix") }

func signalOf(*os.ProcessState) string { return "" }
