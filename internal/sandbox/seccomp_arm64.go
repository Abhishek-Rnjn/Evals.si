//go:build linux && arm64

package sandbox

import "golang.org/x/net/bpf"

const auditArch = 0xc00000b7 // AUDIT_ARCH_AARCH64

var (
	archDenied   []uint32
	archPrologue []bpf.Instruction
)
