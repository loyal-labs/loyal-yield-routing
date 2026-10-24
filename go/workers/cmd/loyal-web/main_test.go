package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
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
		t.Fatalf("the grant checks took %s against a database that never answers, want about %s", took.Round(time.Second), grantBound)
	}
	if verdicts != (grantVerdicts{}) {
		t.Fatalf("a check that timed out must degrade its routes: %+v", verdicts)
	}
}

func TestAMalformedYieldDatabaseURLStillServesEarnMaxWithoutAPY(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "WEB_YIELD_DATABASE_URL"), []byte("not a url"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	rpcClient, err := chain.New("http://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	yieldDB := openDB(t.Context(), "Earn MAX routes", "WEB_YIELD_DATABASE_URL")
	if earnMax(rpcClient, grantVerdicts{EarnMax: true}, yieldDB) == nil {
		t.Fatal("a malformed yield database URL only costs APY; Earn MAX must still be served")
	}
}

func TestEarningsReadTheTimescaleURLFromTheCredentialStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TIMESCALEDB_URL"), []byte("postgres://timescale.fixture/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("TIMESCALEDB_URL", "")

	e := earn(t.Context(), nil, nil, nil)

	if e == nil {
		t.Fatal("earn returned nil")
	}
	if e.TimescaleURL != "postgres://timescale.fixture/db" {
		t.Fatalf("TimescaleURL = %q, want the installed credential so earnings rebuilds do not fail missing_timescale_database_url", e.TimescaleURL)
	}
}
