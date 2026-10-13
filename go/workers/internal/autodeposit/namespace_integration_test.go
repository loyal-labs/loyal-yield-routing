package autodeposit

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A target is a mainnet target: the schema refuses any other namespace, so
// no reader has a foreign or unknown one to refuse.
func TestTargetsAreMainnetOnly(t *testing.T) {
	s := integrationStore(t)
	target := seedIntegrationTarget(t, s, "mainnet-only")
	for _, cluster := range []any{"devnet", nil} {
		_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, target.TargetID, cluster)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "23514" && pgErr.Code != "23502") {
			t.Fatalf("cluster %v accepted: %v", cluster, err)
		}
	}
}

// A vault whose route policy is not an active mainnet one is never selected
// or claimed.
func TestForeignRoutePolicyRefusesFreshWork(t *testing.T) {
	for _, cluster := range []string{"devnet", "unknown"} {
		t.Run(cluster, func(t *testing.T) {
			s := integrationStore(t)
			id, reader := seedControlRuntime(t, s)
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET cluster=$2 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults mv JOIN loyal_yield.balance_sweep_targets target ON target.settings=mv.settings AND target.vault_index=mv.vault_index AND target.vault_pubkey=mv.vault_pubkey WHERE target.id=$1)`, id, cluster); err != nil {
				t.Fatal(err)
			}
			if reader.calls != 0 {
				t.Fatal("unscoped control proof reached mainnet reader")
			}
			fresh, err := s.loadFreshTargets(t.Context(), 100, nil)
			if err != nil || len(fresh) != 0 {
				t.Fatalf("foreign fresh selection=%v err=%v", fresh, err)
			}
			var slot int64
			if err := s.pool.QueryRow(t.Context(), `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, id).Scan(&slot); err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimEligibleLotsOnce(t.Context(), id, "namespace-new", slot, 9_000_000, 4_000_000, nil, nil)
			if err != nil || claim.Status != ClaimNoopStatus() {
				t.Fatalf("unscoped claim=%+v err=%v", claim, err)
			}
		})
	}
}

func TestControlConfirmedSameSlotConflictDoesNotOverwriteWallet(t *testing.T) {
	s := integrationStore(t)
	id, reader := seedControlRuntime(t, s)
	target, err := s.LoadControlTarget(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	o, err := reader.ObserveControl(t.Context(), *target, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=$2,amount_raw=8000000,source_commitment='confirmed' WHERE target_id=$1`, id, o.ObservedSlot); err != nil {
		t.Fatal(err)
	}
	enqueueReconciliationRequest(t, s, id, o.ObservedSlot)
	request, err := s.ClaimAutodepositReconciliationRequest(t.Context(), "same-bank-reader", 30)
	if err != nil || request == nil {
		t.Fatalf("claim=%v %v", request, err)
	}
	if err = s.ApplyControlObservation(t.Context(), *request, "same-bank-reader", o); err == nil {
		t.Fatal("contradictory confirmed bank applied")
	}
	var amount, processed int64
	if err = s.pool.QueryRow(t.Context(), `SELECT wallet.amount_raw,request.processed_slot FROM loyal_yield.balance_sweep_wallet_balances_current wallet JOIN loyal_yield.autodeposit_reconciliation_requests request ON request.target_id=wallet.target_id WHERE wallet.target_id=$1`, id).Scan(&amount, &processed); err != nil {
		t.Fatal(err)
	}
	if amount != 8_000_000 || processed != 0 {
		t.Fatalf("confirmed contradiction overwritten: amount=%d processed=%d", amount, processed)
	}
}
