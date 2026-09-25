package e2e

import (
	"os/exec"
	"syscall"
)

// tied has the kernel kill a program the installation started when the test's process ends,
// however it ends: a test killed by go test's timeout runs no cleanup, and an API and a
// controller left behind keep their ports and their connections.
func tied(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
