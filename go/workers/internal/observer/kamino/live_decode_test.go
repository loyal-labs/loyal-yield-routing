package kamino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestLiveConfirmedKaminoAccountsDecode(t *testing.T) {
	timescaleURL := os.Getenv("TEST_TIMESCALE_DATABASE_URL")
	rpcURL := os.Getenv("TEST_SOLANA_RPC_URL")
	if timescaleURL == "" || rpcURL == "" {
		t.Skip("TEST_TIMESCALE_DATABASE_URL and TEST_SOLANA_RPC_URL are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, timescaleURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	targets, err := NewStore(pool, "kamino").LoadTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) < 10 {
		t.Fatalf("loaded %d Kamino targets, want at least Earn MAX manifest", len(targets))
	}
	if apiBase := os.Getenv("TEST_KAMINO_API_BASE"); apiBase != "" {
		targets, err = NewCatalogClient(apiBase, time.Minute).Enrich(ctx, targets)
		if err != nil {
			t.Fatalf("enrich live Kamino catalog: %v", err)
		}
	}
	client, err := chain.New(rpcURL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	addresses := make([]solana.PublicKey, len(targets))
	for index, target := range targets {
		addresses[index] = solana.MustPublicKeyFromBase58(target.Reserve)
	}
	slot, accounts, err := client.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
	if err != nil {
		t.Fatal(err)
	}
	for index, account := range accounts {
		target := targets[index]
		if account == nil {
			t.Fatalf("reserve %s was missing", target.Reserve)
		}
		if account.Owner != klend.ProgramID {
			t.Fatalf("reserve %s owner = %s", target.Reserve, account.Owner)
		}
		if _, err := Decode(target, slot, time.Now().UTC(), account, 400); err != nil {
			t.Fatalf("decode reserve %s: %v", target.Reserve, err)
		}
	}
}
