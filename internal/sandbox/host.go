package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// processKind is a process-confinement rung (bwrap, landlock): every Exec is
// a fresh confined process over a sandbox directory kept on the host.
type processKind interface {
	name() string
	level() Level
	available() error
	supports(sp *Spec) error
	// proxyNetwork is where the egress proxy listens: "unix" (a socket bound
	// into the sandbox) or "tcp" (loopback, when the rung shares the network).
	proxyNetwork() string
	isolation(b *hostBackend) Isolation
	launch(b *hostBackend, e *Exec) (*launchSpec, error)
}

type processRung struct {
	sb   *Sandbox
	kind processKind
}

func (p *processRung) name() string            { return p.kind.name() }
func (p *processRung) level() Level            { return p.kind.level() }
func (p *processRung) available() error        { return p.kind.available() }
func (p *processRung) supports(sp *Spec) error { return p.kind.supports(sp) }

// hostBackend is a sandbox kept in a host directory:
//
//	<dir>/root/        private writable copy of the image root (image, not read_only_root)
//	<dir>/workspace/   the workdir when there is no writable root
//	<dir>/tmp/         TMPDIR on rungs that cannot mount a private /tmp
//	<dir>/egress.sock  the egress proxy (network: allowlist)
type hostBackend struct {
	sb   *Sandbox
	kind processKind
	sp   *Spec
	dir  string
	// Exactly one of root (writable copy) and roRoot (shared, read-only) is
	// set when the sandbox has an image.
	root, roRoot string
	image        *imageRoot
	// Host path of the workdir.
	work  string
	tmp   string
	proxy *egressProxy
}

func (p *processRung) open(ctx context.Context, sp *Spec, dir, from string) (backend, error) {
	b := &hostBackend{sb: p.sb, kind: p.kind, sp: sp, dir: dir, tmp: filepath.Join(dir, "tmp")}
	if err := os.Mkdir(b.tmp, 0o777); err != nil {
		return nil, err
	}
	if err := os.Chmod(b.tmp, 0o777); err != nil {
		return nil, err
	}
	if sp.Image != "" {
		img, err := p.sb.images.get(ctx, sp.Image)
		if err != nil {
			return nil, err
		}
		b.image = img
		if sp.ReadOnlyRoot {
			b.roRoot = img.Root
		} else {
			b.root = filepath.Join(dir, "root")
		}
	}
	switch {
	case b.root != "":
		src := b.image.Root
		if from != "" {
			src = filepath.Join(from, "root")
		}
		if err := copyTree(ctx, src, b.root); err != nil {
			return nil, fmt.Errorf("copying the image root: %w", err)
		}
		b.work = filepath.Join(b.root, sp.Workdir)
	default:
		b.work = filepath.Join(dir, "workspace")
		if from != "" {
			if err := copyTree(ctx, filepath.Join(from, "workspace"), b.work); err != nil {
				return nil, fmt.Errorf("restoring the workspace: %w", err)
			}
		}
	}
	if err := os.MkdirAll(b.work, 0o777); err != nil {
		return nil, fmt.Errorf("creating the workdir: %w", err)
	}
	// The sandboxed user may be mapped to another uid; the sandbox directory
	// itself is private (0700) to the evalsid user.
	if err := os.Chmod(b.work, 0o777); err != nil {
		return nil, err
	}
	if from == "" && len(sp.Files) > 0 {
		files := make([]File, 0, len(sp.Files))
		for name, content := range sp.Files {
			files = append(files, File{Path: name, Content: content})
		}
		if err := b.writeFiles(files); err != nil {
			return nil, err
		}
	}
	if sp.Network.Mode == NetworkAllowlist {
		network, addr := p.kind.proxyNetwork(), "127.0.0.1:0"
		if network == "unix" {
			addr = filepath.Join(dir, "egress.sock")
		}
		proxy, err := newEgressProxy(network, addr, sp.Network.Allow)
		if err != nil {
			return nil, err
		}
		b.proxy = proxy
	}
	return b, nil
}

func (b *hostBackend) isolation() Isolation { return b.kind.isolation(b) }

func (b *hostBackend) imageDigest() string {
	if b.image == nil {
		return ""
	}
	return b.image.Digest
}

