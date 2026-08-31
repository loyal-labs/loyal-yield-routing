package watch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoaderProductionSchemaCombinationFiltersAppSettings(t *testing.T) {
	databaseURL := os.Getenv("TEST_WATCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_WATCH_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test fixture path")
	}
	fixturePath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "..", "test-fixtures", "earn-watch-production-schema.sql")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}

	targets, err := NewLoader(pool, "mainnet-beta").loadEarnTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("target count = %d, want app, cross-mint, and Earn MAX targets", len(targets))
	}
	var appTargets, crossMintTargets, earnMaxTargets int
	for _, target := range targets {
		if target.Settings != "settings-a" {
			t.Fatalf("loaded non-app settings %q", target.Settings)
		}
		switch {
		case target.EarnMax:
			earnMaxTargets++
		case len(target.PolicyAccounts) > 0:
			crossMintTargets++
		default:
			appTargets++
		}
	}
	if appTargets != 1 || crossMintTargets != 1 || earnMaxTargets != 1 {
		t.Fatalf("targets app=%d cross_mint=%d earn_max=%d", appTargets, crossMintTargets, earnMaxTargets)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO loyal_yield.balance_sweep_targets
		    (id, cluster, settings, wallet, wallet_token_ata, vault_index,
		     vault_pubkey, vault_token_ata, token_mint, policy_account,
		     subscription_authority, recurring_delegation, desired_active, chain_status)
		VALUES
		    (1, 'mainnet-beta', 'settings-a', 'wallet-a', 'mainnet-wallet-ata', 1,
		     'vault-a', 'mainnet-vault-ata', $1, 'autodeposit-policy-a',
		     'subscription-a', 'delegation-a', TRUE, 'active'),
		    (2, 'testnet', 'settings-test', 'wallet-test', 'testnet-wallet-ata', 1,
		     'vault-test', 'testnet-vault-ata', $1, 'autodeposit-policy-test',
		     'subscription-test', 'delegation-test', TRUE, 'active')`, stablecoins[3].Mint.String()); err != nil {
		t.Fatal(err)
	}

	productionLoader := NewLoader(pool, "mainnet-beta")
	clusterTargets, err := productionLoader.loadEarnTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var autodepositTargets int
	for _, target := range clusterTargets {
		if len(target.AutodepositAccounts) > 0 {
			autodepositTargets++
			if target.Settings != "settings-a" {
				t.Fatalf("loaded cross-cluster Autodeposit settings %q", target.Settings)
			}
		}
	}
	if autodepositTargets != 1 {
		t.Fatalf("Autodeposit target count = %d, want 1 production target", autodepositTargets)
	}
	ataTargets, err := productionLoader.loadATATargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ataTargets) != 1 || ataTargets["mainnet-wallet-ata"].Cluster != "mainnet-beta" {
		t.Fatalf("ATA targets were not cluster-filtered: %#v", ataTargets)
	}

	unownedTargets, err := NewLoader(pool, "devnet").loadEarnTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unownedTargets) != 0 {
		t.Fatalf("loaded %d targets for an app environment with no settings", len(unownedTargets))
	}
}
