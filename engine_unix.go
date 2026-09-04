//go:build unix

package gateway

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr places the engine in its own process group so a
// shutdown kills the whole group: engines that fork helpers (tablebase
// probers, GPU wrappers) cannot leave orphaned children behind.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess terminates the engine process group. A negative pid targets
// the group created with Setpgid above.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
