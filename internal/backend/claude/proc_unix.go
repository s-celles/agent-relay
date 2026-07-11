//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// setProcAttrs puts the subprocess in its own process group and makes
// context cancellation kill the whole group, so helpers the CLI spawns
// (node, shells) die with it (REQ-PROC-04).
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
