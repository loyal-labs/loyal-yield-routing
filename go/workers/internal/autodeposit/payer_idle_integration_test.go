package autodeposit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

// Root cause (04e792e0, 6557c221): on 2026-08-07 the shared signer fell to
// 0.001 SOL and every autodeposit stopped for hours, reported as a generic
// failure, and users whose sweeps had been promised heard nothing. The TS
// executor refuses below 0.05 SOL with fee_payer_exhausted and pushes the
// user once per slot. Go defined the result but sent the pull regardless.
// The payer is now read once per pass: exhausted, the pass claims nothing and
// pushes each due slot once per outage; the balance gauge is the one page.
func TestFeePayerExhaustionClaimsNothingAndPushesEachDueSlot(t *testing.T) {
	scenario := newFreshScenario(t, "payer", 0)
	var mu sync.Mutex
	var pushes []map[string]string
	var authorization string
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var push map[string]string
		_ = json.Unmarshal(body, &push)
		mu.Lock()
		pushes, authorization = append(pushes, push), r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	notifier, err := NewSweepNotifier(app.URL+"/api/solana-week/sweep-notify", "itest-notify-secret")
	if err != nil {
		t.Fatal(err)
	}
	notifier.client = app.Client()
	payer := scenario.wires.FeePayer()
	scenario.chain.lamports = map[string]uint64{payer: FeePayerMinimumLamports - 1}
	worker, registry := scenario.worker(t, ControllerDependencies{}, notifier)
	balance := func() float64 {
		return metricValue(t, registry, "loyal_fee_payer_balance_lamports", map[string]string{"payer": payer})
	}

	for pass := 1; pass <= 2; pass++ {
		report, err := worker.Tick(t.Context())
		if err != nil || !report.FeePayerExhausted || len(report.Dispatched) != 0 || report.settled() {
			t.Fatalf("pass %d with an exhausted payer: %+v %v", pass, report, err)
		}
		if got := failedCount(t, registry, "autodeposit_fee_payer_exhausted"); got != 0 || balance() != FeePayerMinimumLamports-1 {
			t.Fatalf("pass %d counted %v exhaustions, gauge %v", pass, got, balance())
		}
	}
	var claims int
	if err := scenario.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1`, scenario.target.TargetID).Scan(&claims); err != nil || claims != 0 || len(scenario.wires.built) != 0 {
		t.Fatalf("exhausted payer claimed %d / built %v: %v", claims, scenario.wires.built, err)
	}
	mu.Lock()
	want := map[string]string{"walletAddress": scenario.wallet, "kind": "failed", "dedupeKey": "slot-" + strconv.FormatInt(scenario.slot, 10)}
	if len(pushes) != 1 || authorization != "Bearer itest-notify-secret" || !reflect.DeepEqual(pushes[0], want) {
		t.Fatalf("pushes %v (auth %q), want %v once per outage", pushes, authorization, want)
	}
	mu.Unlock()

	// A funded pass ends the outage; the next outage pushes the slot again.
	setDue := func(due bool) {
		t.Helper()
		if _, err := scenario.store.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET eligible_after = CASE WHEN $2 THEN now() - interval '1 second' ELSE now() + interval '1 hour' END WHERE id=$1`, scenario.slot, due); err != nil {
			t.Fatal(err)
		}
	}
	setDue(false)
	scenario.chain.lamports = map[string]uint64{payer: FeePayerMinimumLamports}
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	setDue(true)
	scenario.chain.lamports = map[string]uint64{payer: FeePayerMinimumLamports - 1}
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(pushes) != 2 {
		t.Fatalf("a new outage pushed %d times in total, want 2", len(pushes))
	}
	mu.Unlock()

	// Funded again, the same due slot deposits at once: no backoff was taken.
	scenario.chain.lamports = map[string]uint64{payer: FeePayerMinimumLamports}
	report, err := worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsCompleted != 1 || !report.settled() || balance() != FeePayerMinimumLamports {
		t.Fatalf("funded payer outcome %+v %v, gauge %v", report.Outcome, err, balance())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 {
		t.Fatalf("a landed sweep pushed the user: %v", pushes)
	}
}

func TestUnownedResidueDepositsWithThePull(t *testing.T) {
	residue := newFreshScenario(t, "unowned-residue", 3)
	topup := "itest-controller-topup-sig-unowned-residue"
	residue.chain.receipts[topup] = ReceiptEvidence{Signature: topup, Slot: 870_002, Effects: []ReceiptEffect{
		{TokenAccount: "itest-vault-usdc-unowned-residue", Mint: USDCMint, PreRaw: 5_000_003, PostRaw: 0},
		{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_013},
	}}
	worker, _ := residue.worker(t, ControllerDependencies{}, nil)
	report, err := worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsCompleted != 1 {
		t.Fatalf("unowned residue blocked the deposit: %+v %v", report.Outcome, err)
	}
	var deposited int64
	if err := residue.store.pool.QueryRow(t.Context(), `SELECT amount_raw FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1 AND operation_kind='top_up' AND attempt_state='confirmed'`, residue.target.TargetID).Scan(&deposited); err != nil || deposited != 5_000_003 {
		t.Fatalf("top-up deposited %d, want the 5000000 pull plus the 3 residue: %v", deposited, err)
	}
}

func TestOpenCrossMintMovementDefersBeforeItsCustodyReachesTheVault(t *testing.T) {
	window := newFreshScenario(t, "cross-mint-window", 3)
	if _, err := window.store.pool.Exec(t.Context(), `
INSERT INTO loyal_yield.rebalance_decisions (vault_id,status,source_reserve,target_reserve,source_liquidity_mint,target_liquidity_mint,amount_raw,
 source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,decision_reason,execution_plan,idempotency_key,cross_mint_activation_control_generation)
SELECT id,'confirming','cross-mint-window-reserve','cross-mint-window-target',$2,'itest-other-mint',5000000,1,2,1,0,'target_supply_apy_exceeds_source','{}'::jsonb,'itest-cross-mint-window',1
FROM loyal_yield.managed_vaults WHERE vault_pubkey=$1`, "itest-vault-cross-mint-window", USDCMint); err != nil {
		t.Fatal(err)
	}
	worker, _ := window.worker(t, ControllerDependencies{}, nil)
	report, err := worker.Tick(t.Context())
	if report.Outcome.ExecutionsDeferred != 1 || report.Outcome.ExecutionsCompleted != 0 || len(window.wires.built) != 0 {
		t.Fatalf("an open cross-mint movement did not defer the deposit: %+v built %v: %v", report.Outcome, window.wires.built, err)
	}
}
