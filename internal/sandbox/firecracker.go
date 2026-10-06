package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1/guestv1alpha1connect"
)

// FirecrackerConfig configures the microVM rung (design §13).
//
// Each sandbox is a Firecracker microVM booted from an ext4 root built from
// its OCI image (cached by digest), with evalsi-guest as init. The host
// reaches the guest only over vsock. There is no network device: egress,
// when the policy allows any, goes through the logging egress proxy over
// vsock, so the rung needs no tap devices, nftables or root (except for the
// optional jailer).
type FirecrackerConfig struct {
	// firecracker binary; default: firecracker on PATH.
	Binary string `json:"binary,omitempty"`
	// Optional jailer binary (chroot, cgroups, seccomp, dropped privileges);
	// it needs evalsid to run as root.
	Jailer    string `json:"jailer,omitempty"`
	JailerUID int    `json:"jailer_uid,omitempty"`
	JailerGID int    `json:"jailer_gid,omitempty"`
	// An uncompressed guest kernel (vmlinux) with virtio-blk, vsock and ext4.
	Kernel     string `json:"kernel"`
	KernelArgs string `json:"kernel_args,omitempty"`
	// The static evalsi-guest binary; default: evalsi-guest next to evalsid.
	Guest string `json:"guest,omitempty"`
	// The image for sandboxes whose spec names none (and for the probe), for
	// example docker.io/library/debian:bookworm-slim.
	DefaultImage string `json:"default_image,omitempty"`
	VCPUs        int    `json:"vcpus,omitempty"`
	MemoryMB     int    `json:"memory_mb,omitempty"`
	// Root disk size; default: the image plus 1 GiB.
	DiskMB int `json:"disk_mb,omitempty"`
	// Booted VMs kept ready per image, so creating a sandbox skips the boot.
	WarmPool int `json:"warm_pool,omitempty"`
	// The KVM device; default /dev/kvm.
	KVMDevice string `json:"kvm_device,omitempty"`
}

func (c *FirecrackerConfig) validate() error {
	if c.WarmPool < 0 || c.VCPUs < 0 || c.MemoryMB < 0 || c.DiskMB < 0 {
		return errors.New("sandbox.firecracker: sizes and warm_pool cannot be negative")
	}
	if c.Jailer != "" && (c.JailerUID == 0 || c.JailerGID == 0) {
		return errors.New("sandbox.firecracker: the jailer needs jailer_uid and jailer_gid (non-root)")
	}
	return nil
}

const (
	fcAPISocket   = "api.sock"
	fcVsockSocket = "v.sock"
	fcRootfs      = "rootfs.ext4"
	fcKernel      = "vmlinux"
	fcSnapshot    = "vm.snap"
	fcMemory      = "vm.mem"
	fcGuestCID    = 3
	fcEgressPort  = 1025
	fcGuestPath   = "/sbin/evalsi-guest"
	fcProxyInside = "127.0.0.1:0"
)

type firecrackerDriver struct {
	sb *Sandbox

	mu     sync.Mutex
	pools  map[string]chan *fcVM
	disks  map[string]*sync.Mutex
	closed bool
	// Refills in flight; shutdown waits for them.
	refills sync.WaitGroup
}

// shutdown stops the warm VMs; refills that finish later are discarded.
func (d *firecrackerDriver) shutdown() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.refills.Wait()
	d.mu.Lock()
	pools := d.pools
	d.pools = map[string]chan *fcVM{}
	d.mu.Unlock()
	for _, pool := range pools {
		for {
			select {
			case vm := <-pool:
				_ = vm.close()
				continue
			default:
			}
			break
		}
	}
}

func newFirecracker(sb *Sandbox) driver {
	return &firecrackerDriver{sb: sb, pools: map[string]chan *fcVM{}, disks: map[string]*sync.Mutex{}}
}

func (d *firecrackerDriver) cfg() *FirecrackerConfig {
	if d.sb.cfg.Firecracker == nil {
		return &FirecrackerConfig{}
	}
	return d.sb.cfg.Firecracker
}

func (d *firecrackerDriver) name() string { return "firecracker" }
func (d *firecrackerDriver) level() Level { return LevelVM }

