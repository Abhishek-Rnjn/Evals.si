package sandboxsvc

import (
	"os"
	"testing"
)

func TestProbeFilterDropsOnlyProbeHangups(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	_, _ = probeFilter{}.Write([]byte("2026/10/09 http: TLS handshake error from 10.0.0.1:5000: EOF\n"))
	_, _ = probeFilter{}.Write([]byte("2026/10/09 http: TLS handshake error from 10.0.0.1:5000: remote error: tls: bad certificate\n"))
	os.Stderr = old
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])
	if got != "2026/10/09 http: TLS handshake error from 10.0.0.1:5000: remote error: tls: bad certificate\n" {
		t.Errorf("logged %q", got)
	}
}
