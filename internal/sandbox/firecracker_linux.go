//go:build linux

package sandbox

import "syscall"

// firecrackerProcAttr puts firecracker in its own process group, killed
// with evalsid.
func firecrackerProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
