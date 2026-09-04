//go:build !unix

package gateway

import "os/exec"

// configureSysProcAttr has no effect where process groups are unavailable;
// shutdown falls back to killing the engine process itself.
func configureSysProcAttr(cmd *exec.Cmd) {}

// killProcess terminates the engine process itself.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