func (d *firecrackerDriver) binary() (string, error) {
	if b := d.cfg().Binary; b != "" {
		return b, nil
	}
	return exec.LookPath("firecracker")
}

func (d *firecrackerDriver) guestBinary() string {
	if g := d.cfg().Guest; g != "" {
		return g
	}
	return filepath.Join(filepath.Dir(d.sb.cfg.Launcher), "evalsi-guest")
}

func (d *firecrackerDriver) available() error {
	c := d.cfg()
	if runtime.GOOS != "linux" {
		return errors.New("firecracker needs Linux")
	}
	kvm := c.KVMDevice
	if kvm == "" {
		kvm = "/dev/kvm"
	}
	f, err := os.OpenFile(kvm, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("no usable KVM (%s): %w", kvm, errors.Unwrap(err))
	}
	f.Close()
	if d.sb.cfg.Firecracker == nil || c.Kernel == "" {
		return errors.New("configure sandbox.firecracker.kernel (a vmlinux) and default_image")
	}
	if c.DefaultImage == "" {
		return errors.New("configure sandbox.firecracker.default_image (the probe boots it)")
	}
	if _, err := d.binary(); err != nil {
		return fmt.Errorf("firecracker not found: %w", err)
	}
	if _, err := os.Stat(c.Kernel); err != nil {
		return fmt.Errorf("guest kernel: %w", err)
	}
	if g := d.guestBinary(); !staticBinary(g) {
		return fmt.Errorf("guest agent %s is missing or not statically linked (build evalsi-guest with CGO_ENABLED=0)", g)
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return errors.New("mkfs.ext4 not found (e2fsprogs builds the VM root disks)")
	}
	if c.Jailer != "" && os.Geteuid() != 0 {
		return errors.New("the jailer needs evalsid to run as root")
	}
	return nil
}

func (d *firecrackerDriver) supports(sp *Spec) error {
	if sp.Image == "" && d.cfg().DefaultImage == "" {
		return errors.New("a microVM needs an image (set sandbox.firecracker.default_image)")
	}
	return nil
}

// --- root disks ---

func fileDigest(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return "missing"
	}
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func dirSizeMB(root string) int {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return int(total >> 20)
}

// baseDisk builds (once, cached by image digest, guest and size) an ext4
// image of the image root with evalsi-guest as /sbin/evalsi-guest.
func (d *firecrackerDriver) baseDisk(ctx context.Context, img *imageRoot) (string, error) {
	guestBin := d.guestBinary()
	key := strings.NewReplacer(":", "-", "/", "_").Replace(img.Digest) + "-" + fileDigest(guestBin)
	size := d.cfg().DiskMB
	if size <= 0 {
		size = dirSizeMB(img.Root) + 1024
	}
	key += "-" + strconv.Itoa(size)
	d.mu.Lock()
	lock, ok := d.disks[key]
	if !ok {
		lock = &sync.Mutex{}
		d.disks[key] = lock
	}
	d.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(d.sb.cfg.CacheDir, "firecracker")
	out := filepath.Join(dir, key+".ext4")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(dir, ".stage-")
	if err != nil {
		return "", err
	}
	defer removeAll(stage)
	root := filepath.Join(stage, "root")
	if err := copyTree(ctx, img.Root, root); err != nil {
		return "", err
	}
	for _, p := range []string{"sbin", "proc", "sys", "dev", "tmp", "run"} {
		_ = os.MkdirAll(filepath.Join(root, p), 0o755)
	}
	target := filepath.Join(root, strings.TrimPrefix(fcGuestPath, "/"))
	_ = os.Remove(target)
	raw, err := os.ReadFile(guestBin)
	if err != nil {
		return "", fmt.Errorf("guest agent: %w", err)
	}
	if err := os.WriteFile(target, raw, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(stage, "disk.ext4")
	cmd := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-L", "evalsi-root", "-d", root, tmp, strconv.Itoa(size)+"M")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.ext4: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return out, nil
}

// --- the Firecracker API and vsock ---

type fcAPI struct {
	client *http.Client
}

func newFCAPI(socket string) *fcAPI {
	return &fcAPI{client: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialUnix(ctx, socket)
		},
	}}}
}

