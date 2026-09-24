package backyardrwa

import (
	"context"
	"encoding/binary"
	"math/big"
	"reflect"
	"testing"
)

func releaseAdmissionFixture(t *testing.T, output uint64) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := fundingAdmissionFixture(t, output)
	route := ethenaUSDePYUSD
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	observed, full, err := observeKaminoPayoffWindow(context.Background(), rpc, route, 42, 5)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := decodeKaminoRepaymentRelease(full, route, observed.ObservedSlot)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, bound.ReceiptRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	r.RepaymentRelease = true
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects, err := exactKaminoTokenEffects(full, source, destination, bound.LiquidityRaw)
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: DeleverRouteStep, AmountRaw: 1, StrategyKey: route.Lane, Reason: "withdrawal_release_repayment_collateral", IdempotencyKey: "release-return"}
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func TestReleaseAdmissionReservesFundingPayoffAndCompleteReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := releaseAdmissionFixture(t, 20_000)
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{ReportNAV, SwapCollateralToDebtStep, ReportNAV, DeleverRouteStep, ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.Snapshot != o.Snapshot || plan.Payoff.ThroughUnix != 1300 || plan.ValidThroughSlot > 74 || plan.FundingSwap == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil {
		t.Fatal("release omitted return, lost actual snapshot or extended freshness", actions)
	}
	r, _, _, err := plan.Input.decode()
	if err != nil || !r.(KaminoPrimeUSDCRequest).RepaymentRelease || r.(KaminoPrimeUSDCRequest).AmountRaw != e.Request.AmountRaw {
		t.Fatal("future template replaced current release")
	}
	w, _, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || w.(KaminoPrimeUSDCRequest).AmountRaw != uint64(o.Snapshot.PositionCollateralRaw)-e.Request.AmountRaw {
		t.Fatal("remaining receipt withdrawal not reserved")
	}
	if binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 0 {
		t.Fatal("cost projection mutated current custody")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3Withdrawal(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
}

func TestReleaseAdmissionRejectsUnsafeFundingAndChangedSignedState(t *testing.T) {
	o, d, e, m, rpc, client, _ := releaseAdmissionFixture(t, 1_000)
	_, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
	o, d, e, m, rpc, client, _ = releaseAdmissionFixture(t, 900_000)
	_, err = legacyAdmissionCostCheck(observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects))
	assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
	_, _, e, _, rpc, _, accounts := releaseAdmissionFixture(t, 20_000)
	encoded, _ := jsonMarshalExpectedEffects(e.ExpectedEffects)
	input, _ := encodePhase3BuildInput(e.Request, encoded)
	digest, _ := Phase3IntentDigest(e.Request, encoded)
	message, _ := CompileKaminoMessage(e.Request)
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, SignedWireSHA256: op.SignedWireSHA256, BuildInput: input}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 1)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "repayment_release_debt_cash_changed")
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 1)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "repayment_release_effects_changed")
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 0)
	// A rate/debt move cannot retain the original maximum release authority.
	putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(2_000), 60))
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "repayment_release_exceeds_safe_size")
}

// The release is re-checked at build and send on a raw five-step capture, so
// sizing must use the raw capture too (one window longer for headroom). Live
// 2026-09-24: sizing on the refreshed-reserve simulation produced a release
// 290 receipts above what the raw re-check allowed, and every withdrawal held.
func TestRawRepaymentReleaseSizingPassesSendRecheck(t *testing.T) {
	_, _, e, m, rpc, _, _ := releaseAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	sized, rows, err := m.observeRawRepaymentRelease(context.Background(), rpc, route, 42, false)
	if err != nil || sized.ReceiptRaw == 0 || sized.ReceiptRaw > e.Request.AmountRaw {
		t.Fatal("raw six-step sizing must not exceed the five-step safe size", sized.ReceiptRaw, e.Request.AmountRaw, err)
	}
	r := e.Request
	r.AmountRaw = sized.ReceiptRaw
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects, err := exactKaminoTokenEffects(rows, source, destination, sized.LiquidityRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.validateRepaymentReleaseRequest(context.Background(), rpc, r, effects, 42); err != nil {
		t.Fatal("sized release refused by its own send re-check", err)
	}
}

// Build and send re-check a full payoff on a raw one-step capture, so the
// wire is sized on a raw three-step capture. Live 2026-09-24: sizing on the
// refreshed simulation left the wire under the send-time bound and every
// withdrawal repay held with full_payoff_request_underfunded.
func TestRawFullPayoffSizingPassesSendRecheck(t *testing.T) {
	_, _, _, m, rpc, _, accounts := releaseAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	// Funded debt custody, served by the fixture RPC to every capture.
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 1_000_000)
	sized, rows, err := observeRawFullPayoff(context.Background(), rpc, route, 42)
	if err != nil {
		t.Fatal(err)
	}
	check, _, err := observeKaminoPayoffBound(context.Background(), rpc, route, 42)
	if err != nil || sized.UpperDebtRaw < check.UpperDebtRaw || sized.ObservedDebtRaw > check.ObservedDebtRaw {
		t.Fatal("three-step raw payoff must cover the one-step send bound", sized, check, err)
	}
	r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, sized.UpperDebtRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves, r.FullPayoff = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}, true
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(rows, source, destination, sized.ObservedDebtRaw, sized.UpperDebtRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.validateFullPayoffRequest(context.Background(), rpc, r, effects, 42); err != nil {
		t.Fatal("raw-sized payoff refused by its own send re-check", err)
	}
}