// env is the command's whole environment: the image's (or fixed basics),
// then the sandbox's, then the call's.
func (b *hostBackend) env(e *Exec, workdir, tmp string) []string {
	vars := map[string]string{"PATH": sandboxPath, "LANG": "C.UTF-8"}
	if b.image != nil {
		for _, kv := range b.image.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
	}
	vars["HOME"], vars["TMPDIR"], vars["EVALSI_WORKDIR"] = workdir, tmp, workdir
	if b.proxy != nil {
		url := "http://" + b.proxyAddrInside()
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
			vars[k] = url
		}
		vars["NO_PROXY"], vars["no_proxy"] = "localhost,127.0.0.1,::1", "localhost,127.0.0.1,::1"
	}
	for k, v := range b.sp.Env {
		vars[k] = v
	}
	for k, v := range e.Env {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// proxyAddrInside is where commands reach the egress proxy.
func (b *hostBackend) proxyAddrInside() string {
	if b.kind.proxyNetwork() == "unix" {
		return forwardAddr
	}
	return b.proxy.Addr()
}

func (b *hostBackend) exec(ctx context.Context, e *Exec, stdout, stderr io.Writer) *ExecResult {
	spec, err := b.kind.launch(b, e)
	if err != nil {
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: err.Error()}
	}
	sniff := &capped{limit: 64 << 10, keep: true}
	res := b.sb.launch(ctx, spec, e.Stdin, e.Timeout, stdout, io.MultiWriter(stderr, sniff))
	classify(b.kind.name(), res, sniff.String())
	return res
}

// openRoot is the directory a file path resolves in: the workdir for
// relative paths, the writable root for absolute ones. os.Root keeps every
// lookup inside it, so a symlink the sandboxed command planted cannot make
// evalsid read or write host files.
func (b *hostBackend) openRoot(name string) (*os.Root, string, error) {
	if path.IsAbs(name) {
		if b.root == "" {
			return nil, "", fmt.Errorf("file %q: absolute paths need a writable image root; use a path relative to the workdir", name)
		}
		r, err := os.OpenRoot(b.root)
		return r, strings.TrimPrefix(path.Clean(name), "/"), err
	}
	rel, err := relPath(name)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(b.work)
	return r, rel, err
}

func (b *hostBackend) writeFiles(files []File) error {
	for _, f := range files {
		if err := b.writeFile(f); err != nil {
			return err
		}
	}
	return nil
}

func (b *hostBackend) writeFile(f File) error {
	root, rel, err := b.openRoot(f.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o777); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
	}
	mode := f.Mode.Perm()
	if mode == 0 {
		mode = 0o666
	}
	// O_NONBLOCK: a FIFO planted at this path fails instead of blocking.
	fh, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return fmt.Errorf("file %q: %w", f.Path, err)
	}
	defer fh.Close()
	if fi, err := fh.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("file %q is not a regular file", f.Path)
	}
	if _, err := fh.Write(f.Content); err != nil {
		return fmt.Errorf("file %q: %w", f.Path, err)
	}
	return fh.Chmod(mode)
}

func (b *hostBackend) readFiles(paths []string, maxBytes int64) ([]File, []string, bool, error) {
	var (
		out     []File
		missing []string
		total   int64
	)
	for _, name := range paths {
		root, rel, err := b.openRoot(name)
		if err != nil {
			return nil, nil, false, err
		}
		done, err := readTree(root, rel, name, maxBytes, &total, &out)
		root.Close()
		if errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		if done {
			return out, missing, true, nil
		}
	}
	return out, missing, false, nil
}

// readTree reads rel (a file, or a directory recursively) into out, naming
// files as the caller did. It reports true when maxBytes ran out. Symlinks
// and special files are skipped: opening a FIFO on the host would block.
func readTree(root *os.Root, rel, name string, maxBytes int64, total *int64, out *[]File) (bool, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return false, err
	}
	if fi.IsDir() {
		full := false
		err := fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			display := path.Join(name, strings.TrimPrefix(strings.TrimPrefix(p, rel), "/"))
			stop, err := readOne(root, p, display, maxBytes, total, out)
			if stop {
				full = true
				return fs.SkipAll
			}
			return err
		})
		return full, err
	}
	if !fi.Mode().IsRegular() {
		return false, nil
	}
	return readOne(root, rel, name, maxBytes, total, out)
}

func readOne(root *os.Root, rel, name string, maxBytes int64, total *int64, out *[]File) (bool, error) {
	fh, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	if *total+fi.Size() > maxBytes {
		return true, nil
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxBytes-*total))
	if err != nil {
		return false, err
	}
	*total += int64(len(data))
	*out = append(*out, File{Path: name, Content: data, Mode: fi.Mode().Perm()})
	return false, nil
}

func (b *hostBackend) snapshot(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if b.root != "" {
		return copyTree(ctx, b.root, filepath.Join(dir, "root"))
	}
	return copyTree(ctx, b.work, filepath.Join(dir, "workspace"))
}

func (b *hostBackend) egress() []EgressEvent {
	if b.proxy == nil {
		return nil
	}
	return b.proxy.Events()
}

func (b *hostBackend) close() error {
	if b.proxy != nil {
		b.proxy.Close()
	}
	return nil
}

// copyTree copies src to dst (which must not exist), preserving modes and
// symlinks and sharing blocks where the filesystem can (reflinks).
func copyTree(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// walkChmod adds owner write and search permission to every directory, so a
// tree the sandbox locked down can still be deleted.
func walkChmod(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}

func newSessionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sbx_" + hex.EncodeToString(b)
}
