package backyardrwa

// AUTO cleanup integration tests. They drive the EXISTING return path on a
// candidate manifest — observePhase3WithdrawalAdmission,
// observePhase3CollateralReturnAdmission and pricePhase3CollateralReturn —
// over ACTUAL post-payoff observations (production observer, flat or
// debt-free obligation, custody balances the observer really reports).
//
// Two things are deliberately kept apart:
//
//  1. Fresh balance and conversion pricing: what the existing admissions do
//     with observed receipts, AUTO custody and PYUSD residue, including
//     refreshed extra proceeds and zero/stale/ambiguous/wrong-lane refusals.
//  2. Durable PYUSD lane attribution: OPEN (doc 19). The AUTO and Ethena
//     routes share one PYUSD debt custody account, so observed balances plus
//     a caller-filled StrategyKey prove token custody identity, never which
//     lane's operation left the residue. Every pricing test below labels the
//     residue attribution as a fixture ASSUMPTION, and
//     TestAutoCleanupSharedPYUSDAttributionGateRemainsOpen documents the gap
//     as repo facts. No test here claims journal proof; no store or journal
//     semantics are changed (that recovery plumbing is A922's).

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// autoCleanupState is the post-payoff state the observer must actually
// report: what the obligation still carries and what the custodies hold.
type autoCleanupState struct {
	receipts     uint64 // obligation deposit receipts still observed
	custodyAUTO  uint64 // route.CollateralCustody AUTO token balance
	custodyPYUSD uint64 // shared PYUSD debt custody balance (see attribution note)
	zeroSquads   bool   // terminal variant: shared USDC custody already staged away
}

// autoCleanupObservation shapes one owned AUTO batch into a post-payoff
// state and runs the production confirmed-route observer over it. The
// strategy's USDC custody is already zero in the owned batch
// (VoltrStrategyIdleRaw decodes from bridgeStrategyATA, not from the
// strategy receipt position), so no strategy byte is touched.
func autoCleanupObservation(t *testing.T, slot int64, state autoCleanupState) (RouteManifest, RuntimeRoute, Observation, []ConfirmedAccount) {
	t.Helper()
	manifest, route, accounts := autoObservationBatch(t, slot, nil)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], state.custodyAUTO)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], state.custodyPYUSD)
	if state.zeroSquads {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
	}
	img := kaminoObligationImage(t, route, slot, state.receipts, 0)
	copy(accountAt(accounts, route.Kamino.Obligation).Data, img.Data)
	observation, _, err := autoObservationForAccounts(manifest, slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observation.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("auto-cleanup-trace"))
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	s := observation.Snapshot
	if s.RouteLane != route.Lane || s.StrategyKey != route.Lane || !s.Fresh || s.Nonterminal != "" || s.HasAmbiguousSubmission {
		t.Fatalf("cleanup observation lost the candidate lane identity: %+v", s)
	}
	if s.CollateralIdleRaw != int64(state.custodyAUTO) || s.DebtIdleRaw != int64(state.custodyPYUSD) {
		t.Fatalf("observer did not report the shaped custody: %+v vs %+v", s, state)
	}
	if s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 {
		t.Fatalf("cleanup state must leave strategy USDC custody empty: %+v", s)
	}
	if state.receipts == 0 && (s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0) {
		t.Fatalf("flat obligation did not observe flat: %+v", s)
	}
	if state.receipts > 0 && (!s.HasPosition || s.PositionCollateralRaw != int64(state.receipts) || s.PositionDebtRaw != 0) {
		t.Fatalf("debt-free obligation did not observe receipts debt-free: %+v", s)
	}
	return manifest, route, observation, accounts
}

