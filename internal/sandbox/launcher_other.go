//go:build !linux

package sandbox

import (
	"fmt"
	"io"
)

// Exec is `evalsid sandbox-exec`; sandboxing needs Linux.
func Exec(stderr io.Writer) int {
	fmt.Fprintln(stderr, launcherPrefix+"sandboxing needs Linux")
	return exitLauncherFailure
}
