package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

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

func TestEarnRelayKeepsTheRPCURLsWhenTheAPIKeyIsSet(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{
		"WEB_EVM_RPC_URLS": "base=https://rpc.example/base",
		"RELAY_API_KEY":    " k \n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("LOYAL_WEB_SOLANA_CLUSTER", "mainnet")
	rpcClient, err := chain.New("http://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := earn(t.Context(), prometheus.NewRegistry(), rpcClient, nil, nil, nil).Relay
	if relay.APIKey != "k" || relay.EVMRPC["base"] != "https://rpc.example/base" {
		t.Fatalf("Relay APIKey=%q EVMRPC=%v, want the key and the base RPC", relay.APIKey, relay.EVMRPC)
	}
}