// autoCleanupWithdrawalEffects builds the debt-free withdrawal evidence from
// OBSERVED units only: the redemption is decoded from the same reserve bytes
// the observer read (redeemLiquidityRaw at the observed liquidity-per-receipt
// ratio), and the source side moves the observed supply custody balance. No
// fixture ratio is fabricated.
func autoCleanupWithdrawalEffects(t *testing.T, route RuntimeRoute, accounts []ConfirmedAccount, state autoCleanupState) (ExpectedEffects, uint64) {
	t.Helper()
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := reserve.redeemLiquidityRaw(state.receipts)
	if err != nil {
		t.Fatal(err)
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	if source.Address != route.CollateralLiquiditySupply || destination.Address != route.CollateralCustody {
		t.Fatalf("withdrawal custody plan drifted: %s -> %s", source.Address, destination.Address)
	}
	supplyBefore := binary.LittleEndian.Uint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72])
	if redeemed > supplyBefore {
		t.Fatalf("observed redemption exceeds observed liquidity: %d > %d", redeemed, supplyBefore)
	}
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: supplyBefore, AfterRaw: supplyBefore - redeemed},
		{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: state.custodyAUTO, AfterRaw: state.custodyAUTO + redeemed},
	}}
	return effects, redeemed
}

func autoCleanupRPC(t *testing.T, slot int64, accounts []ConfirmedAccount) *RPCClient {
	t.Helper()
	return autoPayoffRPC(t, slot, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, autoAUTOPYUSD)...))
}

