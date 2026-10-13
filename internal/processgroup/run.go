// Package processgroup keeps owned subprocess trees from outliving workspace leases.
package processgroup

import (
	"os/exec"
	"time"
)

// Run is exec.Cmd.Run with group-wide cancellation and mandatory descendant
// cleanup. Even a successful parent may leave children holding inherited pipes;
// WaitDelay bounds that drain, then Kill and Wait stop all remaining writers.
// Cancellation does not shorten cleanup: callers may release owned paths only
// after this function returns.
func Run(cmd *exec.Cmd) error {
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = time.Second
	}
	if err := Start(cmd); err != nil {
		return err
	}
	err := cmd.Wait()
	_ = Kill(cmd)
	Wait(cmd)
	return err
}
