package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func debtClearPayoffFixture(t *testing.T) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, phase3BridgeAdmission) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixture(t)
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	decision := Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, AmountRaw: o.Snapshot.PositionDebtRaw, Reason: "withdrawal_repay_debt"}
	request, err := m.kaminoPacketForRoute(decision.Action, kaminoLegRepay, 1001, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	request.FullPayoff = true
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	evidence := KaminoExecutionEvidence{request, effects}
	plan, err := observePhase3PayoffAdmission(context.Background(), rpc, client, m, o, decision, evidence)
	if err != nil {
		t.Fatal(err)
	}
	return o, decision, evidence, m, rpc, plan
}

func TestDebtClearClassifiesWholeFlowBeforeFirstCapitalLeg(t *testing.T) {
	_, _, evidence, m, _, plan := debtClearPayoffFixture(t)
	evidence.Request.FullPayoff = false // The actual repay amount, not this flag, controls consent.
	required, err := debtClearRequired(m, evidence.Request, evidence.ExpectedEffects, &plan, debtClearRouteState{})
	if err != nil || !required {
		t.Fatal("whole-debt repayment escaped confirmation", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"economic", func(s *Snapshot) { s.Unwind = true }},
		{"one_x", func(s *Snapshot) { s.LeverageTargetLevel = 1 }},
		{"withdrawal_fallback", func(s *Snapshot) { s.WithdrawalDemandRaw = 1_600_000_000 }},
		{"cutover", func(s *Snapshot) { s.CutoverDrain = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := livePartialSnapshot()
			change.mutate(&s)
			decision := Decide(s)
			p := phase3BridgeAdmission{Snapshot: s, Decision: decision}
			// Releasing collateral/funding, before any repayment, is capital work.
			required, err := debtClearRequired(m, JupiterSwapRequest{Action: SwapCollateralToDebtStep}, ExpectedEffects{}, &p, debtClearRouteState{})
			if err != nil || !required {
				t.Fatal("first full-exit capital leg escaped confirmation", err)
			}
		})
	}
	s := livePartialSnapshot()
	p := phase3BridgeAdmission{Snapshot: s, Decision: Decide(s)}
	required, err = debtClearRequired(m, JupiterSwapRequest{Action: SwapCollateralToDebtStep}, ExpectedEffects{}, &p, debtClearRouteState{})
	if err != nil || required {
		t.Fatal("genuine debt-retaining partial release blocked", err)
	}
	// Utilization alone never turns a partial move into a full-clear emergency.
	s.BorrowUtilizationBlocked = true
	p.Snapshot = s
	required, err = debtClearRequired(m, JupiterSwapRequest{Action: SwapCollateralToDebtStep}, ExpectedEffects{}, &p, debtClearRouteState{})
	if err != nil || required {
		t.Fatal("utilization changed partial classification", err)
	}
}

func TestDebtClearHighRiskReportingNeedsNoAuthority(t *testing.T) {
	s := livePartialSnapshot()
	s.LTVBPS = 6000
	s.Unwind = true
	o := Observation{Snapshot: s, ObservedAt: time.Now().UTC()}
	decision := Decision{Action: ReportNAV, StrategyKey: s.RouteLane}
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if proof, err := verifyDebtClearEmergency(m, o, decision, "report", time.Now().UTC()); err != nil || proof != nil {
		t.Fatal("report requested emergency proof", err)
	}
	if required, err := debtClearRequired(m, bridgeTestRequest(ReportNAV, 0), ExpectedEffects{}, nil, debtClearRouteState{}); err != nil || required {
		t.Fatal("report requested consent", err)
	}
}

