package datasets

import (
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// A spooled upload (the keyed HTTP path) is validated in place and takes no
// slot: it works while both upload slots are held, and stores the same file
// Put would.
func TestPutSpooledNeedsNoSlotAndMatchesPut(t *testing.T) {
	body := "{\"id\":\"one\",\"input\":\"question\"}\n"
	service := &Service{Root: t.TempDir()}
	want, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	service.uploads <- struct{}{}
	service.uploads <- struct{}{}
	if _, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader(body)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("Put with both slots held: %v", err)
	}
	spool, err := os.CreateTemp(t.TempDir(), "spool-")
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	_, _ = spool.WriteString(body)
	got, err := service.PutSpooled(t.Context(), uploadConfig("jsonl"), spool)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSha256() != want.GetSha256() || got.GetPath() != want.GetPath() || got.GetRecords() != 1 {
		t.Fatalf("PutSpooled %+v, Put %+v", got, want)
	}
	if _, err := service.PutSpooled(t.Context(), uploadConfig("csv"), spool); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad format: %v", err)
	}
}
