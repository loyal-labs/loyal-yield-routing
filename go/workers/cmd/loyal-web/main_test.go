package main

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGrantChecksFinishWithinOneBoundWhenTheDatabaseNeverAnswers(t *testing.T) {
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	pool, err := pgxpool.New(t.Context(), fmt.Sprintf("postgres://u:p@%s/db?sslmode=disable", blackhole.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	grantBound = 300 * time.Millisecond
	start := time.Now()
	verdicts := checkGrants(t.Context(), pool, pool)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("five grant checks took %s against a database that never answers, want about %s", took.Round(time.Second), grantBound)
	}
	if verdicts != (grantVerdicts{}) {
		t.Fatalf("a check that timed out must degrade its routes: %+v", verdicts)
	}
}
