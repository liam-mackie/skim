//go:build unix

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Start starts cmd as the leader of a new process group and makes
// CommandContext cancellation kill the entire group. Pair a successful Start
// with cmd.Wait, then Kill and Wait.
func Start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return Kill(cmd) }
	}
	return cmd.Start()
}

// Kill signals all processes in the command's group, including orphaned tools.
// Wait must still be called after reaping the direct child.
func Kill(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Wait runs after the direct child has been reaped. SIGKILL delivery
// alone is asynchronous: a descendant may still have a workspace file open.
// Orphan zombies cannot execute or hold files, and only their new parent can
// reap them. Do not wait for that parent's reaping policy (notably container PID
// 1); do wait until every remaining group member is a zombie or has disappeared.
func Wait(cmd *exec.Cmd) {
	group := cmd.Process.Pid
	for {
		if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
			return
		}
		if stopped, err := Stopped(group); err == nil && stopped {
			return
		}
		// Fail closed if the process table cannot be inspected. Never release a
		// workspace merely because a timeout elapsed while a child can write.
		_ = syscall.Kill(-group, syscall.SIGKILL)
		time.Sleep(10 * time.Millisecond)
	}
}

// Stopped reports whether no executing process remains in a process group.
// Unreaped zombies are stopped: they cannot execute or retain workspace files.
func Stopped(group int) (bool, error) {
	// Only group IDs and states are requested; command lines may hold secrets.
	// /bin/ps is available on the supported Unix hosts, independent of PATH.
	command := exec.Command("/bin/ps", "-A", "-o", "pgid=", "-o", "stat=")
	output, err := command.Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return false, errors.New("cannot inspect owned process group")
		}
		pgid, err := strconv.Atoi(fields[0])
		if err != nil {
			return false, errors.New("cannot inspect owned process group")
		}
		if pgid == group && !strings.HasPrefix(fields[1], "Z") {
			return false, nil
		}
	}
	return true, nil
}
