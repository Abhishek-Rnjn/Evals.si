//go:build !linux

package sandbox

import (
	"errors"
	"io"
)

type netnsHolder struct{}

func NetNS(stdin io.Reader, stdout, stderr io.Writer) int { return exitLauncherFailure }

func Probe(args []string, stderr io.Writer) int { return exitLauncherFailure }

func startNetNS(launcher string) (*netnsHolder, error) {
	return nil, errors.New("private networks need Linux")
}

func (h *netnsHolder) close() {}

func (h *netnsHolder) nsenterArgs(nsenter string) []string { return nil }
