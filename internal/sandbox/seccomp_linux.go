//go:build linux

package sandbox

import (
	"fmt"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

const (
	retAllow = 0x7fff0000
	retErrno = 0x00050000
	retKill  = 0x80000000
)

// Syscalls a sandboxed command never needs: kernel and namespace
// administration, tracing other processes, keyrings, eBPF and the like.
var deniedSyscalls = append([]uint32{
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE, unix.SYS_FSOPEN, unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR,
	unix.SYS_SETNS, unix.SYS_UNSHARE,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_REBOOT,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_ACCT, unix.SYS_QUOTACTL, unix.SYS_SYSLOG,
	unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX,
	unix.SYS_FANOTIFY_INIT,
}, archDenied...)

const newNamespaces = unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWPID |
	unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP

// seccompProgram builds the filter: wrong architecture kills, denied syscalls
// fail with EPERM, clone creating namespaces fails with EPERM, clone3 reports
// ENOSYS (libc then falls back to clone, whose flags we can inspect), and
// with denyInet, IPv4 and IPv6 sockets fail with EACCES.
func seccompProgram(denyInet bool) ([]unix.SockFilter, error) {
	if auditArch == 0 {
		return nil, fmt.Errorf("no seccomp filter for this architecture")
	}
	loadNr := bpf.LoadAbsolute{Off: 0, Size: 4}
	loadArg0 := bpf.LoadAbsolute{Off: 16, Size: 4} // low half, little-endian
	prog := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 4, Size: 4},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: auditArch, SkipTrue: 1},
		bpf.RetConstant{Val: retKill},
		loadNr,
	}
	prog = append(prog, archPrologue...)
	for _, nr := range deniedSyscalls {
		prog = append(prog,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: nr, SkipFalse: 1},
			bpf.RetConstant{Val: retErrno | uint32(unix.EPERM)})
	}
	prog = append(prog,
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.SYS_CLONE3, SkipFalse: 1},
		bpf.RetConstant{Val: retErrno | uint32(unix.ENOSYS)},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.SYS_CLONE, SkipFalse: 3},
		loadArg0,
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: newNamespaces, SkipFalse: 1},
		bpf.RetConstant{Val: retErrno | uint32(unix.EPERM)},
		loadNr,
	)
	if denyInet {
		prog = append(prog,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.SYS_SOCKET, SkipFalse: 4},
			loadArg0,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.AF_INET, SkipTrue: 1},
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.AF_INET6, SkipFalse: 1},
			bpf.RetConstant{Val: retErrno | uint32(unix.EACCES)},
			loadNr,
		)
	}
	prog = append(prog, bpf.RetConstant{Val: retAllow})
	raw, err := bpf.Assemble(prog)
	if err != nil {
		return nil, err
	}
	out := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		out[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	return out, nil
}
