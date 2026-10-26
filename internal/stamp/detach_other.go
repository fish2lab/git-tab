//go:build !unix

package stamp

import "os/exec"

func detach(cmd *exec.Cmd) {}
