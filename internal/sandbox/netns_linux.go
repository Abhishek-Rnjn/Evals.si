//go:build linux

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// netnsHolder is a process kept alive in new user and network namespaces, with
// only a loopback interface. The commands of a sandbox with services, and the
// services themselves, join its network namespace (through nsenter) before they
// start their own bubblewrap sandbox, so they share one private network on
// which the services listen on loopback. Nothing in it can reach the host's
// network or any other sandbox's.
//
// The holder reads its standard input and exits when that closes, which the
// parent's death does too, so a crashed pool cannot leave namespaces behind.
type netnsHolder struct {
	cmd   *exec.Cmd
	pid   int
	stdin io.WriteCloser
}

// NetNS is `evalsid sandbox-netns`: it brings loopback up, says "ready" and waits
// for its standard input to close. It must run in the namespaces startNetNS makes.
func NetNS(stdin io.Reader, stdout, stderr io.Writer) int {
	if err := loopbackUp(); err != nil {
		fmt.Fprintf(stderr, launcherPrefix+"loopback: %v\n", err)
		return exitLauncherFailure
	}
	fmt.Fprintln(stdout, "ready")
	_, _ = io.Copy(io.Discard, stdin)
	return 0
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// Probe is `evalsid sandbox-probe host:port`: exit 0 when a TCP connection is
// accepted. Run through nsenter, it is how the pool checks that a service is
// listening in the private network.
func Probe(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: evalsid sandbox-probe host:port")
		return 2
	}
	conn, err := net.DialTimeout("tcp", args[0], 3*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = conn.Close()
	return 0
}

func startNetNS(launcher string) (*netnsHolder, error) {
	cmd := exec.Command(launcher, "sandbox-netns")
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		Setpgid:                    true,
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr capped
	stderr.limit, stderr.keep = 4096, true
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the private network: %w", err)
	}
	h := &netnsHolder{cmd: cmd, pid: cmd.Process.Pid, stdin: in}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil || line != "ready\n" {
			ready <- fmt.Errorf("the private network did not start: %s", firstLine(stderr.String()))
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			h.close()
			return nil, err
		}
	case <-time.After(15 * time.Second):
		h.close()
		return nil, errors.New("the private network did not start in 15s")
	}
	return h, nil
}

func (h *netnsHolder) close() {
	_ = h.stdin.Close()
	_ = syscall.Kill(-h.pid, syscall.SIGKILL)
	_ = h.cmd.Wait()
}

// nsenterArgs is the command prefix that runs what follows inside the holder's
// user and network namespaces.
func (h *netnsHolder) nsenterArgs(nsenter string) []string {
	return []string{nsenter, "--user", "--net", "--target", strconv.Itoa(h.pid), "--"}
}
