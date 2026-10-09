// Command evalsi-guest is init inside each Firecracker microVM of the
// Evals.si sandbox (design §13). As PID 1 it mounts the basic filesystems,
// starts itself as the agent (PID 2) and reaps orphans; the agent serves
// GuestAgentService on vsock port 1024. It must be built statically
// (CGO_ENABLED=0): it runs in any image's root filesystem.
//
//	evalsi-guest                  init (PID 1)
//	evalsi-guest agent            the agent, on vsock
//	evalsi-guest agent --listen unix:///path --root DIR   the agent on the host, for tests
//	evalsi-guest install DIR      copy itself to DIR (a sandbox pod's init container)
//	evalsi-guest seed SRC DST     copy SRC's tree into DST (a sandbox pod's init container)
//	evalsi-guest dial HOST:PORT   exit 0 if a TCP connection opens, 3 if it is refused or times out (the pod rung's NetworkPolicy canary)
//	evalsi-guest limit ...        internal: apply resource limits, then exec
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/guest"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "agent":
			os.Exit(agent(os.Args[2:]))
		case "install":
			os.Exit(install(os.Args[2:]))
		case "seed":
			os.Exit(seed(os.Args[2:]))
		case "limit":
			os.Exit(limit(os.Args[2:]))
		case "dial":
			os.Exit(dial(os.Args[2:]))
		case "version":
			fmt.Println(guest.Version)
			return
		}
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "evalsi-guest: run as init inside a microVM, or `evalsi-guest agent`")
		os.Exit(2)
	}
	initMain()
}

// initMain is PID 1: mount, start the agent, reap.
func initMain() {
	for _, m := range []struct {
		src, dst, fstype string
		flags            uintptr
		data             string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, ""},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
	} {
		_ = os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.fstype, m.flags, m.data); err != nil && !errors.Is(err, unix.EBUSY) {
			fmt.Fprintf(os.Stderr, "evalsi-guest: mount %s: %v\n", m.dst, err)
		}
	}
	_ = unix.Sethostname([]byte("sandbox"))
	loopbackUp()
	cmd := exec.Command("/proc/self/exe", "agent")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: starting the agent: %v\n", err)
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, unix.SIGCHLD)
	for range sigs {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid == cmd.Process.Pid {
				// The agent died: the VM is useless; power off so the host notices.
				_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
			}
		}
	}
}

// loopbackUp sets lo up (IFF_UP) with an ioctl, no netlink library needed.
func loopbackUp() {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	_ = unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

type system struct{}

// AddEntropy mixes bytes into the kernel pool and credits them (RNDADDENTROPY).
func (system) AddEntropy(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 8+len(b))
	*(*int32)(unsafe.Pointer(&buf[0])) = int32(len(b) * 8)
	*(*int32)(unsafe.Pointer(&buf[4])) = int32(len(b))
	copy(buf[8:], b)
	const rndAddEntropy = 0x40085203
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		_, err := f.Write(b) // mixed in, uncredited
		return err
	}
	return nil
}

func (system) SetTime(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Settimeofday(&tv)
}

func agent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	listen := fs.String("listen", "", "unix:///path (tests) or tcp://:PORT (a sandbox pod) instead of vsock")
	tokenEnv := fs.String("token-env", "", "a sandbox pod: environment variable holding the bearer token callers must present")
	egress := fs.String("egress", "", "a sandbox pod: host:port of the egress proxy, for StartEgress")
	root := fs.String("root", "", "tests: a directory standing in for the guest's /")
	hostSocket := fs.String("host-socket", "", "tests: unix socket prefix standing in for the host's vsock ports")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, _ := os.Executable()
	// In a pod the container's cgroup limits memory; an address-space limit
	// on top only breaks programs that reserve more than they use (Go, JVMs).
	pod := strings.HasPrefix(*listen, "tcp://")
	a := &guest.Agent{
		Root: *root,
		Limiter: func(l *guestv1alpha1.Limits, argv []string) []string {
			mem := l.GetMemoryMb()
			if pod {
				mem = 0
			}
			return append([]string{self, "limit",
				strconv.Itoa(int(mem)), strconv.Itoa(int(l.GetMaxProcs())),
				strconv.Itoa(int(l.GetMaxFileMb())), strconv.Itoa(int(l.GetCpuSeconds())), "--"}, argv...)
		},
	}
	var ln net.Listener
	var err error
	if addr, ok := strings.CutPrefix(*listen, "tcp://"); ok {
		// A sandbox pod: on the pod network, behind a token, with the
		// container's environment as the base for commands.
		if *tokenEnv == "" || os.Getenv(*tokenEnv) == "" {
			fmt.Fprintln(os.Stderr, "evalsi-guest: a tcp listener needs --token-env naming a set variable")
			return 2
		}
		a.Token = os.Getenv(*tokenEnv)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, *tokenEnv+"=") {
				a.BaseEnv = append(a.BaseEnv, kv)
			}
		}
		_ = os.Unsetenv(*tokenEnv)
		upstream := *egress
		a.DialHost = func(uint32) (net.Conn, error) {
			if upstream == "" {
				return nil, fmt.Errorf("no egress proxy")
			}
			return net.DialTimeout("tcp", upstream, 10*time.Second)
		}
		ln, err = net.Listen("tcp", addr)
	} else if socket, ok := strings.CutPrefix(*listen, "unix://"); ok {
		_ = os.Remove(socket)
		ln, err = net.Listen("unix", socket)
		a.DialHost = func(port uint32) (net.Conn, error) {
			return net.Dial("unix", fmt.Sprintf("%s_%d", *hostSocket, port))
		}
	} else {
		ln, err = vsock.Listen(guest.VsockPort, nil)
		a.System = system{}
		a.DialHost = func(port uint32) (net.Conn, error) { return vsock.Dial(vsock.Host, port, nil) }
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: listen: %v\n", err)
		return 1
	}
	if err := a.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: %v\n", err)
		return 1
	}
	return 0
}

