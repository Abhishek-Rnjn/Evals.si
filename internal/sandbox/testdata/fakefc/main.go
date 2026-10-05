// Command fakefc stands in for firecracker in tests on hosts without KVM. It
// serves the parts of the API the driver uses; "booting" unpacks the root
// disk with debugfs and runs evalsi-guest's agent on the host over that
// directory (no isolation at all), behind a vsock-style Unix socket with the
// CONNECT handshake. Snapshots tar the directory. Every API call is logged
// to calls.log in the working directory.
package main

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	vsock   string
	drive   string
	agent   *exec.Cmd
	logFile *os.File
)

func main() {
	api := ""
	for i, a := range os.Args {
		if a == "--api-sock" && i+1 < len(os.Args) {
			api = os.Args[i+1]
		}
	}
	logFile, _ = os.Create("calls.log")
	ln, err := net.Listen("unix", api)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = http.Serve(ln, http.HandlerFunc(handle))
}

func fail(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": err.Error()})
}

func handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	raw, _ := json.Marshal(body)
	mu.Lock()
	fmt.Fprintf(logFile, "%s %s %s\n", r.Method, r.URL.Path, raw)
	mu.Unlock()
	var err error
	switch r.URL.Path {
	case "/drives/rootfs":
		drive, _ = body["path_on_host"].(string)
	case "/vsock":
		vsock, _ = body["uds_path"].(string)
	case "/actions":
		if body["action_type"] == "InstanceStart" {
			err = boot()
		}
	case "/snapshot/create":
		err = tarDir("fsroot", body["mem_file_path"].(string))
		if err == nil {
			err = os.WriteFile(body["snapshot_path"].(string), []byte(`{"fake":true}`), 0o600)
		}
	case "/snapshot/load":
		vsock = "v.sock"
		backend := body["mem_backend"].(map[string]any)
		if err = untar(backend["backend_path"].(string), "fsroot"); err == nil {
			err = startAgent()
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func boot() error {
	if err := os.MkdirAll("fsroot", 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("debugfs", "-R", "rdump / fsroot", drive).CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs: %v: %s", err, out)
	}
	if _, err := os.Stat("fsroot/sbin/evalsi-guest"); err != nil {
		return fmt.Errorf("the root disk has no /sbin/evalsi-guest: %w", err)
	}
	return startAgent()
}

func startAgent() error {
	cwd, _ := os.Getwd()
	// Relative socket paths: a jailer chroot can be longer than sun_path allows.
	guestSock := "guest.sock"
	agent = exec.Command(os.Getenv("FAKEFC_GUEST"), "agent", "--listen", "unix://"+guestSock,
		"--root", filepath.Join(cwd, "fsroot"), "--host-socket", vsock)
	agent.Stdout, agent.Stderr = os.Stderr, os.Stderr
	if err := agent.Start(); err != nil {
		return err
	}
	for range 200 {
		if _, err := os.Stat(guestSock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ln, err := net.Listen("unix", vsock)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "CONNECT 1024" {
					fmt.Fprintf(c, "ERR %q\n", line)
					return
				}
				up, err := net.Dial("unix", guestSock)
				if err != nil {
					return
				}
				defer up.Close()
				fmt.Fprint(c, "OK 1073741824\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return nil
}

func tarDir(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()
	return filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil || !(fi.IsDir() || fi.Mode().IsRegular()) {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			_, err = io.Copy(tw, in)
			return err
		}
		return nil
	})
}

func untar(in, dir string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			_ = os.MkdirAll(p, os.FileMode(hdr.Mode))
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return err
		}
	}
}
