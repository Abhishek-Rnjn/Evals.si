package store

import (
	"context"
	"testing"
	"time"
)

func TestLeases(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ok, err := s.AcquireLease(ctx, "run:r1", "a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("free lease: %v %v", ok, err)
	}
	if ok, _ := s.AcquireLease(ctx, "run:r1", "b", time.Minute); ok {
		t.Fatal("b took a's live lease")
	}
	if ok, _ := s.AcquireLease(ctx, "run:r1", "a", time.Minute); !ok {
		t.Fatal("a could not renew")
	}
	if who, _ := s.LeaseHolder(ctx, "run:r1"); who != "a" {
		t.Fatalf("holder %q", who)
	}
	// An expired lease goes to whoever asks.
	if ok, _ := s.AcquireLease(ctx, "run:r2", "a", -time.Second); !ok {
		t.Fatal("r2")
	}
	if who, _ := s.LeaseHolder(ctx, "run:r2"); who != "" {
		t.Fatalf("expired lease held by %q", who)
	}
	if ok, _ := s.AcquireLease(ctx, "run:r2", "b", time.Minute); !ok {
		t.Fatal("b could not take an expired lease")
	}
	if err := s.ReleaseLease(ctx, "run:r1", "b"); err != nil {
		t.Fatal(err)
	}
	if who, _ := s.LeaseHolder(ctx, "run:r1"); who != "a" {
		t.Fatal("b released a's lease")
	}
	if err := s.ReleaseLease(ctx, "run:r1", "a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AcquireLease(ctx, "run:r1", "b", time.Minute); !ok {
		t.Fatal("released lease not free")
	}
}
