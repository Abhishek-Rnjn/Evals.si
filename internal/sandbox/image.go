package sandbox

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// imageRoot is an unpacked image: a root filesystem plus the parts of its
// config a sandbox uses.
type imageRoot struct {
	Ref    string   `json:"ref"`
	Digest string   `json:"digest"`
	Root   string   `json:"-"`
	Env    []string `json:"env,omitempty"`
	// The image's working directory and user, informational.
	WorkingDir string `json:"working_dir,omitempty"`
	User       string `json:"user,omitempty"`
}

// imageStore unpacks OCI images into root filesystems, cached by digest:
//
//	<dir>/<digest>/root/        the flattened filesystem
//	<dir>/<digest>/image.json   imageRoot
//
// Image references:
//
//	python:3.12-slim, ghcr.io/org/img@sha256:...   a registry (credentials from ~/.docker/config.json)
//	oci-layout:/path/to/layout[@<tag or digest>]   an OCI image layout directory
//	docker-archive:/path/to/image.tar              `docker save` output
//	dir:/path/to/root                              an already unpacked root, used in place
//
// Files are unpacked owned by the evalsid user, without setuid bits and with
// owner write permission: inside the user namespace that user is the
// sandbox user, so a private copy of the root is fully writable by it, as
// under fakeroot. Device nodes are skipped (the sandbox gets its own /dev).
type imageStore struct {
	dir string
	// With sandbox.image_signatures, images must be signed before use.
	verifier *signatureVerifier

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newImageStore(dir string) *imageStore {
	return &imageStore{dir: dir, locks: map[string]*sync.Mutex{}}
}

func (st *imageStore) lock(key string) func() {
	st.mu.Lock()
	l, ok := st.locks[key]
	if !ok {
		l = &sync.Mutex{}
		st.locks[key] = l
	}
	st.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (st *imageStore) get(ctx context.Context, ref string) (*imageRoot, error) {
	if st.verifier != nil && isLocalImage(ref) {
		if err := st.verifier.verify(ctx, ref, ""); err != nil {
			return nil, err
		}
	}
	if p, ok := strings.CutPrefix(ref, "dir:"); ok {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("image %s: not a directory", ref)
		}
		return &imageRoot{Ref: ref, Digest: "dir:" + abs, Root: abs, Env: []string{"PATH=" + sandboxPath}}, nil
	}
	img, err := resolveImage(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if st.verifier != nil && !isLocalImage(ref) {
		if err := st.verifier.verify(ctx, ref, digest.String()); err != nil {
			return nil, err
		}
	}
	key := strings.ReplaceAll(digest.String(), ":", "-")
	defer st.lock(key)()
	dir := filepath.Join(st.dir, key)
	if info, err := readImageInfo(dir); err == nil {
		info.Root = filepath.Join(dir, "root")
		return info, nil
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image %s: config: %w", ref, err)
	}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(st.dir, ".unpack-")
	if err != nil {
		return nil, err
	}
	defer removeAll(tmp)
	rc := mutate.Extract(img)
	err = extractTar(ctx, rc, filepath.Join(tmp, "root"))
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("image %s: unpacking: %w", ref, err)
	}
	info := &imageRoot{Ref: ref, Digest: digest.String(), Env: cfg.Config.Env, WorkingDir: cfg.Config.WorkingDir, User: cfg.Config.User}
	raw, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "image.json"), raw, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	info.Root = filepath.Join(dir, "root")
	return info, nil
}

func readImageInfo(dir string) (*imageRoot, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return nil, err
	}
	var info imageRoot
	return &info, json.Unmarshal(raw, &info)
}

func resolveImage(ctx context.Context, ref string) (v1.Image, error) {
	switch {
	case strings.HasPrefix(ref, "oci-layout:"):
		p := strings.TrimPrefix(ref, "oci-layout:")
		want := ""
		if i := strings.LastIndex(p, "@"); i > 0 {
			p, want = p[:i], p[i+1:]
		}
		idx, err := layout.ImageIndexFromPath(p)
		if err != nil {
			return nil, err
		}
		m, err := idx.IndexManifest()
		if err != nil {
			return nil, err
		}
		for _, d := range m.Manifests {
			if want == "" || d.Digest.String() == want || d.Annotations["org.opencontainers.image.ref.name"] == want {
				return idx.Image(d.Digest)
			}
		}
		return nil, fmt.Errorf("no image %q in the layout", want)
	case strings.HasPrefix(ref, "docker-archive:"):
		return tarball.ImageFromPath(strings.TrimPrefix(ref, "docker-archive:"), nil)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	return remote.Image(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
}

// extractTar unpacks a flattened image filesystem into dst. os.Root keeps
// every write inside dst, whatever the archive's paths and symlinks say.
func extractTar(ctx context.Context, r io.Reader, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	type dirMode struct {
		name string
		mode os.FileMode
	}
	var dirs []dirMode
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if name == "" {
			continue
		}
		mode := os.FileMode(hdr.Mode).Perm() | 0o200
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
			dirs = append(dirs, dirMode{name, mode | 0o700})
		case tar.TypeReg:
			_ = root.Remove(name)
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			_ = root.Remove(name)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
		case tar.TypeLink:
			target := strings.TrimPrefix(path.Clean("/"+hdr.Linkname), "/")
			_ = root.Remove(name)
			if err := root.Link(target, name); err != nil {
				return fmt.Errorf("%s: %w", hdr.Name, err)
			}
		default:
			// Devices, FIFOs and sockets: the sandbox has its own /dev.
		}
	}
	// Directory modes last, so read-only directories do not block their contents.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Chmod(dirs[i].name, dirs[i].mode)
	}
	return nil
}