func prepareDebtClearDatabase(t *testing.T) (context.Context, *Database, string) {
	t.Helper()
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(db.Close)
	_, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
 ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
 ADD COLUMN IF NOT EXISTS recent_blockhash text,
 ADD COLUMN IF NOT EXISTS last_valid_block_height bigint,
 ADD COLUMN IF NOT EXISTS confirmation_status text,
 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,
 ADD COLUMN IF NOT EXISTS reconciled_effects jsonb;
 CREATE UNIQUE INDEX IF NOT EXISTS multiply_operations_one_nonterminal_per_route ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling');`)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("debt-clear-%d", time.Now().UnixNano())
	b := emptyTestBudget()
	b.Families["Maple"] = FamilyBudget{ExitMicros: 4_000_000}
	state, _ := json.Marshal(map[string]any{"generation": 1, "phase3": b})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "debt-clear-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	return ctx, db, key
}

func insertDebtClearOperation(t *testing.T, ctx context.Context, db *Database, key, id string, o Observation, decision Decision, m RouteManifest) {
	t.Helper()
	envelope, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, decision, m.SHA256, *m.PolicyCatalog.SHA256)})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, decision.Action, decision.StrategyKey, envelope); err != nil {
		t.Fatal(err)
	}
}

func TestDebtClearDatabaseAdmissionConfirmationAndReplay(t *testing.T) {
	ctx, db, key := prepareDebtClearDatabase(t)
	o, decision, e, m, rpc, plan := debtClearPayoffFixture(t)
	id := key + "-ordinary"
	insertDebtClearOperation(t, ctx, db, key, id, o, decision, m)
	assertBudgetHold(t, db.persistPhase3ExitAdmissionOnManifest(ctx, rpc, m, id, o, decision, plan), "debt_clear_confirmation_required")
	var admitted, wire, broadcast bool
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects ? 'phase3',signed_wire IS NOT NULL,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&admitted, &wire, &broadcast); err != nil {
		t.Fatal(err)
	}
	if admitted || wire || broadcast {
		t.Fatal("unapproved capital admission mutated authority or wire")
	}
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	confirmation := DebtClearConfirmation{RequestID: strings.Repeat("c", 64), ConfirmedBy: "privileged-test-operator", ConfirmationRecord: strings.Repeat("d", 64), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	intent := UnwindIntent{SourceLane: o.Snapshot.RouteLane, Reason: "economic_rotation", ObservationID: o.Snapshot.ObservationID, MaxCollateralRaw: o.Snapshot.PositionCollateralRaw, MaxDebtRaw: 2000, CostBoundRaw: 4_000_000, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: strings.Repeat("e", 64), CreatedAt: time.Now().UTC()}
	if err := db.commitUnwindIntentWithConfirmation(ctx, key, &intent, m, confirmation); err != nil {
		t.Fatal(err)
	}
	retry := intent
	retry.CreatedAt = time.Now().UTC()
	if err := db.commitUnwindIntentWithConfirmation(ctx, key, &retry, m, confirmation); err != nil {
		t.Fatal("same explicit confirmation not idempotent", err)
	}
	changed := intent
	changed.MaxDebtRaw++
	assertBudgetHold(t, db.commitUnwindIntentWithConfirmation(ctx, key, &changed, m, confirmation), "debt_clear_confirmation_reused")
	id = key + "-approved"
	insertDebtClearOperation(t, ctx, db, key, id, o, decision, m)
	if err := db.persistPhase3ExitAdmissionOnManifest(ctx, rpc, m, id, o, decision, plan); err != nil {
		t.Fatal(err)
	}
	// Successful authorization reaches the existing signer boundary; this test
	// deliberately never supplies a signer and never creates a signed wire.
	effects, _ := jsonMarshalExpectedEffects(e.ExpectedEffects)
	if err := db.authorizePhase3BuildOnManifest(ctx, m, rpc, id, e.Request, effects, plan.CurrentCost); err != nil {
		t.Fatal("approved pre-sign authorization refused", err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=state-'debtClearAuthority' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.authorizePhase3BuildOnManifest(ctx, m, rpc, id, e.Request, effects, plan.CurrentCost), "debt_clear_confirmation_required")
	if err := db.pool.QueryRow(ctx, `SELECT signed_wire IS NOT NULL,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&wire, &broadcast); err != nil {
		t.Fatal(err)
	}
	if wire || broadcast {
		t.Fatal("pre-sign denial wrote a signed or broadcast operation")
	}
	// Receipt survives completed/superseded authority and cannot be reactivated.
	if err := db.MarkPreBroadcastFailed(ctx, id, Decided, "fixture_confirmation_retired"); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.commitUnwindIntentWithConfirmation(ctx, key, &intent, m, confirmation), "debt_clear_confirmation_reused")
}

func TestDebtClearOldSignedDenialRetiresOnlyExpiredAbsent(t *testing.T) {
	for _, found := range []bool{false, true} {
		t.Run(fmt.Sprint(found), func(t *testing.T) {
			ctx, db, key := prepareDebtClearDatabase(t)
			o, decision, e, m, _, plan := debtClearPayoffFixture(t)
			id := key + "-old-signed"
			insertDebtClearOperation(t, ctx, db, key, id, o, decision, m)
			input := plan.Input
			digest, _ := Phase3IntentDigest(e.Request, input.Effects)
			message, err := m.compileKaminoMessage(e.Request, mustKey(bridgeDelegate))
			if err != nil {
				t.Fatal(err)
			}
			wire := append(make([]byte, 65), message...)
			wire[0] = 1
			auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, BuildInput: input, BridgeAdmission: &plan, SignedWireSHA256: sha256Bytes(wire)}
			encoded, _ := json.Marshal(auth)
			b := emptyTestBudget()
			b.Families["Maple"] = FamilyBudget{ExitMicros: 4_000_000}
			if err = b.Admit(BudgetReservation{OperationID: id, Family: "Maple", IntentSHA256: digest, UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: true}); err != nil {
				t.Fatal(err)
			}
			budget, _ := json.Marshal(b)
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3}',$2::jsonb) WHERE route_key=$1`, key, budget); err != nil {
				t.Fatal(err)
			}
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,expected_effects=jsonb_set(expected_effects,'{phase3}',$3::jsonb) WHERE operation_id=$1`, id, wire, encoded); err != nil {
				t.Fatal(err)
			}
			operation := PersistedOperation{Operation: Operation{ID: id, RouteKey: key, Decision: decision}, Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
			height := int64(99)
			sends, absenceReads := 0, 0
			rpc, _ := NewRPCClient("https://rpc.invalid")
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var call struct{ Method string }
				if json.Unmarshal(body, &call) != nil {
					t.Fatal("RPC request")
				}
				switch call.Method {
				case "getBlockHeight":
					return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d}`, height)), nil
				case "getSignatureStatuses":
					absenceReads++
					if found {
						return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":42,"confirmations":null,"err":null,"confirmationStatus":"finalized"}]}}`), nil
					}
					return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[null]}}`), nil
				case "sendTransaction":
					sends++
				}
				return nil, fmt.Errorf("unexpected RPC during consent hold: %s", call.Method)
			})
			assertBudgetHold(t, advanceNonterminalWithManifest(ctx, m, db, rpc, operation), "debt_clear_operation_not_authorized")
			var status string
			var sent bool
			if err = db.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &sent); err != nil {
				t.Fatal(err)
			}
			if status != "signed" || sent || sends != 0 || absenceReads != 0 {
				t.Fatal("valid signed wire was sent or retired")
			}
			height = 100
			if err = advanceNonterminalWithManifest(ctx, m, db, rpc, operation); err != nil {
				t.Fatal(err)
			}
			if err = db.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &sent); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if found {
				want = "manual_recovery"
			}
			if status != want || sent || sends != 0 || absenceReads != 1 {
				t.Fatalf("unsafe expiry result %s sends=%d absence=%d", status, sends, absenceReads)
			}
		})
	}
}

// This fixture uses account bytes and the normal health/debt/LTV decoders,
// not a Snapshot.LTVBPS assertion masquerading as emergency authority.
func TestDebtClearRiskRequiresCoherentFreshAccounts(t *testing.T) {
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot-only claims are not risk evidence, even at the exact threshold.
	s := livePartialSnapshot()
	s.LTVBPS = 6000
	s.UnwindRefreshRequired = true
	o := Observation{Snapshot: s, ObservedAt: time.Now().UTC()}
	if _, err = verifyDebtClearEmergency(m, o, Decide(s), "risk", time.Now().UTC()); err == nil {
		t.Fatal("unverified snapshot authorized emergency")
	}
	// Genuine risk still preempts an expired/out-of-envelope ordinary unwind.
	s.SquadsIdleRaw = 0
	s.DebtIdleRaw = s.PayoffDebtRaw
	s.Unwind = true
	if decision := Decide(s); decision.Action == Hold || decision.Reason == "unwind_requires_fresh_admission" {
		t.Fatal("ordinary renewal preempted risk", decision)
	}
	s.LTVBPS = 3331
	if decision := Decide(s); decision.Action != Hold || decision.Reason != "unwind_requires_fresh_admission" {
		t.Fatal("ordinary oversized envelope continued", decision)
	}
}

func debtClearRiskFixture(t *testing.T) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, phase3BridgeAdmission) {
	t.Helper()
	o, m, _, _, accounts := usdcReturnFixture(t)
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	collateralAccount := accountAt(accounts, route.Kamino.CollateralReserve)
	collateralAccount.Data[kaminoLoanToValueOffset] = 80
	collateralAccount.Data[kaminoLoanToValueOffset+1] = 90
	collateral, err := decodeKaminoReserve(collateralAccount, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := collateral.redeemLiquidityRaw(uint64(o.Snapshot.PositionCollateralRaw))
	if err != nil {
		t.Fatal(err)
	}
	// Derive the price from actual debt price/decimal evidence: this fixture
	// retains the saved reserve's $2 debt price, even on the USDC test lane.
	debtBefore, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	numerator := new(big.Int).Mul(littleInt(debtBefore.marketPriceSF[:]), big.NewInt(o.Snapshot.PositionDebtRaw*3))
	numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(collateral.mintDecimals)), nil))
	denominator := new(big.Int).Mul(new(big.Int).SetUint64(redeemed*2), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(debtBefore.mintDecimals)), nil))
	price := new(big.Int).Quo(numerator, denominator)
	putScaledFraction(collateralAccount.Data[248:264], price)
	for _, address := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[264:272], 1000)
	}
	for _, lane := range selectorObservationLanes(m) {
		other, _ := runtimeRoute(lane)
		if other.Kamino.Obligation != route.Kamino.Obligation {
			accounts = append(accounts, ConfirmedAccount{Address: other.Kamino.Obligation})
			custody := tokenAccountFixture(t, other.CollateralCustody, other.Kamino.CollateralMint, bridgeVault, 0)
			custody.Owner = other.CollateralTokenProgram
			accounts = append(accounts, custody)
		}
	}
	collateral, err = decodeKaminoReserve(collateralAccount, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	rawDebt, err := obligation.debtAtReserveRate(debt)
	if err != nil {
		t.Fatal(err)
	}
	ltv, err := observedLTVBPS(KaminoPosition{DebtRaw: rawDebt, RedeemablePrimeRaw: redeemed, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF})
	if err != nil {
		t.Fatal(err)
	}
	o.Snapshot.PilotActive = true
	o.Snapshot.LTVBPS, o.Snapshot.LiquidationThresholdBPS = ltv, 9000
	o.Snapshot.PositionCollateralValueRaw = 1500
	o.Snapshot.Unwind, o.Snapshot.UnwindRefreshRequired = true, true
	o.ObservedAt = time.Now().UTC()
	o.ValuationSource, o.ValuationSlot = "confirmed", o.Snapshot.Slot
	o.Snapshot.ValuationSource, o.Snapshot.ValuationSlot = o.ValuationSource, o.ValuationSlot
	o.routeBatch = &routeObservationBatch{Slot: o.Snapshot.Slot, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
	decision := Decide(o.Snapshot)
	if decision.Reason != "hard_ltv_repay" {
		t.Fatal("fixture not real hard risk", decision)
	}
	request, err := m.kaminoPacketForRoute(decision.Action, kaminoLegRepay, 1001, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	request.FullPayoff = true
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonMarshalExpectedEffects(effects)
	input, _ := encodePhase3BuildInput(request, encoded)
	payoff, err := decodeKaminoPayoffWindow(accounts, route, o.Snapshot.Slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	plan := phase3BridgeAdmission{Payoff: &payoff, Snapshot: o.Snapshot, Decision: decision, Input: input, CurrentCost: ValuedTransactionCost{TotalMicros: 10, ObservationSlot: 42, ValidThroughSlot: 74}, ExitAfterMicros: 90, ValidThroughSlot: 74}
	return o, decision, KaminoExecutionEvidence{request, effects}, m, plan
}

func TestDebtClearVerifiedRiskAndStaleEvidence(t *testing.T) {
	for _, variant := range []string{"confirmed", "refresh", "stale_time", "stale_reserve", "stale_oracle", "missing_batch", "snapshot_only", "bad_provenance"} {
		t.Run(variant, func(t *testing.T) {
			o, d, _, m, _ := debtClearRiskFixture(t)
			route, _ := runtimeRoute(o.Snapshot.RouteLane)
			switch variant {
			case "refresh":
				o.ValuationSource, o.Snapshot.ValuationSource = routeRefreshValuationSource, routeRefreshValuationSource
				for i := range o.routeBatch.Accounts {
					o.routeBatch.Accounts[i].ValuationSource = routeRefreshValuationSource
					o.routeBatch.Accounts[i].ValuationSlot = o.Snapshot.Slot
				}
			case "stale_time":
				o.ObservedAt = time.Now().UTC().Add(-time.Minute)
			case "stale_reserve":
				binary.LittleEndian.PutUint64(accountAt(o.routeBatch.Accounts, route.Kamino.CollateralReserve).Data[16:24], 1)
			case "stale_oracle":
				binary.LittleEndian.PutUint64(accountAt(o.routeBatch.Accounts, route.Kamino.DebtReserve).Data[264:272], 1)
			case "missing_batch":
				o.routeBatch = nil
			case "snapshot_only":
				o.Snapshot.LTVBPS = 6000
			case "bad_provenance":
				o.ValuationSource = "unverified"
			}
			proof, err := verifyDebtClearEmergency(m, o, d, "risk-origin", time.Now().UTC())
			valid := variant == "confirmed" || variant == "refresh"
			if valid && (err != nil || proof == nil) {
				t.Fatal("fresh account-verified risk rejected", err)
			}
			if !valid && (err == nil || proof != nil) {
				t.Fatal("unverified risk obtained authority", err)
			}
		})
	}
}

func TestDebtClearEmergencySupersedesOldBoundsAndRequiresReconciledOrigin(t *testing.T) {
	ctx, db, key := prepareDebtClearDatabase(t)
	o, decision, e, m, plan := debtClearRiskFixture(t)
	old := UnwindIntent{SourceLane: o.Snapshot.RouteLane, Reason: "economic_rotation", ObservationID: "old-ordinary", MaxCollateralRaw: o.Snapshot.PositionCollateralRaw - 1, MaxDebtRaw: 500, CostBoundRaw: 4_000_000, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}
	c := DebtClearConfirmation{RequestID: strings.Repeat("b", 64), ConfirmedBy: "test-operator", ConfirmationRecord: strings.Repeat("c", 64), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := db.commitUnwindIntentWithConfirmation(ctx, key, &old, m, c); err != nil {
		t.Fatal(err)
	}
	if err := applyUnwindIntentWithLane(&o.Snapshot, &old, m.selectorEntryLaneAllowed); err != nil {
		t.Fatal("amount growth became global integrity failure", err)
	}
	if !o.Snapshot.UnwindRefreshRequired || Decide(o.Snapshot).Reason != "hard_ltv_repay" {
		t.Fatal("ordinary amount envelope blocked fresh risk")
	}
	below := o.Snapshot
	below.LTVBPS = 3333
	if d := Decide(below); d.Action != Hold || d.Reason != "unwind_requires_fresh_admission" {
		t.Fatal("ordinary amount expansion did not hold", d)
	}
	plan.Snapshot = o.Snapshot
	id := key + "-risk"
	insertDebtClearOperation(t, ctx, db, key, id, o, decision, m)
	risk, err := verifyDebtClearEmergency(m, o, decision, id, time.Now().UTC())
	if err != nil || risk == nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	budget, auth, err := db.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.authorizeDebtClearTx(ctx, tx, m, id, e.Request, e.ExpectedEffects, &plan, &auth, risk, 42, true); err != nil {
		t.Fatal("old ordinary bounds blocked risk", err)
	}
	auth.GoalID = Phase3GoalID
	auth.BridgeAdmission = &plan
	auth.BuildInput = plan.Input
	auth.IntentSHA256, _ = Phase3IntentDigest(e.Request, plan.Input.Effects)
	if err = db.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A failed/never-submitted origin is not permission for a lower-risk exit.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	next := id + "-continuation"
	continuation := plan
	continuation.Snapshot.LTVBPS = 3333
	continuation.Snapshot.UnwindRefreshRequired = false
	continuation.CurrentCost.TotalMicros = 10
	continuation.ExitAfterMicros = 80
	insertDebtClearOperation(t, ctx, db, key, next, o, decision, m)
	var continuationRequest any = e.Request
	check := func(commit bool) error {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err = db.lockOperationLease(ctx, tx, next); err != nil {
			return err
		}
		var nextAuth phase3OperationAuthorization
		err = db.authorizeDebtClearTx(ctx, tx, m, next, continuationRequest, e.ExpectedEffects, &continuation, &nextAuth, nil, 42, true)
		if err != nil {
			return err
		}
		if commit {
			return tx.Commit(ctx)
		}
		return nil
	}
	assertBudgetHold(t, check(false), "debt_clear_emergency_origin_not_reconciled")
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',confirmation_status='finalized',reconciled_effects='{}' WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = check(true); err != nil {
		t.Fatal("reconciled risk continuation required ordinary consent", err)
	}
	continuation.Snapshot.PositionDebtRaw, continuation.Snapshot.PayoffDebtRaw = 0, 0
	continuation.Payoff = nil
	continuation.Decision.Action = StageSquadsToVoltr
	continuation.ExitAfterMicros = 70
	continuationRequest = bridgeTestRequest(StageSquadsToVoltr, 1000)
	if err = check(false); err != nil {
		t.Fatal("reconciled emergency debt-free tail blocked", err)
	}
	continuation.Snapshot.PositionDebtRaw = 2001
	assertBudgetHold(t, check(false), "debt_clear_scope_changed")
	// Finishing the same flow retains origin/ordinary receipts but retires authority.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id=$1`, next); err != nil {
		t.Fatal(err)
	}
	unwind, err := db.LoadUnwindIntentOnManifest(ctx, m, key)
	if err != nil || unwind == nil {
		t.Fatal(err)
	}
	flat := Snapshot{Fresh: true, RouteLane: o.Snapshot.RouteLane}
	if err = db.CompleteUnwindIntentOnManifest(ctx, m, key, *unwind, flat); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err = db.pool.QueryRow(ctx, `SELECT state->'debtClearReceipts' ? $2 AND state->'debtClearAuthority'='null'::jsonb FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key, c.RequestID).Scan(&retained); err != nil || !retained {
		t.Fatal("completion recycled authority", err)
	}
	continuation.Snapshot.PositionDebtRaw = 1000
	assertBudgetHold(t, check(false), "debt_clear_confirmation_required")
}

func TestDebtClearActiveDebtFreeTailKeepsExpiryScopeAndCostFence(t *testing.T) {
	for _, kind := range []string{"withdraw", "swap", "stage"} {
		for _, condition := range []string{"valid", "expired", "revoked", "cost_exceeded"} {
			t.Run(kind+"/"+condition, func(t *testing.T) {
				ctx, db, key := prepareDebtClearDatabase(t)
				o, _, _, m, _, plan := debtClearPayoffFixture(t)
				origin := UnwindIntent{SourceLane: o.Snapshot.RouteLane, Reason: "economic_rotation", ObservationID: o.Snapshot.ObservationID, MaxCollateralRaw: o.Snapshot.PositionCollateralRaw, MaxDebtRaw: 2000, CostBoundRaw: 100, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: strings.Repeat("d", 64), CreatedAt: time.Now().UTC()}
				c := DebtClearConfirmation{RequestID: strings.Repeat("f", 64), ConfirmedBy: "test-operator", ConfirmationRecord: strings.Repeat("e", 64), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(time.Minute)}
				if err := db.commitUnwindIntentWithConfirmation(ctx, key, &origin, m, c); err != nil {
					t.Fatal(err)
				}
				plan.Snapshot.PositionDebtRaw, plan.Snapshot.PayoffDebtRaw = 0, 0
				plan.Payoff = nil
				plan.CurrentCost.TotalMicros, plan.ExitAfterMicros = 10, 20
				plan.Decision = Decision{Action: StageSquadsToVoltr, StrategyKey: o.Snapshot.RouteLane, Reason: "withdrawal_stage", AmountRaw: 1000}
				var request any = bridgeTestRequest(StageSquadsToVoltr, 1000)
				if kind == "swap" {
					request = JupiterSwapRequest{Action: SwapCollateralToStableStep}
					plan.Decision.Action = SwapCollateralToStableStep
				}
				if kind == "withdraw" {
					r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, 1000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, o.Snapshot.RouteLane)
					if err != nil {
						t.Fatal(err)
					}
					request = r
					plan.Decision.Action = DeleverRouteStep
				}
				id := key + "-tail"
				insertDebtClearOperation(t, ctx, db, key, id, o, plan.Decision, m)
				tx, err := db.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				budget, auth, err := db.readPhase3BudgetTx(ctx, tx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err = db.authorizeDebtClearTx(ctx, tx, m, id, request, ExpectedEffects{}, &plan, &auth, nil, 42, true); err != nil {
					t.Fatal("valid tail refused", err)
				}
				auth.GoalID = Phase3GoalID
				auth.BridgeAdmission = &plan
				if err = db.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				a := *auth.DebtClear
				switch condition {
				case "expired":
					expired := *a.Confirmation
					expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
					a.Confirmation = &expired
				case "cost_exceeded":
					a.UsedCostMicros = a.Origin.CostBoundRaw + 1
				}
				encoded, _ := json.Marshal(a)
				if condition == "revoked" {
					_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=state-'debtClearAuthority'-'selectorUnwind' WHERE route_key=$1`, key)
				} else {
					_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{debtClearAuthority}',$2::jsonb,true) WHERE route_key=$1`, key, encoded)
				}
				if err != nil {
					t.Fatal(err)
				}
				tx, err = db.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err = db.lockOperationLease(ctx, tx, id); err != nil {
					t.Fatal(err)
				}
				gateErr := db.authorizeDebtClearTx(ctx, tx, m, id, request, ExpectedEffects{}, &plan, &auth, nil, 42, false)
				tx.Rollback(ctx)
				signedErr := db.checkSignedDebtClearConsent(ctx, m, PersistedOperation{Operation: Operation{ID: id, RouteKey: key}}, auth, request, ExpectedEffects{})
				if condition == "valid" {
					if gateErr != nil || signedErr != nil {
						t.Fatal("valid tail lost authority", gateErr, signedErr)
					}
				} else if gateErr == nil || signedErr == nil {
					t.Fatal("debt-free tail lost its consent/cost fence", gateErr, signedErr)
				}
				var wire, sent bool
				if err = db.pool.QueryRow(ctx, `SELECT signed_wire IS NOT NULL,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&wire, &sent); err != nil {
					t.Fatal(err)
				}
				if wire || sent {
					t.Fatal("tail authorization test crossed sign/send boundary")
				}
			})
		}
	}
}
