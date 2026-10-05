package sandbox

import (
	"os"
	"path/filepath"
)

// System paths a sandboxed command may read. Everything else on the host
// (home directories, /root, /var, /tmp, /srv, other /etc files) stays out.
var (
	systemDirs  = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc/alternatives", "/etc/ssl", "/etc/ca-certificates", "/etc/ld.so.conf.d"}
	systemFiles = []string{"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/localtime", "/etc/nsswitch.conf", "/etc/passwd", "/etc/group", "/etc/hosts", "/etc/resolv.conf", "/etc/mime.types"}
)

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// isSymlink reports whether p is a symlink and where it points (merged-/usr
// distributions make /bin, /lib and friends symlinks into /usr).
func isSymlink(p string) (string, bool) {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(p)
	return target, err == nil
}

func absPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if a, err := filepath.Abs(p); err == nil {
			out = append(out, a)
		}
	}
	return out
}
