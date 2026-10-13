//go:build !unix && !windows

package processgroup

import (
	"errors"
	"os/exec"
)

var errUnsupported = errors.New("safe subprocess execution requires Unix process groups or Windows job objects")

// Start rejects this platform before a process can be started.
func Start(cmd *exec.Cmd) error { return errUnsupported }

func Kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }

// Wait is unreachable: Start never succeeds on this platform.
func Wait(cmd *exec.Cmd) {
	panic("process-group cleanup is unsupported on this host")
}

// Stopped cannot prove that a group stopped on this platform.
func Stopped(group int) (bool, error) { return false, errUnsupported }
