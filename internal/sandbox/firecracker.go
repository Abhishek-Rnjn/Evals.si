package sandbox

import (
	"context"
	"errors"
)

// FirecrackerConfig configures the microVM rung.
type FirecrackerConfig struct{}

func (c *FirecrackerConfig) validate() error { return nil }

type firecrackerDriver struct{ sb *Sandbox }

func newFirecracker(sb *Sandbox) driver { return &firecrackerDriver{sb: sb} }

func (d *firecrackerDriver) name() string { return "firecracker" }
func (d *firecrackerDriver) level() Level { return LevelVM }
func (d *firecrackerDriver) available() error {
	return errors.New("the firecracker rung is not in this build yet")
}
func (d *firecrackerDriver) supports(sp *Spec) error { return nil }
func (d *firecrackerDriver) open(ctx context.Context, sp *Spec, dir, from string) (backend, error) {
	return nil, errors.New("the firecracker rung is not in this build yet")
}
