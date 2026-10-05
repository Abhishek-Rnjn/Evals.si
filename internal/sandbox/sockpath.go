package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
)

// maxSocketPath is the usable length of a Unix socket path (sun_path).
const maxSocketPath = 100

// shortSocket returns a path for a Unix socket that fits sun_path: the path
// itself when short enough, otherwise its directory through an open file
// descriptor (/proc/self/fd/N/name), which is valid for both connecting and
// listening. release closes the descriptor; call it when the socket is no
// longer dialed or served.
func shortSocket(p string) (path string, release func(), err error) {
	if len(p) <= maxSocketPath {
		return p, func() {}, nil
	}
	dir, err := os.Open(filepath.Dir(p))
	if err != nil {
		return "", nil, err
	}
	short := "/proc/self/fd/" + strconv.Itoa(int(dir.Fd())) + "/" + filepath.Base(p)
	return short, func() { dir.Close() }, nil
}
