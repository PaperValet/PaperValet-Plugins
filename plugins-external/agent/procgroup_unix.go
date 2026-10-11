//go:build unix

package main

// procgroup_unix.go — process-group handling for the agent's child processes.

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcGroup puts the child in its own process group so a timed-out command
// is killed together with everything it spawned. Without it, a command like
// `sh -c "sleep 300 & echo ok"` would leave the sleep running after the shell
// is killed, still holding the output pipe until it exits on its own.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Replace the default single-process kill with a group kill: the negative
	// pid targets the whole group created by Setpgid.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// If a grandchild survives the group kill and still holds the pipes, stop
	// waiting for it instead of blocking the run forever.
	cmd.WaitDelay = 3 * time.Second
}
