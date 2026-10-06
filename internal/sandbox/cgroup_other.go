//go:build !linux

package sandbox

import (
	"errors"
	"syscall"
)

type cgroupPool struct{}

type cgroupLease struct{}

func setupCgroups(mode string, _ float64) (*cgroupPool, error) {
	if mode == "" || mode == "auto" || mode == "off" {
		return nil, nil
	}
	return nil, errors.New("sandbox.cgroup needs Linux")
}

func (p *cgroupPool) has(string) bool { return false }
func (p *cgroupPool) note() string    { return "" }
func (p *cgroupPool) lease(*launchSpec) (*cgroupLease, error) {
	return nil, errors.New("cgroups need Linux")
}
func (l *cgroupLease) kill() error                 { return nil }
func (l *cgroupLease) oomKilled() bool             { return false }
func (l *cgroupLease) close()                      {}
func (l *cgroupLease) attach(*syscall.SysProcAttr) {}