func autoCleanupStaleRPC(t *testing.T, slot int64, accounts []ConfirmedAccount) *RPCClient {
	t.Helper()
	base := autoCleanupRPC(t, slot, accounts)
	inner := base.client.Transport
	base.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &call) != nil || call.Method != "getSlot" {
			return inner.RoundTrip(request)
		}
		// Confirm one slot BELOW the observed policy slot: every read is
		// behind the confirmed floor, so the return pricing must fail closed.
		return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d}`, slot-1)), nil
	})
	return base
}

func autoCleanupClient(t *testing.T, route RuntimeRoute) *jupiterClient {
	t.Helper()
	return autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
}

// TestAutoCleanupWithdrawalAdmissionPricesRefreshedExtraProceedsFully proves
// the existing debt-free withdrawal admission converts the FULL observed
// remainder: refreshed receipts that exceed the retained doc-17 forecast
// bound plus the AUTO custody leftover, with the PYUSD residue priced after
// the collateral conversion. Bounds are uncertainty bands, not balances — the
// admission must never truncate freshly observed proceeds to any bound.
//
// ATTRIBUTION ASSUMPTION (open, doc 19): the PYUSD in the shared debt custody
// is treated as this lane's residue by fixture construction only. Nothing
// here is journal proof.
func TestAutoCleanupWithdrawalAdmissionPricesRefreshedExtraProceedsFully(t *testing.T) {
	const slot = int64(58)
	const staleForecastBound = uint64(15_000_000_000) // retained doc-17 forecast, fixture-only reference
	// 18e9 receipts: above the illustrative retained bound, below the
	// observed 20e9 pool receipt supply. The observed pool redeems 2 liquidity
	// tokens per receipt (40e9 liquidity over 20e9 receipt supply), so the
	// withdrawal converts to 36e9 AUTO tokens — receipt and token units are
	// different scales and neither is 1:1.
	state := autoCleanupState{receipts: 18_000_000_000, custodyAUTO: 2_000_000_000, custodyPYUSD: 9_000_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, uint64(observation.Snapshot.PositionCollateralRaw),
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	request.ObligationReserves = []string{route.Kamino.CollateralReserve}
	effects, redeemed := autoCleanupWithdrawalEffects(t, route, accounts, state)
	if redeemed != 2*state.receipts {
		t.Fatalf("observed redemption lost the pool ratio: %d for %d receipts", redeemed, state.receipts)
	}
	returnRaw := state.custodyAUTO + redeemed
	decision := Decision{Action: DeleverRouteStep, AmountRaw: int64(state.receipts), StrategyKey: route.Lane, Reason: "withdrawal_withdraw_collateral", IdempotencyKey: "auto-cleanup-withdrawal"}
	plan, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, observation, decision, KaminoExecutionEvidence{request, effects})
	if err != nil {
		t.Fatal(err)
	}
	// The refreshed receipts deliberately exceed the retained forecast bound;
	// the priced conversion must carry custody PLUS the fully redeemed
	// withdrawal, not the bound.
	if returnRaw <= staleForecastBound {
		t.Fatalf("fixture lost its extra-proceeds shape: %d", returnRaw)
	}
	if plan.QuotedExit == nil || plan.QuotedExit.QuotedOutputRaw != returnRaw*3/2000 {
		t.Fatalf("collateral conversion did not price the full observed remainder: %+v", plan.QuotedExit)
	}
	var actions []Action
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		if step.Action == SwapCollateralToStableStep && step.Amount != returnRaw {
			t.Fatalf("collateral conversion truncated observed proceeds: %d != %d", step.Amount, returnRaw)
		}
		if step.Action == SwapDebtToUSDCStep && step.Amount != state.custodyPYUSD {
			t.Fatalf("PYUSD residue was not priced at its observed amount: %d != %d", step.Amount, state.custodyPYUSD)
		}
	}
	want := []Action{ReportNAV, SwapCollateralToStableStep, ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("cleanup exit order drifted: %v", actions)
	}
	if len(plan.AdditionalQuotedExits) != 1 || plan.AdditionalQuotedExits[0].QuotedOutputRaw != state.custodyPYUSD {
		t.Fatalf("PYUSD residue conversion missing from the quoted plan: %+v", plan.AdditionalQuotedExits)
	}
	if plan.QuotedExit.EstimatedUpperOutputRaw <= plan.QuotedExit.QuotedOutputRaw || plan.QuotedExit.ProofLevel != "UNSIGNED_PROSPECTIVE_QUOTE_COST_ESTIMATE_NOT_EXECUTION_OR_OUTPUT_GUARANTEE" {
		t.Fatalf("quoted exit lost its estimate/proof discipline: %+v", plan.QuotedExit)
	}
	if plan.ExitAfterMicros <= 0 || plan.CurrentCost.PrincipalMicros <= 0 {
		t.Fatalf("cleanup plan carried no cost discipline: %+v", plan)
	}
}

// TestAutoCleanupCollateralReturnAdmissionConvertsCustodyThenResidue proves
// the flat-position continuation on actual observed custody: the AUTO
// collateral swap is the admitted current step, the PYUSD residue conversion
// is quoted after it (collateral first, residue only once collateral is
// gone), and stage/restore follow.
//
// ATTRIBUTION ASSUMPTION (open, doc 19): residue attribution is fixture
// construction only, not journal proof.
func TestAutoCleanupCollateralReturnAdmissionConvertsCustodyThenResidue(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{receipts: 0, custodyAUTO: 2_000_000_000, custodyPYUSD: 4_500_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	decision := Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(state.custodyAUTO), StrategyKey: route.Lane, IdempotencyKey: "auto-cleanup-return"}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, decision, state.custodyAUTO, uint64(observation.Snapshot.SquadsIdleRaw), slot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, observation, decision, evidence.Request, evidence.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if plan.QuotedExit == nil || plan.QuotedExit.QuotedOutputRaw != state.custodyAUTO*3/2000 {
		t.Fatalf("current custody swap was not the priced head: %+v", plan.QuotedExit)
	}
	if len(plan.AdditionalQuotedExits) != 1 || plan.AdditionalQuotedExits[0].QuotedOutputRaw != state.custodyPYUSD {
		t.Fatalf("PYUSD residue conversion not quoted after the collateral swap: %+v", plan.AdditionalQuotedExits)
	}
	var actions []Action
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		if step.Action == SwapDebtToUSDCStep && step.Amount != state.custodyPYUSD {
			t.Fatalf("residue conversion amount drifted: %d != %d", step.Amount, state.custodyPYUSD)
		}
		// The admitted current swap is evidence, not a future template: it
		// must not be priced twice.
		if step.Action == SwapCollateralToStableStep {
			t.Fatalf("current swap duplicated as a template: %+v", plan.Exit)
		}
	}
	// The pricer records an NAV re-read after every conversion, including the
	// one whose swap template is the admitted current evidence.
	want := []Action{ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("continuation order drifted: %v", actions)
	}
}

// TestAutoCleanupDebtResidueContinuesAfterCollateralExhausted proves the
// debt-only tail: once the AUTO custody is empty, the same admission prices
// the PYUSD residue conversion alone, at exactly the observed residue.
//
// ATTRIBUTION ASSUMPTION (open, doc 19): fixture attribution only.
func TestAutoCleanupDebtResidueContinuesAfterCollateralExhausted(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{receipts: 0, custodyAUTO: 0, custodyPYUSD: 4_500_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	decision := Decision{Action: SwapDebtToUSDCStep, AmountRaw: int64(state.custodyPYUSD), StrategyKey: route.Lane, IdempotencyKey: "auto-cleanup-residue"}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, decision, state.custodyPYUSD, uint64(observation.Snapshot.SquadsIdleRaw), slot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, observation, decision, evidence.Request, evidence.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if plan.QuotedExit == nil || plan.QuotedExit.QuotedOutputRaw != state.custodyPYUSD {
		t.Fatalf("debt-only tail did not price the residue: %+v", plan.QuotedExit)
	}
	if len(plan.AdditionalQuotedExits) != 0 {
		t.Fatalf("debt-only tail invented extra conversions: %+v", plan.AdditionalQuotedExits)
	}
	var actions []Action
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
	}
	want := []Action{ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("debt-only tail order drifted: %v", actions)
	}
}

// TestAutoCleanupZeroResidueRefusesEmptyReturn proves the terminal case: a
// flat position with nothing in either custody is not a cleanup, at the
// admission gate and inside the pricer.
func TestAutoCleanupZeroResidueRefusesEmptyReturn(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	decision := Decision{Action: SwapCollateralToStableStep, StrategyKey: route.Lane, IdempotencyKey: "auto-cleanup-empty"}
	if _, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, observation, decision, JupiterSwapRequest{Action: SwapCollateralToStableStep, RouteLane: route.Lane}, ExpectedEffects{}); err == nil || !strings.Contains(err.Error(), "complete_collateral_return_admission_unavailable") {
		t.Fatalf("zero residue must refuse the return admission, got %v", err)
	}
	if _, err := pricePhase3CollateralReturn(context.Background(), rpc, client, manifest, observation, decision, JupiterSwapRequest{}, ExpectedEffects{}, 0, false, nil); err == nil || !strings.Contains(err.Error(), "empty_custody_return") {
		t.Fatalf("zero residue must hold empty_custody_return, got %v", err)
	}
}

// TestAutoCleanupStaleConfirmedSlotHoldsReturn proves the pricer's confirmed
// floor: when the confirmed slot falls behind the slot the policies were
// observed at, the return pricing fails closed.
func TestAutoCleanupStaleConfirmedSlotHoldsReturn(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{receipts: 0, custodyAUTO: 0, custodyPYUSD: 4_500_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupStaleRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	decision := Decision{Action: SwapDebtToUSDCStep, AmountRaw: int64(state.custodyPYUSD), StrategyKey: route.Lane, IdempotencyKey: "auto-cleanup-stale"}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, decision, state.custodyPYUSD, uint64(observation.Snapshot.SquadsIdleRaw), slot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, observation, decision, evidence.Request, evidence.ExpectedEffects)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale confirmed slot must hold, got plan %+v err %v", plan, err)
	}
}

// TestAutoCleanupRefusesNonterminalAndAmbiguousOperations proves the journal
// hold: an observation enriched with a nonterminal or ambiguous operation
// must stop both cleanup admissions, whatever the balances say.
func TestAutoCleanupRefusesNonterminalAndAmbiguousOperations(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{receipts: 0, custodyAUTO: 2_000_000_000, custodyPYUSD: 4_500_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	decision := Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(state.custodyAUTO), StrategyKey: route.Lane, IdempotencyKey: "auto-cleanup-nonterminal"}
	cases := []struct {
		name  string
		mutae func(*Snapshot)
	}{
		{"nonterminal operation", func(s *Snapshot) { s.Nonterminal = "multiply_operation_pending" }},
		{"ambiguous submission", func(s *Snapshot) { s.HasAmbiguousSubmission = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			poisoned := observation
			c.mutae(&poisoned.Snapshot)
			if _, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, poisoned, decision, JupiterSwapRequest{Action: SwapCollateralToStableStep, RouteLane: route.Lane, AmountRaw: state.custodyAUTO}, ExpectedEffects{}); err == nil || !strings.Contains(err.Error(), "complete_collateral_return_admission_unavailable") {
				t.Fatalf("nonterminal/ambiguous state must refuse the return, got %v", err)
			}
		})
	}
}

// TestAutoCleanupRefusesWrongLaneDecision proves lane-key discipline is
// enforced at the admission boundary: a decision keyed to another lane, or a
// withdrawal packet routed for another lane, never prices this snapshot.
// This is caller-key enforcement only — it is NOT lane attribution (doc 19).
func TestAutoCleanupRefusesWrongLaneDecision(t *testing.T) {
	const slot = int64(58)
	state := autoCleanupState{receipts: 0, custodyAUTO: 2_000_000_000, custodyPYUSD: 4_500_000}
	manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
	rpc := autoCleanupRPC(t, slot, accounts)
	client := autoCleanupClient(t, route)
	foreign := Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(state.custodyAUTO), StrategyKey: ethenaUSDePYUSD.Lane, IdempotencyKey: "auto-cleanup-wrong-lane"}
	if _, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, manifest, observation, foreign, JupiterSwapRequest{Action: SwapCollateralToStableStep, RouteLane: ethenaUSDePYUSD.Lane, AmountRaw: state.custodyAUTO}, ExpectedEffects{}); err == nil || !strings.Contains(err.Error(), "complete_collateral_return_admission_unavailable") {
		t.Fatalf("foreign lane decision must refuse, got %v", err)
	}
	// Same discipline on the withdrawal side: the request's lane must equal
	// the observed lane.
	withdrawState := autoCleanupState{receipts: 18_000_000_000, custodyAUTO: 2_000_000_000, custodyPYUSD: 9_000_000}
	_, _, withdrawObservation, _ := autoCleanupObservation(t, slot, withdrawState)
	request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, uint64(withdrawObservation.Snapshot.PositionCollateralRaw),
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	request.RouteLane = ethenaUSDePYUSD.Lane
	miskeyed := Decision{Action: DeleverRouteStep, AmountRaw: int64(withdrawState.receipts), StrategyKey: route.Lane, Reason: "withdrawal_withdraw_collateral", IdempotencyKey: "auto-cleanup-wrong-lane-withdrawal"}
	if _, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, manifest, withdrawObservation, miskeyed, KaminoExecutionEvidence{request, ExpectedEffects{}}); err == nil || !strings.Contains(err.Error(), "complete_position_exit_admission_unavailable") {
		t.Fatalf("foreign lane withdrawal packet must refuse, got %v", err)
	}
}

// TestAutoCleanupDecisionProgressionFollowsPostPayoffLadder drives the
// EXISTING non-USDC decide entrypoint (decideNonUSDC — the same decision
// logic the fixed lanes resolve through; the production Decide wrapper still
// fails AUTO closed because the candidate lane is not a registered decision
// lane) over actual observed post-payoff snapshots, and hands each selected
// decision straight to the admission that must price it. The ladder order is
// the existing return path: withdraw remaining receipts, convert custody,
// convert residue, stage, then hold with flat custody. The drain posture is
// a state bit, not a balance, so the fixture sets CutoverDrain.
//
// ATTRIBUTION ASSUMPTION (open, doc 19): the PYUSD residue in the shared
// debt custody is treated as this lane's by fixture construction only —
// decideNonUSDC reads balances plus the snapshot's lane keys; no journal
// evidence distinguishes which lane left shared-custody residue, and this
// test does not claim otherwise.
func TestAutoCleanupDecisionProgressionFollowsPostPayoffLadder(t *testing.T) {
	const slot = int64(58)
	assertDecision := func(t *testing.T, d Decision, action Action, reason string, amount int64) {
		t.Helper()
		if d.Action != action || d.Reason != reason || d.AmountRaw != amount || d.StrategyKey != autoAUTOPYUSD.Lane {
			t.Fatalf("decision ladder drifted: want %s/%s/%d, got %s/%s/%d (%s)", action, reason, amount, d.Action, d.Reason, d.AmountRaw, d.StrategyKey)
		}
	}
	t.Run("debt_free remaining receipts withdraw first", func(t *testing.T) {
		state := autoCleanupState{receipts: 18_000_000_000, custodyAUTO: 2_000_000_000, custodyPYUSD: 9_000_000}
		manifest, route, observation, accounts := autoCleanupObservation(t, slot, state)
		observation.Snapshot.CutoverDrain = true
		decision := decideNonUSDC(observation.Snapshot, initializationSnapshotReady)
		assertDecision(t, decision, DeleverRouteStep, "withdrawal_withdraw_collateral", 0)
		// The decide-selected decision must be admissible as-is: the existing
		// withdrawal admission prices the full observed remainder.
		request, err := manifest.kaminoPacketForRoute(decision.Action, kaminoLegWithdraw, uint64(observation.Snapshot.PositionCollateralRaw),
			LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		request.ObligationReserves = []string{route.Kamino.CollateralReserve}
		effects, redeemed := autoCleanupWithdrawalEffects(t, route, accounts, state)
		plan, err := observePhase3WithdrawalAdmission(context.Background(), autoCleanupRPC(t, slot, accounts), autoCleanupClient(t, route),
			manifest, observation, decision, KaminoExecutionEvidence{request, effects})
		if err != nil {
			t.Fatal(err)
		}
		if plan.QuotedExit == nil || plan.QuotedExit.QuotedOutputRaw != (state.custodyAUTO+redeemed)*3/2000 {
			t.Fatalf("decide->admission handoff lost the full observed remainder: %+v", plan.QuotedExit)
		}
	})
	t.Run("custody converts before residue", func(t *testing.T) {
		state := autoCleanupState{custodyAUTO: 2_000_000_000, custodyPYUSD: 4_500_000}
		_, _, observation, _ := autoCleanupObservation(t, slot, state)
		observation.Snapshot.CutoverDrain = true
		decision := decideNonUSDC(observation.Snapshot, initializationSnapshotReady)
		assertDecision(t, decision, SwapCollateralToStableStep, "withdrawal_convert_collateral_to_usdc", int64(state.custodyAUTO))
	})
	t.Run("residue converts once custody is gone", func(t *testing.T) {
		state := autoCleanupState{custodyPYUSD: 4_500_000}
		_, _, observation, _ := autoCleanupObservation(t, slot, state)
		observation.Snapshot.CutoverDrain = true
		assertDecision(t, decideNonUSDC(observation.Snapshot, initializationSnapshotReady), SwapDebtToUSDCStep, "withdrawal_convert_debt_residue_to_usdc", int64(state.custodyPYUSD))
	})
	t.Run("terminal cash stages", func(t *testing.T) {
		_, _, observation, _ := autoCleanupObservation(t, slot, autoCleanupState{})
		observation.Snapshot.CutoverDrain = true
		assertDecision(t, decideNonUSDC(observation.Snapshot, initializationSnapshotReady), StageSquadsToVoltr, "withdrawal_terminal_residue", observation.Snapshot.SquadsIdleRaw)
	})
	t.Run("flat custody holds not trades", func(t *testing.T) {
		_, _, observation, _ := autoCleanupObservation(t, slot, autoCleanupState{zeroSquads: true})
		observation.Snapshot.CutoverDrain = true
		decision := decideNonUSDC(observation.Snapshot, initializationSnapshotReady)
		// A fully flat lane owes its terminal NAV report and must not
		// construct an entry, swap, or withdrawal.
		assertDecision(t, decision, ReportNAV, "withdrawal_terminal_nav_due", 0)
	})
}
