package observer

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func testPosition(raw, liquidity string) FleetPosition {
	return FleetPosition{Reserve: benchmarkReserve, Market: benchmarkMarket, Mint: USDCMint, AmountRaw: raw, Metadata: json.RawMessage(`{"amountSemantics":"kamino_obligation_collateral_deposited_amount","redeemable_liquidity_amount_raw":"` + liquidity + `"}`)}
}
func testVault(id int64, at time.Time, p FleetPosition) FleetVault {
	zero := "0"
	return FleetVault{ID: id, Snapshot: &FleetSnapshot{ObservedAt: at, Slot: 100, ContextIdle: &zero, Positions: []FleetPosition{p}}}
}
func TestAllocationPreservesConversionCoverageAndClocks(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 30, 0, 0, time.UTC)
	actual := now.Add(-20 * time.Minute)
	good := testVault(1, actual, testPosition("1000000000", "1200000000"))
	unknown := testVault(2, actual, testPosition("1000000000", ""))
	stale := testVault(3, now.Add(-7*time.Hour), testPosition("1000000000", "1200000000"))
	future := testVault(4, now.Add(time.Second), testPosition("1000000000", "1200000000"))
	mismatched := testVault(5, actual, testPosition("1000000000", "1200000000"))
	mismatched.Snapshot.ContextIdle = nil
	mismatched.CurrentIdle = []FleetIdle{{Mint: USDCMint, AmountRaw: "10", Slot: 99}}
	sample, err := AggregateAllocation([]FleetVault{good, unknown, stale, future, mismatched, {ID: 6}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Included != 1 || sample.Invalid != 3 || sample.Stale != 1 || sample.Missing != 1 || sample.Total != 6 || sample.ReserveAmounts[benchmarkReserve] != "1200000000" || sample.IdleAmountRaw != "0" {
		t.Fatalf("unverified units entered allocation: %+v", sample)
	}
	if sample.OldestSourceAt == nil || !sample.OldestSourceAt.Equal(actual) || !sample.NewestSourceAt.Equal(actual) {
		t.Fatalf("source clock replaced: %+v", sample)
	}
	prices := SnapshotSharePrices([]FleetVault{good, unknown, stale, future}, now)
	if len(prices) != 1 || prices[0].Price != 1.2 || prices[0].Slot != 100 || !prices[0].ObservedAt.Equal(actual) {
		t.Fatalf("conversion price = %+v", prices)
	}
}
func TestAllocationUnknownIdleAndConflictingConversionNeverBecomeZero(t *testing.T) {
	now := time.Now().UTC()
	vault := testVault(1, now, testPosition("10", "20"))
	vault.Snapshot.ContextIdle = nil
	s, err := AggregateAllocation([]FleetVault{vault}, now)
	if err != nil || s.Invalid != 1 || len(s.ReserveAmounts) != 0 {
		t.Fatalf("missing idle accepted: %+v %v", s, err)
	}
	vault.CurrentIdle = []FleetIdle{{Mint: USDCMint, AmountRaw: "0", Slot: 100}}
	s, err = AggregateAllocation([]FleetVault{vault}, now)
	if err != nil || s.Included != 1 {
		t.Fatalf("actual zero refused: %+v %v", s, err)
	}
	vault.Snapshot.Positions[0].Metadata = json.RawMessage(`{"amountSemantics":"kamino_obligation_collateral_deposited_amount","redeemable_liquidity_amount_raw":"20","redeemable_source_liquidity_amount_raw":"21"}`)
	s, err = AggregateAllocation([]FleetVault{vault}, now)
	if err != nil || s.Invalid != 1 || len(s.ReserveAmounts) != 0 {
		t.Fatalf("conflicting conversion accepted: %+v %v", s, err)
	}
}
func TestAllocationIntegerPrecisionAndInputBounds(t *testing.T) {
	now := time.Now().UTC()
	a := testVault(1, now, testPosition("9007199254740993", "9007199254740993"))
	b := testVault(2, now, testPosition("9007199254740993", "9007199254740993"))
	sample, err := AggregateAllocation([]FleetVault{a, b}, now)
	if err != nil || sample.ReserveAmounts[benchmarkReserve] != "18014398509481986" {
		t.Fatalf("integer precision lost: %+v %v", sample, err)
	}
	a.Snapshot.Positions[0].AmountRaw = "9223372036854775808"
	sample, err = AggregateAllocation([]FleetVault{a}, now)
	if err != nil || sample.Invalid != 1 {
		t.Fatalf("BIGINT overflow admitted: %+v %v", sample, err)
	}
	a.Snapshot.Positions = make([]FleetPosition, 129)
	if _, err = AggregateAllocation([]FleetVault{a}, now); err == nil {
		t.Fatal("partial bounded allocation published")
	}
}
func TestPublicModelChargesCrossMintFeeAndRejectsFutureEvidence(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rows := []ModelReserve{{Reserve: "reserve", Mint: stableMints[1], ObservedAt: start, APY: .1, TotalSupplyUSD: 200001, Active: true}, {Reserve: "future", Mint: USDCMint, ObservedAt: end.Add(time.Second), APY: .49, TotalSupplyUSD: 200001, Active: true}}
	result, err := SimulatePublicModel(rows, start, end)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := modeledBPS(.9999*math.Pow(1.1, 1.0/365), 24*time.Hour)
	if result.APYBPS != want {
		t.Fatalf("cross-mint fee/future rate changed result: %d want %d", result.APYBPS, want)
	}
	rows[0].APY = math.NaN()
	if _, err = SimulatePublicModel(rows, start, end); err == nil {
		t.Fatal("nonfinite rate admitted")
	}
}
func TestPublicModelNewestSlotInvalidationAndBenchmarkCompounding(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rows := []ModelReserve{{Reserve: "reserve", Mint: USDCMint, ObservedAt: start, Slot: 1, APY: .1, TotalSupplyUSD: 200001, Active: true}, {Reserve: "reserve", Mint: USDCMint, ObservedAt: start.Add(12 * time.Hour), Slot: 3, APY: .1, TotalSupplyUSD: 200001, Active: true, Stale: true}, {Reserve: "reserve", Mint: USDCMint, ObservedAt: start.Add(12 * time.Hour), Slot: 2, APY: .2, TotalSupplyUSD: 200001, Active: true}}
	result, err := SimulatePublicModel(rows, start, end)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := modeledBPS(math.Pow(1.1, 1.0/730), 24*time.Hour)
	if result.APYBPS != want {
		t.Fatalf("newest stale slot did not remove rate: %+v want %d", result, want)
	}
	benchmark, err := BenchmarkModel([]ModelReserve{{ObservedAt: start.Add(-time.Hour), APY: .1}, {ObservedAt: start.Add(12 * time.Hour), APY: .2}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	want, _ = modeledBPS(math.Pow(1.1, 1.0/730)*math.Pow(1.2, 1.0/730), 24*time.Hour)
	if len(benchmark) != 2 || benchmark[1].APYBPS != want || benchmark[1].APYBPS == 2000 {
		t.Fatalf("benchmark published spot instead of cumulative APY: %+v want %d", benchmark, want)
	}
}

// Saved health detector source: one expired market, missing ready timestamp,
// ALT backlog still within its distinct SLO, and one overdue sender.
const stageFixture = `[{"cluster":"fixture","planner_registered_at":"2026-09-30T12:00:00Z","planner_last_seen_at":"2026-09-30T12:02:00Z","latest_market_epoch_id":1,"latest_market_expires_at":"2026-09-30T12:01:55Z","ready_opportunity_count":2,"oldest_ready_state_entered_at":null,"waiting_alt_opportunity_count":1,"oldest_waiting_alt_state_entered_at":"2026-09-30T12:00:30Z","sender_submission_count":3,"oldest_sender_state_entered_at":"2026-09-30T12:01:40Z","confirmer_submission_count":0,"oldest_confirmer_state_entered_at":null,"reconciler_submission_count":0,"oldest_reconciler_state_entered_at":null}]`

func TestStageHealthDistinctDeadlinesAndMissingTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 2, 0, 0, time.UTC)
	report, err := StageHealth([]byte(stageFixture), 5*time.Second, 5*time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Signals) != 6 || len(report.StuckStages) != 3 {
		t.Fatalf("stage set = %+v", report)
	}
	want := []string{"market_epoch", "ready", "sender"}
	delay := []int64{5000, 0, 10000}
	for i, d := range report.StuckStages {
		if d.Stage != want[i] || d.DetectionMilliseconds != delay[i] {
			t.Fatalf("wrong deadline: %+v", report)
		}
	}
	report, err = StageHealth([]byte(stageFixture), 30*time.Second, time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StuckStages) != 2 {
		t.Fatalf("poll threshold not respected: %+v", report)
	}
	if _, err = StageHealth([]byte(strings.TrimSuffix(stageFixture, "]")+`,{"cluster":"other"}]`), time.Second, time.Second, now); err == nil {
		t.Fatal("mixed cluster health accepted")
	}
	if _, err = StageHealth([]byte(`[]`), time.Second, time.Second, now); err == nil {
		t.Fatal("empty status invented healthy fleet")
	}
}

// Saved source-shaped SQL payload: exact BIGINT and PostgreSQL UTC clocks.
const healthSQLFixture = `[{"cluster":"fixture","opportunity_state":null,"opportunity_count":1,"principal_usd_micros":9007199254740993,"annual_yield_gain_usd_micros":0,"yield_gain_usd_micros_per_hour":0,"oldest_created_at":"2026-09-30T12:00:00+00:00","oldest_state_entered_at":null,"oldest_age_seconds":null,"oldest_state_age_seconds":null,"expired_lease_count":0,"pending_outbox_count":0,"pending_submission_count":0,"pending_compiled_fee_lamports":0,"expiry_check_pending_count":0,"effect_ambiguous_count":0,"oldest_pending_submission_at":null,"oldest_pending_submission_age_seconds":null,"sender_submission_count":0,"oldest_sender_state_entered_at":null,"oldest_sender_state_age_seconds":null,"confirmer_submission_count":0,"oldest_confirmer_state_entered_at":null,"oldest_confirmer_state_age_seconds":null,"reconciler_submission_count":0,"oldest_reconciler_state_entered_at":null,"oldest_reconciler_state_age_seconds":null,"planner_registered_at":"2026-09-30T12:00:00+00:00","planner_last_seen_at":"2026-09-30T12:02:00+00:00","planner_last_seen_age_seconds":null,"full_sweep_started_at":null,"full_sweep_completed_at":null,"full_sweep_age_seconds":null,"planned_optimizer_epoch_key":null,"planned_optimizer_epoch_expires_at":null,"complete_frontier":null,"observed_vault_count":null,"planned_opportunity_count":null,"planned_selected_count":null,"planned_deferred_count":null,"planning_generation":null,"latest_market_epoch_id":1,"latest_market_epoch_key":null,"latest_market_slot":null,"latest_market_observed_at":null,"latest_market_expires_at":"2026-09-30T12:03:00+00:00","latest_market_epoch_age_seconds":null,"latest_market_epoch_expires_in_seconds":null,"latest_market_epoch_expired":null,"planner_epoch_matches_latest":null,"waiting_alt_opportunity_count":0,"waiting_alt_principal_usd_micros":0,"waiting_alt_yield_gain_usd_micros_per_hour":0,"oldest_waiting_alt_state_entered_at":null,"oldest_waiting_alt_state_age_seconds":null,"ready_opportunity_count":0,"ready_principal_usd_micros":0,"ready_yield_gain_usd_micros_per_hour":0,"oldest_ready_state_entered_at":null,"oldest_ready_state_age_seconds":null,"current_epoch_opportunity_count":0,"current_epoch_principal_usd_micros":0,"current_epoch_recoverable_yield_usd_micros_per_hour":0,"current_epoch_submitted_within_10s_yield_ppm":0,"current_epoch_submitted_within_2m_yield_ppm":0,"current_epoch_submitted_within_10m_yield_ppm":0,"current_epoch_confirmed_within_30s_yield_ppm":0,"current_epoch_submission_p95_milliseconds":null,"current_epoch_confirmation_p95_milliseconds":null,"current_epoch_compiled_fee_lamports":0,"active_physical_writable_key_count":1,"top_physical_writable_key_congestion":[{"writable_account_key":"physical-key","classification":"payer","active_submission_count":1,"principal_usd_micros":9007199254740993,"recoverable_yield_usd_micros_per_hour":7}],"future_view_column":"must-not-leak"}]`

func TestHealthPayloadParityPreservesInt64AndUTCContract(t *testing.T) {
	payload, err := normalizeHealthPayload([]byte(healthSQLFixture))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "future_view_column") || !strings.Contains(string(payload), "9007199254740993") || !strings.Contains(string(payload), `"oldest_created_at":"2026-09-30T12:00:00Z"`) {
		t.Fatalf("SQL payload changed public contract: %s", payload)
	}
	report, err := StageHealth(payload, 5*time.Second, time.Second, time.Date(2026, 9, 30, 12, 2, 0, 0, time.UTC))
	if err != nil || len(report.StuckStages) != 0 {
		t.Fatalf("healthy control not clear: %+v %v", report, err)
	}
	var rows []map[string]json.RawMessage
	if err = json.Unmarshal([]byte(healthSQLFixture), &rows); err != nil {
		t.Fatal(err)
	}
	delete(rows[0], "pending_submission_count")
	bad, _ := json.Marshal(rows)
	if _, err = normalizeHealthPayload(bad); err == nil {
		t.Fatal("missing source field invented zero")
	}
	rows[0]["pending_submission_count"] = json.RawMessage(`0`)
	rows[0]["top_physical_writable_key_congestion"] = json.RawMessage(`[{"writable_account_key":"key","classification":"semantic_lane","active_submission_count":1,"principal_usd_micros":1,"recoverable_yield_usd_micros_per_hour":1}]`)
	bad, _ = json.Marshal(rows)
	if _, err = normalizeHealthPayload(bad); err == nil {
		t.Fatal("semantic lane represented as physical congestion")
	}
}
func TestPublicForecastPublishedSeriesContract(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	model := PublicModel{Samples: []ModelPoint{{ObservedAt: at, APYBPS: 1000}}, Benchmark: []ModelPoint{{ObservedAt: at, APYBPS: 900}}}
	samples, series, err := marshalPublicModel(model)
	if err != nil {
		t.Fatal(err)
	}
	if string(samples) != `[{"observedAt":"2026-09-30T12:00:00Z","apyBps":1000}]` {
		t.Fatalf("samples contract: %s", samples)
	}
	var parsed []struct {
		Key, Label string
		Metadata   map[string]string
		Samples    []ModelPoint
	}
	if err = json.Unmarshal(series, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0].Key != "loyal" || parsed[0].Label != "Loyal Earn" || parsed[1].Key != "mainUsdcReserve" || parsed[1].Label != "Kamino Main USDC" || parsed[1].Metadata["reserve"] != benchmarkReserve || parsed[1].Metadata["market"] != benchmarkMarket || parsed[1].Metadata["liquidityMint"] != USDCMint || parsed[1].Metadata["metric"] != "cumulative_annualized_apy_bps" || parsed[0].Metadata["metric"] != "cumulative_annualized_apy_bps" || parsed[1].Samples[0].APYBPS != 900 {
		t.Fatalf("Apps series compatibility lost: %s", series)
	}
}

func TestStageUnknownBacklogIsNotEmptyBacklog(t *testing.T) {
	unknown := strings.Replace(stageFixture, `"ready_opportunity_count":2`, `"ready_opportunity_count":null`, 1)
	if _, err := StageHealth([]byte(unknown), time.Second, time.Second, time.Now().UTC()); err == nil {
		t.Fatal("unknown backlog became zero")
	}
}

func TestHealthSQLNumericDriftCannotBecomePublishedFloatMoney(t *testing.T) {
	bad := strings.Replace(healthSQLFixture, `"principal_usd_micros":9007199254740993`, `"principal_usd_micros":1.5`, 1)
	if _, err := normalizeHealthPayload([]byte(bad)); err == nil {
		t.Fatal("SQL float principal published as integer-money contract")
	}
	bad = strings.Replace(healthSQLFixture, `"pending_submission_count":0`, `"pending_submission_count":null`, 1)
	if _, err := normalizeHealthPayload([]byte(bad)); err == nil {
		t.Fatal("required source counter unknown accepted")
	}
}
