//go:build linux && !amd64 && !arm64

package sandbox

import "golang.org/x/net/bpf"

// No filter for this architecture: seccompProgram fails, so the probes fail
// and the sandbox fails closed.
const auditArch = 0

var (
	archDenied   []uint32
	archPrologue []bpf.Instruction
)
