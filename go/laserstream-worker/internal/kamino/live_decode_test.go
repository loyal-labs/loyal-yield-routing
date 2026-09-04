package kamino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/solanarpc"
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
	rpc := solanarpc.New(rpcURL, time.Minute)
	for start := 0; start < len(targets); start += 100 {
		end := min(start+100, len(targets))
		addresses := make([]string, end-start)
		for index := start; index < end; index++ {
			addresses[index-start] = targets[index].Reserve
		}
		response, err := rpc.MultipleAccounts(ctx, addresses, "confirmed", nil)
		if err != nil {
			t.Fatal(err)
		}
		for index, account := range response.Accounts {
			target := targets[start+index]
			if account == nil {
				t.Fatalf("reserve %s was missing", target.Reserve)
			}
			if account.Owner != klendProgram {
				t.Fatalf("reserve %s owner = %s", target.Reserve, account.Owner)
			}
			if _, err := Decode(target, response.Slot, time.Now().UTC(), account.Data, 400); err != nil {
				t.Fatalf("decode reserve %s: %v", target.Reserve, err)
			}
		}
	}
}
