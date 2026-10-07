package engine

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestHoldFamilyAdmitsOneWriterUntilItsSessionEnds(t *testing.T) {
	dsn := os.Getenv("FLEET_EXEC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL fixture")
	}
	if u, err := url.Parse(dsn); err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("refusing a non-loopback database")
	}
	first, cancel := context.WithCancel(context.Background())
	lost, err := HoldFamily(first, dsn, FamilyFleet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HoldFamily(context.Background(), dsn, FamilyFleet); !errors.Is(err, ErrFamilyHeld) {
		t.Fatalf("second fleet writer: %v", err)
	}
	other, err := HoldFamily(first, dsn, FamilyLookup)
	if err != nil {
		t.Fatalf("another family is independent: %v", err)
	}
	cancel()
	for _, done := range []<-chan struct{}{lost, other} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("hold did not end with its context")
		}
	}
	next, stop := context.WithCancel(context.Background())
	defer stop()
	if _, err := HoldFamily(next, dsn, FamilyFleet); err != nil {
		t.Fatalf("released fleet lock not reacquired: %v", err)
	}
}
