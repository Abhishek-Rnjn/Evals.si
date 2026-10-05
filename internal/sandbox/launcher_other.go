//go:build !linux

package sandbox

import (
	"fmt"
	"io"
)

// Launch is `evalsid sandbox-exec`; sandboxing needs Linux.
func Launch(stderr io.Writer) int {
	fmt.Fprintln(stderr, launcherPrefix+"sandboxing needs Linux")
	return exitLauncherFailure
}
