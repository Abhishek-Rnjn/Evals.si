package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRingKeepsTheNewestBytes(t *testing.T) {
	r := &ring{size: 8}
	r.write([]byte("abcdef"))
	r.write([]byte("ghij"))
	data, next, dropped := r.read(0, 100)
	if string(data) != "cdefghij" || next != 10 || dropped != 2 {
		t.Fatalf("read %q next %d dropped %d", data, next, dropped)
	}
	data, next, dropped = r.read(8, 100)
	if string(data) != "ij" || next != 10 || dropped != 0 {
		t.Fatalf("tail %q %d %d", data, next, dropped)
	}
	r.write([]byte("0123456789ABCDEF"))
	data, _, _ = r.read(0, 3)
	if string(data) != "89A" {
		t.Fatalf("oversized write kept %q", data)
	}
}

func readUntil(t *testing.T, ss *Session, id string, want string) string {
	t.Helper()
	var got string
	var off int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := ss.ProcessOutput(context.Background(), id, off, 0, 0, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got += string(out.Stdout)
		off = out.StdoutOffset
		if strings.Contains(got, want) {
			return got
		}
	}
	t.Fatalf("never saw %q in %q", want, got)
	return ""
}

// A server started in one call is still there in the next, shares the
// workspace with later commands, and dies with the sandbox.
func TestBackgroundProcessSurvivesAcrossCalls(t *testing.T) {
	needPython(t)
	eachRung(t, func(t *testing.T, s *Sandbox) {
		ss := open(t, s, &Spec{})
		st, err := ss.StartProcess(&ProcessSpec{Command: sh("echo started; echo hello > served.txt; sleep 300")})
		if err != nil {
			t.Fatal(err)
		}
		readUntil(t, ss, st.ID, "started")
		// Later foreground commands run while it lives and see its files.
		if r := execIn(t, ss, "sh", "-c", "cat served.txt"); r.stdout != "hello\n" {
			t.Fatalf("foreground saw %q (%s)", r.stdout, r.stderr)
		}
		if n := ss.RunningProcesses(); n != 1 {
			t.Fatalf("running %d", n)
		}
		got, err := ss.KillProcess(st.ID)
		if err != nil || got.Running || !got.Killed || got.Result == nil || got.Result.ExitCode != 137 {
			t.Fatalf("kill: %+v %v", got, err)
		}
		if n := ss.RunningProcesses(); n != 0 {
			t.Fatalf("running after kill %d", n)
		}
	})
}

func TestBackgroundProcessStdinAndExit(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		ss := open(t, s, &Spec{})
		st, err := ss.StartProcess(&ProcessSpec{Command: sh("while read line; do echo got:$line; done; echo bye; exit 3"), Stdin: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.WriteProcessStdin(st.ID, []byte("one\n"), false); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ss, st.ID, "got:one")
		if err := ss.WriteProcessStdin(st.ID, []byte("two\n"), true); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ss, st.ID, "bye")
		out := waitEnded(t, ss, st.ID)
		if out.Status.Running || out.Status.Result.ExitCode != 3 || out.Status.Killed {
			t.Fatalf("status %+v", out.Status)
		}
		if err := ss.WriteProcessStdin(st.ID, []byte("late"), false); err == nil {
			t.Fatal("wrote to an ended process")
		}
	})
}

func TestBackgroundProcessLimitsAndCleanup(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		ss := open(t, s, &Spec{Resources: Resources{MaxFileMB: 1}})
		// Resource limits apply as for any command: a 1 MiB file limit stops a bigger write.
		st, err := ss.StartProcess(&ProcessSpec{Command: sh("head -c 3000000 /dev/zero > big.bin; echo rc=$?")})
		if err != nil {
			t.Fatal(err)
		}
		got := readUntil(t, ss, st.ID, "rc=")
		if strings.Contains(got, "rc=0") {
			t.Fatalf("file size limit not applied: %q", got)
		}
		// A timeout ends it.
		st, err = ss.StartProcess(&ProcessSpec{Command: sh("sleep 60"), Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		out := waitEnded(t, ss, st.ID)
		if out.Status.Running || out.Status.Result.Outcome != OutcomeTimeout {
			t.Fatalf("timeout: %+v", out.Status)
		}
		// Close kills what is left.
		live, err := ss.StartProcess(&ProcessSpec{Command: sh("sleep 300")})
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.Close(); err != nil {
			t.Fatal(err)
		}
		if ss.procs.byID[live.ID].status().Running {
			t.Fatal("process outlived its sandbox")
		}
		if _, err := ss.StartProcess(&ProcessSpec{Command: sh("true")}); err == nil {
			t.Fatal("started a process in a closed sandbox")
		}
	})
}

func TestBackgroundProcessCap(t *testing.T) {
	s := rung(t, "landlock")
	ss := open(t, s, &Spec{})
	for range maxProcesses {
		if _, err := ss.StartProcess(&ProcessSpec{Command: sh("sleep 300")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ss.StartProcess(&ProcessSpec{Command: sh("sleep 300")}); err == nil {
		t.Fatal("exceeded the cap")
	}
}

// waitEnded polls with offsets past the output, so each read blocks until the
// process ends.
func waitEnded(t *testing.T, ss *Session, id string) ProcessOutput {
	t.Helper()
	for range 30 {
		out, err := ss.ProcessOutput(context.Background(), id, 1<<40, 1<<40, 0, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Status.Running {
			return out
		}
	}
	t.Fatal("process did not end")
	return ProcessOutput{}
}
