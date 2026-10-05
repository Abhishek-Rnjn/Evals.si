// Command evalsi-guest is init inside each Firecracker microVM of the
// Evals.si sandbox (design §13). As PID 1 it mounts the basic filesystems,
// starts itself as the agent (PID 2) and reaps orphans; the agent serves
// GuestAgentService on vsock port 1024. It must be built statically
// (CGO_ENABLED=0): it runs in any image's root filesystem.
//
//	evalsi-guest                  init (PID 1)
//	evalsi-guest agent            the agent, on vsock
//	evalsi-guest agent --listen unix:///path --root DIR   the agent on the host, for tests
//	evalsi-guest limit ...        internal: apply resource limits, then exec
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
		case "limit":
			os.Exit(limit(os.Args[2:]))
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
	listen := fs.String("listen", "", "tests: unix:///path instead of vsock")
	root := fs.String("root", "", "tests: a directory standing in for the guest's /")
	hostSocket := fs.String("host-socket", "", "tests: unix socket prefix standing in for the host's vsock ports")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, _ := os.Executable()
	a := &guest.Agent{
		Root: *root,
		Limiter: func(l *guestv1alpha1.Limits, argv []string) []string {
			return append([]string{self, "limit",
				strconv.Itoa(int(l.GetMemoryMb())), strconv.Itoa(int(l.GetMaxProcs())),
				strconv.Itoa(int(l.GetMaxFileMb())), strconv.Itoa(int(l.GetCpuSeconds())), "--"}, argv...)
		},
	}
	var ln net.Listener
	var err error
	if socket, ok := strings.CutPrefix(*listen, "unix://"); ok {
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
	set := func(res int, v uint64) {
		if v > 0 {
			_ = unix.Setrlimit(res, &unix.Rlimit{Cur: v, Max: v})
		}
	}
	set(unix.RLIMIT_AS, n(args[0])<<20)
	set(unix.RLIMIT_NPROC, n(args[1]))
	set(unix.RLIMIT_FSIZE, n(args[2])<<20)
	set(unix.RLIMIT_CPU, n(args[3]))
	argv := args[5:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: command not found: %s\n", argv[0])
		return 127
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "sandbox: %s: %v\n", argv[0], err)
	return 126
}
