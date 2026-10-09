//go:build !linux

package sandbox

import "syscall"

// firecrackerProcAttr: Firecracker runs only on Linux; elsewhere the rung's
// probe fails before a VM starts, so this only has to compile.
func firecrackerProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
