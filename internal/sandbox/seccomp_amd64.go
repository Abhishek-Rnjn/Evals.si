//go:build linux && amd64

package sandbox

import (
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

const auditArch = 0xc000003e // AUDIT_ARCH_X86_64

var archDenied = []uint32{unix.SYS_IOPL, unix.SYS_IOPERM}

// x32 syscalls share the x86-64 audit arch but set bit 30; refuse them all.
var archPrologue = []bpf.Instruction{
	bpf.JumpIf{Cond: bpf.JumpGreaterOrEqual, Val: 0x40000000, SkipFalse: 1},
	bpf.RetConstant{Val: retErrno | uint32(unix.EPERM)},
}