// dialUnix connects to a Unix socket of any path length.
func dialUnix(ctx context.Context, p string) (net.Conn, error) {
	short, release, err := shortSocket(p)
	if err != nil {
		return nil, err
	}
	defer release()
	return (&net.Dialer{}).DialContext(ctx, "unix", short)
}

func (a *fcAPI) call(ctx context.Context, method, p string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+p, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var fault struct {
			Message string `json:"fault_message"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &fault)
		if fault.Message == "" {
			fault.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, p, resp.StatusCode, fault.Message)
	}
	return nil
}

// dialVsock connects to a guest port through Firecracker's vsock Unix socket
// (the "CONNECT <port>" handshake).
func dialVsock(ctx context.Context, uds string, port uint32) (net.Conn, error) {
	c, err := dialUnix(ctx, uds)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %q", strings.TrimSpace(line))
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// --- VMs ---

// fcVM is one microVM, a sandbox backend.
type fcVM struct {
	guestConn
	d      *firecrackerDriver
	dir    string // the VM's root: the jailer's chroot, or the sandbox directory
	cmd    *exec.Cmd
	exited chan struct{}
	api    *fcAPI
	image  *imageRoot
	iso    Isolation
	// A warm VM's own directory, removed on close; empty when the VM lives
	// in its session's directory.
	ownDir string

	closeOnce sync.Once
}

func (d *firecrackerDriver) vmRoot(dir string) (string, string) {
	c := d.cfg()
	if c.Jailer == "" {
		return dir, ""
	}
	id := "evalsi-" + hex.EncodeToString(randomBytes(6))
	bin, _ := d.binary()
	return filepath.Join(dir, "jail", filepath.Base(bin), id, "root"), id
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// linkOrCopy places src at dst, hard-linked when on the same filesystem.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// start launches firecracker (under the jailer when configured) for a VM
// root that already holds its files, and waits for the API socket.
func (d *firecrackerDriver) start(ctx context.Context, vm *fcVM, jailID string) error {
	c := d.cfg()
	bin, err := d.binary()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if c.Jailer != "" {
		base := strings.TrimSuffix(vm.dir, filepath.Join(filepath.Base(bin), jailID, "root"))
		cmd = exec.Command(c.Jailer, "--id", jailID, "--exec-file", bin,
			"--uid", strconv.Itoa(c.JailerUID), "--gid", strconv.Itoa(c.JailerGID),
			"--chroot-base-dir", strings.TrimSuffix(base, "/"), "--", "--api-sock", "/"+fcAPISocket)
		if err := filepath.WalkDir(vm.dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(p, c.JailerUID, c.JailerGID)
		}); err != nil {
			return err
		}
	} else {
		cmd = exec.Command(bin, "--api-sock", fcAPISocket)
		cmd.Dir = vm.dir
	}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting firecracker: %w", err)
	}
	vm.cmd = cmd
	vm.exited = make(chan struct{})
	go func() { _ = cmd.Wait(); close(vm.exited) }()
	vm.api = newFCAPI(filepath.Join(vm.dir, fcAPISocket))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(vm.dir, fcAPISocket)); err == nil {
			break
		}
		select {
		case <-vm.exited:
			return fmt.Errorf("firecracker exited: %s", firstLine(logs.String()))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("firecracker did not open its API socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	uds := filepath.Join(vm.dir, fcVsockSocket)
	vm.guest = guestv1alpha1connect.NewGuestAgentServiceClient(&http.Client{Transport: &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVsock(ctx, uds, 1024)
		},
	}}, "http://guest", connect.WithGRPC())
	return nil
}

// waitGuest waits until the guest agent answers.
func (vm *fcVM) waitGuest(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := vm.guest.Health(hctx, connect.NewRequest(&guestv1alpha1.HealthRequest{}))
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-vm.exited:
			return errors.New("the microVM stopped while booting")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the guest agent did not answer: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// boot starts a fresh VM from the image's base disk.
func (d *firecrackerDriver) boot(ctx context.Context, img *imageRoot, dir string) (*fcVM, error) {
	c := d.cfg()
	disk, err := d.baseDisk(ctx, img)
	if err != nil {
		return nil, err
	}
	root, jailID := d.vmRoot(dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := copyTree(ctx, disk, filepath.Join(root, fcRootfs)); err != nil {
		return nil, err
	}
	if err := linkOrCopy(c.Kernel, filepath.Join(root, fcKernel)); err != nil {
		return nil, fmt.Errorf("guest kernel: %w", err)
	}
	vm := &fcVM{d: d, dir: root, image: img}
	if err := d.start(ctx, vm, jailID); err != nil {
		vm.kill()
		return nil, err
	}
	args := c.KernelArgs
	if args == "" {
		args = "console=ttyS0 reboot=k panic=1 pci=off quiet"
	}
	args += " root=/dev/vda rw init=" + fcGuestPath
	vcpus, mem := max(c.VCPUs, 1), c.MemoryMB
	if mem <= 0 {
		mem = 1024
	}
	steps := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/boot-source", map[string]any{"kernel_image_path": fcKernel, "boot_args": args}},
		{http.MethodPut, "/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": fcRootfs, "is_root_device": true, "is_read_only": false}},
		{http.MethodPut, "/machine-config", map[string]any{"vcpu_count": vcpus, "mem_size_mib": mem}},
		{http.MethodPut, "/vsock", map[string]any{"guest_cid": fcGuestCID, "uds_path": fcVsockSocket}},
		{http.MethodPut, "/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if err := vm.api.call(ctx, s.method, s.path, s.body); err != nil {
			vm.kill()
			return nil, err
		}
	}
	if err := vm.waitGuest(ctx); err != nil {
		vm.kill()
		return nil, err
	}
	return vm, nil
}

// restore starts a VM from a snapshot directory (vm.snap, vm.mem, rootfs.ext4).
func (d *firecrackerDriver) restore(ctx context.Context, from, dir string, img *imageRoot) (*fcVM, error) {
	root, jailID := d.vmRoot(dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	for _, f := range []string{fcRootfs, fcSnapshot, fcMemory} {
		if err := copyTree(ctx, filepath.Join(from, f), filepath.Join(root, f)); err != nil {
			return nil, err
		}
	}
	if err := linkOrCopy(d.cfg().Kernel, filepath.Join(root, fcKernel)); err != nil {
		return nil, err
	}
	vm := &fcVM{d: d, dir: root, image: img}
	if err := d.start(ctx, vm, jailID); err != nil {
		vm.kill()
		return nil, err
	}
	if err := vm.api.call(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": fcSnapshot, "mem_backend": map[string]any{"backend_type": "File", "backend_path": fcMemory}, "resume_vm": true,
	}); err != nil {
		vm.kill()
		return nil, err
	}
	if err := vm.waitGuest(ctx); err != nil {
		vm.kill()
		return nil, err
	}
	// Clones must not share RNG state or the snapshot's clock.
	if _, err := vm.guest.Refresh(ctx, connect.NewRequest(&guestv1alpha1.RefreshRequest{
		Entropy: randomBytes(64), UnixNanos: time.Now().UnixNano(),
	})); err != nil {
		vm.kill()
		return nil, fmt.Errorf("refreshing the restored guest: %w", err)
	}
	return vm, nil
}

func (d *firecrackerDriver) poolKey(img *imageRoot) string { return img.Digest }

// take returns a warm VM for the image, if one is ready, and starts booting
// its replacement.
func (d *firecrackerDriver) take(img *imageRoot) *fcVM {
	n := d.cfg().WarmPool
	if n <= 0 {
		return nil
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	pool, ok := d.pools[d.poolKey(img)]
	if !ok {
		pool = make(chan *fcVM, n)
		d.pools[d.poolKey(img)] = pool
	}
	d.mu.Unlock()
	refill := func() {
		defer d.refills.Done()
		dir, err := os.MkdirTemp(d.sb.cfg.WorkDir, "evalsi-warm-")
		if err != nil {
			return
		}
		vm, err := d.boot(context.Background(), img, dir)
		if err != nil {
			_ = removeAll(dir)
			return
		}
		vm.ownDir = dir
		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()
		if closed {
			_ = vm.close()
			return
		}
		select {
		case pool <- vm:
		default:
			_ = vm.close()
		}
	}
	// Registered under the lock, so shutdown (which sets closed under it) waits for every one.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	select {
	case vm := <-pool:
		d.refills.Add(1)
		go refill()
		return vm
	default:
		for range n - len(pool) {
			d.refills.Add(1)
			go refill()
		}
		return nil
	}
}

func (d *firecrackerDriver) open(ctx context.Context, sp *Spec, dir, from string) (backend, error) {
	ref := sp.Image
	if ref == "" {
		ref = d.cfg().DefaultImage
	}
	img, err := d.sb.images.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	var vm *fcVM
	switch {
	case from != "":
		vm, err = d.restore(ctx, from, dir, img)
	default:
		if vm = d.take(img); vm == nil {
			vm, err = d.boot(ctx, img, dir)
		}
	}
	if err != nil {
		return nil, err
	}
	vm.sp, vm.driver, vm.imageEnv = sp, d.name(), img.Env
	vm.iso = Isolation{Driver: d.name(), Level: d.level().String(), Enforcement: "full", Notes: []string{
		"kernel: " + filepath.Base(d.cfg().Kernel), "root: image " + img.Digest + ", private disk",
	}}
	if d.cfg().Jailer != "" {
		vm.iso.Notes = append(vm.iso.Notes, "jailer: chroot, cgroups, seccomp, unprivileged")
	}
	if sp.ReadOnlyRoot {
		vm.iso.Notes = append(vm.iso.Notes, "read_only_root: the root disk is private to this VM instead")
	}
	switch sp.Network.Mode {
	case NetworkDeny:
		vm.iso.Notes = append(vm.iso.Notes, "network: none (no network device)")
	default:
		note := "network: allowlist through the logging egress proxy over vsock"
		if sp.Network.Mode == NetworkAllow {
			note = "network: any host through the logging egress proxy over vsock (proxy-aware clients only)"
		}
		proxy, err := newEgressProxy("unix", filepath.Join(vm.dir, fmt.Sprintf("%s_%d", fcVsockSocket, fcEgressPort)), sp.Network.Allow)
		if err != nil {
			vm.kill()
			return nil, err
		}
		proxy.anyHost = sp.Network.Mode == NetworkAllow
		vm.proxy = proxy
		resp, err := vm.guest.StartEgress(ctx, connect.NewRequest(&guestv1alpha1.StartEgressRequest{Listen: fcProxyInside, VsockPort: fcEgressPort}))
		if err != nil {
			vm.kill()
			return nil, fmt.Errorf("guest egress forwarder: %w", err)
		}
		vm.proxyAddr = resp.Msg.GetListen()
		vm.iso.Notes = append(vm.iso.Notes, note)
	}
	if from == "" {
		if err := vm.mkdir(ctx, sp.Workdir); err != nil {
			vm.kill()
			return nil, err
		}
		if len(sp.Files) > 0 {
			files := make([]File, 0, len(sp.Files))
			for name, content := range sp.Files {
				files = append(files, File{Path: name, Content: content})
			}
			if err := vm.writeFiles(files); err != nil {
				vm.kill()
				return nil, err
			}
		}
	}
	return vm, nil
}

func (vm *fcVM) isolation() Isolation { return vm.iso }

func (vm *fcVM) imageDigest() string { return vm.image.Digest }

// snapshot pauses the VM, saves its memory and device state plus a copy of
// its disk, and resumes it.
func (vm *fcVM) snapshot(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := vm.api.call(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
		return err
	}
	resume := func() error { return vm.api.call(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Resumed"}) }
	for _, f := range []string{fcSnapshot, fcMemory} {
		_ = os.Remove(filepath.Join(vm.dir, f))
	}
	if err := vm.api.call(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": fcSnapshot, "mem_file_path": fcMemory,
	}); err != nil {
		_ = resume()
		return err
	}
	for _, f := range []string{fcRootfs, fcSnapshot, fcMemory} {
		if err := copyTree(ctx, filepath.Join(vm.dir, f), filepath.Join(dir, f)); err != nil {
			_ = resume()
			return err
		}
	}
	return resume()
}

func (vm *fcVM) kill() {
	vm.closeOnce.Do(func() {
		if vm.proxy != nil {
			vm.proxy.Close()
		}
		if vm.cmd != nil && vm.cmd.Process != nil {
			_ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL)
			<-vm.exited
		}
	})
}

func (vm *fcVM) close() error {
	vm.kill()
	if vm.ownDir != "" {
		return removeAll(vm.ownDir)
	}
	return nil
}
