package watch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLiveLoyalSchemasBuildCombinedWatchSet(t *testing.T) {
	databaseURL := os.Getenv("TEST_NEON_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_NEON_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	set, err := NewLoader(pool, "mainnet").Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Vaults) == 0 {
		t.Fatal("production Loyal schema produced no Earn vault watches")
	}
	if len(set.Channels[EarnSmartAccounts]) == 0 || len(set.Channels[EarnVaultAccounts]) == 0 {
		t.Fatalf("required Earn channels are empty: %#v", set.Channels)
	}
	for channel, addresses := range set.Channels {
		for index := 1; index < len(addresses); index++ {
			if addresses[index-1] >= addresses[index] {
				t.Fatalf("channel %s is not sorted and unique", channel)
			}
		}
	}
}
