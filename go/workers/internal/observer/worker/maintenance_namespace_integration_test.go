package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/solana-foundation/solana-go/v2"
)

// Production holds 34 active route_policies with cluster='unknown' and 23
// active positions without a mainnet managed vault. The Apps recorders these
// passes replace publish over every active vault with no cluster test, so
// such rows must not fail every maintenance pass.
func TestMaintenanceNamespaceAdmitsLegacyUnknownCustody(t *testing.T) {
	pool := observerFixturePool(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings, vault, policy, wallet := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	var policyID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,last_seen_slot,last_seen_signature,active,cluster)
        VALUES($1,$2,1,$3,1,$4,1,170,'legacy',true,'unknown') RETURNING id`, settings, wallet, policy, vault).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.user_yield_positions WHERE settings=$1`, settings)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.route_policies WHERE id=$1`, policyID)
	})
	usdc := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,
        initial_reserve,initial_liquidity_mint,deposit_mint,principal_amount_raw,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at,
        current_reserve,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at)
        VALUES($1,$2,$2,1,$3,$4,$5,1,'reserve',$6,$6,1000000,'legacy-deposit','legacy-deposit',170,'active',now(),now(),'reserve',$6,1000000,170,now())`,
		wallet, settings, vault, policyID, policy, usdc); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`)
	}))
	defer server.Close()
	runtime := &Runtime{cfg: config.Config{Cluster: "mainnet-beta"}, rpc: chainClient(t, server.URL, time.Second), neon: pool}
	if err := runtime.validateMaintenanceNamespace(ctx); err != nil {
		t.Fatalf("legacy custody blocked the maintenance pass: %v", err)
	}
}
