//go:build unix

package stamp

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own, so it outlives the hook.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
