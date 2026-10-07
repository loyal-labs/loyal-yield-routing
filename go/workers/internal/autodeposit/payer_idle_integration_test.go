package autodeposit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
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

// Root cause (9467ebf5, 2089d5fa, ASK-2164): Rust fleet same-mint routes
// withdraw all source collateral but deposit the planned liquidity, so the
// interest accrued since planning stays in the vault ATA, and nothing drains
// idle custody. The Go port refused any idle above zero, so a vault holding
// 3 raw units could never autodeposit again. Residue up to the $25 tolerance
// rides beside the pull.
func TestIdleResidueRidesBesideThePull(t *testing.T) {
	residue := newFreshScenario(t, "residue", 3)
	worker, _ := residue.worker(t, ControllerDependencies{IdleToleranceRaw: 25_000_000}, nil)
	report, err := worker.Tick(t.Context())
	if err != nil || report.Outcome.ExecutionsCompleted != 1 {
		t.Fatalf("idle residue blocked the deposit: %+v %v", report.Outcome, err)
	}
	var destinationPre int64
	if err := residue.store.pool.QueryRow(t.Context(), `SELECT destination_pre_balance_raw FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1 AND operation_kind='pull'`, residue.target.TargetID).Scan(&destinationPre); err != nil || destinationPre != 3 {
		t.Fatalf("pull recorded custody pre-balance %d, want the observed 3: %v", destinationPre, err)
	}
}

// The Go port claimed, then released idle-blocked slots: the slot went failed
// with generic text and schedule repair cleared last_error, so the Rust
// trigger's overdue check (which reads the TS marker) could never see a stuck
// deposit after a fallback. The deferral now happens before any claim, as
// the TS executor's deferIdleVaultScheduledSlot does, and the Rust overdue
// query itself reports the slot; the gauge reads the same first-blocked clock.
func TestIdleAboveToleranceDefersBeforeClaimingWithTheRustVisibleMarker(t *testing.T) {
	owned := newFreshScenario(t, "owned-idle", 25_000_001)
	ctx := t.Context()
	worker, registry := owned.worker(t, ControllerDependencies{IdleToleranceRaw: 25_000_000}, nil)
	marker := func() (status, lastError string, waits bool) {
		t.Helper()
		if err := owned.store.pool.QueryRow(ctx, `SELECT status::text, last_error, eligible_after > now() + interval '4 minutes' FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1`, owned.slot).Scan(&status, &lastError, &waits); err != nil {
			t.Fatal(err)
		}
		return status, lastError, waits
	}
	var since string
	for deferral := 1; deferral <= 3; deferral++ {
		report, err := worker.Tick(ctx)
		if err != nil || report.Outcome.ExecutionsDeferred != 1 || len(report.Alerts) != 0 || len(owned.wires.built) != 0 || report.settled() {
			t.Fatalf("deferral %d: outcome %+v alerts %v built %v: %v", deferral, report.Outcome, report.Alerts, owned.wires.built, err)
		}
		status, lastError, waits := marker()
		prefix := "existing idle vault balance must drain before direct autodeposit: 25000001 [idle_blocked_since="
		suffix := "; idle_deferrals=" + strconv.Itoa(deferral) + "]"
		if status != "scheduled" || !waits || !strings.HasPrefix(lastError, prefix) || !strings.HasSuffix(lastError, suffix) {
			t.Fatalf("deferral %d left slot %s %q (waits %v)", deferral, status, lastError, waits)
		}
		blocked := strings.TrimSuffix(strings.TrimPrefix(lastError, prefix), suffix)
		if since != "" && blocked != since {
			t.Fatalf("first-blocked time moved from %s to %s", since, blocked)
		}
		since = blocked
		elapseReleaseDelay(t, owned.store, owned.target.TargetID)
	}
	var claims int
	if err := owned.store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1`, owned.target.TargetID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("idle-blocked target was claimed %d times: %v", claims, err)
	}
	if failedCount(t, registry, "autodeposit_idle_blocked") != 0 {
		t.Fatal("a deferral was counted as a failure")
	}

	// Blocked for two hours: the restarted Rust trigger's own overdue query
	// reports it, and so does the gauge.
	if _, err := owned.store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET last_error = regexp_replace(last_error, 'idle_blocked_since=[0-9]+', 'idle_blocked_since=' || (extract(epoch FROM now())::bigint - 7200)) WHERE id=$1`, owned.slot); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_idle_token_balances_current(vault_id,mint,amount_raw,owner,token_account,observed_slot,observed_at,source_commitment,updated_at) SELECT $1,$2,25000001,vault_pubkey,'itest-vault-usdc-owned-idle',1,now(),'confirmed',now() FROM loyal_yield.managed_vaults WHERE id=$1`, owned.target.ManagedVaultID, USDCMint); err != nil {
		t.Fatal(err)
	}
	var stage string
	var slot int64
	if err := owned.store.pool.QueryRow(ctx, `SELECT owning_stage, scheduled_slot_id FROM (`+rustOverdueSQL(t)+`) AS overdue`, USDCMint, int64(25_000_000)).Scan(&stage, &slot); err != nil {
		t.Fatalf("Rust overdue check does not see the Go deferral: %v", err)
	}
	if stage != "preflight_idle_drain" || slot != owned.slot {
		t.Fatalf("Rust overdue reported %s slot %d", stage, slot)
	}
	if err := worker.passFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if age := metricValue(t, registry, "loyal_autodeposit_oldest_due_lot_age_seconds", nil); age < 7190 || age > 7300 {
		t.Fatalf("oldest due age %v, want the two hours since first blocked", age)
	}
}

// rustOverdueSQL is OVERDUE_AUTODEPOSIT_WORK_SQL from the Rust trigger, the
// fallback's reader of these rows.
func rustOverdueSQL(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("../../../../crates/balance-sweep-autodeposit-trigger/src/main.rs")
	if err != nil {
		t.Fatal(err)
	}
	const start = `const OVERDUE_AUTODEPOSIT_WORK_SQL: &str = r#"`
	text := string(source)
	i := strings.Index(text, start)
	j := strings.Index(text[i+len(start):], `"#;`)
	if i < 0 || j < 0 {
		t.Fatal("Rust overdue query not found")
	}
	return text[i+len(start) : i+len(start)+j]
}
