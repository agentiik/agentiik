//go:build !linux

package e2e

import "os/exec"

// tied does nothing where the kernel has no parent death signal: Stand refuses to stand an
// installation up anywhere but Linux.
func tied(*exec.Cmd) {}