// limit is `evalsi-guest limit MEM_MB PROCS FILE_MB CPU_S -- argv...`: it
// sets rlimits on itself and execs the command.
func limit(args []string) int {
	if len(args) < 6 || args[4] != "--" {
		fmt.Fprintln(os.Stderr, "evalsi-guest: limit MEM_MB PROCS FILE_MB CPU_S -- command...")
		return 125
	}
	n := func(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
	argv := args[5:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: command not found: %s\n", argv[0])
		return 127
	}
	// Everything execve needs is built first: once the address space is
	// limited, this Go process may not be able to allocate again.
	pathp, err1 := syscall.BytePtrFromString(path)
	argvp, err2 := syscall.SlicePtrFromStrings(argv)
	envp, err3 := syscall.SlicePtrFromStrings(os.Environ())
	if err := errors.Join(err1, err2, err3); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %s: %v\n", argv[0], err)
		return 126
	}
	set := func(res int, v uint64) {
		if v > 0 {
			_ = unix.Setrlimit(res, &unix.Rlimit{Cur: v, Max: v})
		}
	}
	set(unix.RLIMIT_NPROC, n(args[1]))
	set(unix.RLIMIT_FSIZE, n(args[2])<<20)
	set(unix.RLIMIT_CPU, n(args[3]))
	set(unix.RLIMIT_AS, n(args[0])<<20)
	_, _, errno := unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(pathp)),
		uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0])))
	fmt.Fprintf(os.Stderr, "sandbox: %s: %v\n", argv[0], errno)
	return 126
}

// install is `evalsi-guest install DIR`: it copies itself to DIR, which is how
// a sandbox pod's init container hands the agent to the task's image.
func install(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "evalsi-guest: install DIR")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: %v\n", err)
		return 1
	}
	data, err := os.ReadFile(self)
	if err == nil {
		err = os.WriteFile(filepath.Join(args[0], "evalsi-guest"), data, 0o755)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: install: %v\n", err)
		return 1
	}
	return 0
}

// dial is `evalsi-guest dial HOST:PORT`: the sandbox pod's NetworkPolicy
// canary. It exits 0 when the connection opens and 3 when it does not.
func dial(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "evalsi-guest: dial HOST:PORT")
		return 2
	}
	c, err := net.DialTimeout("tcp", args[0], 5*time.Second)
	if err != nil {
		return 3
	}
	_ = c.Close()
	return 0
}

// seed is `evalsi-guest seed SRC DST`: it copies the tree at SRC (when it
// exists) into DST. A sandbox pod that runs as a non-root user gets its
// workdir as an empty volume it can write; an init container from the
// task's image seeds it with what the image has there, now owned by the
// pod's user.
func seed(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "evalsi-guest: seed SRC DST")
		return 2
	}
	if err := copyTree(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-guest: seed: %v\n", err)
		return 1
	}
	return 0
}

func copyTree(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		// The copy belongs to the pod's user, who must be able to change it.
		mode := info.Mode().Perm() | 0o200
		switch {
		case d.IsDir() && rel == ".":
			// DST itself: the volume's root, which may belong to someone else.
			return os.MkdirAll(target, 0o700)
		case d.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, mode|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target, mode)
		default:
			return nil // devices, sockets and pipes are not workdir content
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
