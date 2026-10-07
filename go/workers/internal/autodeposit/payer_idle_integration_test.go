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
// failure, and the users whose sweeps had been promised heard nothing. The TS
// executor refuses the pull below 0.05 SOL with fee_payer_exhausted, pushes
// the user once per slot and warns below 0.55 SOL. The Go port defined the
// result but never produced it: it built and sent the pull regardless.
func TestFeePayerExhaustionStopsThePullAndPushesTheUser(t *testing.T) {
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
	scenario.chain.lamports = map[string]uint64{scenario.wires.FeePayer(): FeePayerMinimumLamports - 1}
	worker, registry := scenario.worker(t, ControllerDependencies{Notifier: notifier})

	report, err := worker.Tick(t.Context())
	if err != nil || len(report.Alerts) != 1 || report.Alerts[0].Code != "autodeposit_fee_payer_exhausted" {
		t.Fatalf("exhausted payer outcome %+v %v", report, err)
	}
	if len(scenario.wires.built) != 0 || failedCount(t, registry, "autodeposit_fee_payer_exhausted") != 1 {
		t.Fatalf("exhausted payer built %v, counted %v", scenario.wires.built, failedCount(t, registry, "autodeposit_fee_payer_exhausted"))
	}
	var claim string
	if err := scenario.store.pool.QueryRow(t.Context(), `SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1`, scenario.target.TargetID).Scan(&claim); err != nil || claim != "released" {
		t.Fatalf("exhausted payer kept the claim: %s %v", claim, err)
	}
	mu.Lock()
	want := map[string]string{"walletAddress": scenario.wallet, "kind": "failed", "dedupeKey": "slot-" + strconv.FormatInt(scenario.slot, 10)}
	if len(pushes) != 1 || authorization != "Bearer itest-notify-secret" || !reflect.DeepEqual(pushes[0], want) {
		t.Fatalf("pushes %v (auth %q), want one %v", pushes, authorization, want)
	}
	mu.Unlock()

	// A payer above the floor but below the low mark still deposits, warning.
	scenario.chain.lamports = map[string]uint64{scenario.wires.FeePayer(): FeePayerLowLamports - 1}
	elapseReleaseDelay(t, scenario.store, scenario.target.TargetID)
	report, err = worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsCompleted != 1 || failedCount(t, registry, "autodeposit_fee_payer_low") != 1 {
		t.Fatalf("low payer outcome %+v %v, low count %v", report.Outcome, err, failedCount(t, registry, "autodeposit_fee_payer_low"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 1 {
		t.Fatalf("a landed sweep pushed the user: %v", pushes)
	}
}

// Root cause (9467ebf5, 2089d5fa, ASK-2164): fleet same-mint routes withdraw
// all source collateral but deposit the planned liquidity, so the interest
// accrued since planning stays in the vault ATA, and nothing drains idle
// custody. The Go port refused any idle above zero, so a vault holding 3 raw
// units could never autodeposit again. Residue up to the $25 tolerance rides
// beside the pull; idle above it defers and is counted.
func TestIdleResidueRidesBesideThePullAndLargeIdleDefers(t *testing.T) {
	residue := newFreshScenario(t, "residue", 3)
	worker, _ := residue.worker(t, ControllerDependencies{IdleToleranceRaw: 25_000_000})
	report, err := worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsCompleted != 1 {
		t.Fatalf("idle residue blocked the deposit: %+v %v", report.Outcome, err)
	}
	var destinationPre int64
	if err := residue.store.pool.QueryRow(t.Context(), `SELECT destination_pre_balance_raw FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1 AND operation_kind='pull'`, residue.target.TargetID).Scan(&destinationPre); err != nil || destinationPre != 3 {
		t.Fatalf("pull recorded custody pre-balance %d, want the observed 3: %v", destinationPre, err)
	}

	owned := newFreshScenario(t, "owned-idle", 25_000_001)
	worker, registry := owned.worker(t, ControllerDependencies{IdleToleranceRaw: 25_000_000})
	report, err = worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsDeferred != 1 || len(report.Alerts) != 0 || len(owned.wires.built) != 0 {
		t.Fatalf("idle above tolerance outcome %+v alerts %v built %v: %v", report.Outcome, report.Alerts, owned.wires.built, err)
	}
	if failedCount(t, registry, "autodeposit_idle_blocked") != 1 {
		t.Fatal("blocked idle was not counted")
	}
}
