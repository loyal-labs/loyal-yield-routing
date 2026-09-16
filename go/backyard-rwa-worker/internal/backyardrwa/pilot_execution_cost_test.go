package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func pilotCostFixture(t *testing.T, request any, effects ExpectedEffects) ValuedTransactionCost {
	t.Helper()
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	_, _, message, err := input.decode()
	if err != nil {
		t.Fatal(err)
	}
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	decimals := uint8(6)
	token := budgetTestPrice(debit.Mint, debit.TokenProgram, decimals, 1, 1)
	sol := budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1)
	var rent uint64
	if r, ok := request.(KaminoInitializationRequest); ok {
		rent = r.RentLamports
	}
	cost, err := ValueTransactionCost(message, debit, MessageFeeObservation{MessageSHA256: sha256Bytes(message), Slot: 42, Lamports: 5000}, rent, token, sol, 42)
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

func TestPilotExecutionCostPreservesPrincipalAndBindsExactValuation(t *testing.T) {
	_, _, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 500_000, 500_000, 0, 0)
	request, effects := evidence.Request, evidence.ExpectedEffects
	cost := pilotCostFixture(t, request, effects)
	bound, err := classifyPilotExecutionCost(request, effects, cost, nil)
	if err != nil || bound.TotalMicros != 500 || cost.PrincipalMicros != 500_000 {
		t.Fatalf("principal charged as expense: %+v %v", bound, err)
	}
	cost.ExecutionCost = &bound
	b := pilotTestBudget(t)
	r := BudgetReservation{ExecutionCostUpperMicros: 500}
	if err = validateReservedExecutionCost(b, r, request, effects, cost); err != nil {
		t.Fatal(err)
	}
	r.ExecutionCostUpperMicros--
	assertBudgetHold(t, validateReservedExecutionCost(b, r, request, effects, cost), "fresh_execution_cost_exceeds_reservation")
	r.ExecutionCostUpperMicros++
	cost.ExecutionCost.TotalMicros--
	assertBudgetHold(t, validateReservedExecutionCost(b, r, request, effects, cost), "execution_cost_classification_mismatch")
	cost.ExecutionCost.TotalMicros++
	cost.PrincipalMicros--
	cost.TotalMicros--
	_, err = classifyPilotExecutionCost(request, effects, cost, nil)
	assertBudgetHold(t, err, "execution_cost_valuation_mismatch")
}

func TestPilotSwapCostUsesMinimumOutputAndLowerPrice(t *testing.T) {
	_, _, e, _, _, _, _ := entrySwapAdmissionFixture(t)
	cost := pilotCostFixture(t, e.Request, e.ExpectedEffects)
	dest := e.ExpectedEffects.Accounts[1]
	credit := budgetTestPrice(dest.Mint, dest.Owner, 6, 2, 1)
	credit.Credit = &BudgetCreditBounds{TokenLowerSF: budgetTestPrice("", "", 6, 1, 1).TokenUpperSF, USDCUpperSF: credit.USDCLowerSF}
	bound, err := classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	want := max(0, cost.PrincipalMicros-int64(e.Request.MinimumOutputRaw))
	if err != nil || bound.SwapLossMicros != want || bound.CreditPrice == nil {
		t.Fatalf("did not use floor credit: %+v want %d: %v", bound, want, err)
	}
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, nil)
	assertBudgetHold(t, err, "missing_swap_execution_cost_bound")
	credit.Credit = nil
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	assertBudgetHold(t, err, "missing_credit_valuation_bounds")
	credit.Credit = &BudgetCreditBounds{TokenLowerSF: credit.TokenUpperSF, USDCUpperSF: credit.USDCLowerSF}
	credit.ValidThroughSlot = 41
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	assertBudgetHold(t, err, "missing_stale_or_mismatched_usdc_valuation")
}

func TestPilotNativeInitializationBooksFeeAndRetainsRentAsAsset(t *testing.T) {
	effects, _ := initializationReconcileFixture(t)
	request := *effects.Initialization
	cost := pilotCostFixture(t, request, effects)
	bound, err := classifyPilotExecutionCost(request, effects, cost, nil)
	if err != nil || bound.TotalMicros != cost.NetworkFeeMicros || cost.SetupLamportsMicros <= bound.TotalMicros {
		t.Fatalf("rent was lost or counted as execution expense: %+v %v", bound, err)
	}
}

func TestPilotSignedRepricingProducesBoundWithoutWireReplacement(t *testing.T) {
	auth, op := signedBudgetFixture(t)
	auth.PilotAuthorityID = pilotBudgetAuthorityID
	hash := sha256Bytes(op.SignedWire)
	cost, err := revaluePhase3SignedInput(context.Background(), budgetBuildRPC(t, 5000, 42), auth, op)
	if err != nil || cost.ExecutionCost == nil || cost.ExecutionCost.TotalMicros != cost.NetworkFeeMicros || sha256Bytes(op.SignedWire) != hash {
		t.Fatalf("pilot send cost not independently observed: %+v %v", cost, err)
	}
}

func TestPilotMeasuredAdmissionPersistsCostAndRejectsChangedBuildAndSend(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("pilot-cost-%d", time.Now().UnixNano())
	id := key + "-op"
	prior := emptyTestBudget()
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	previous, _ := json.Marshal(prior)
	authority := pilotTestAuthority(prior)
	authority.Generation = 2
	authority.FinalizedSlot = flat.Slot
	authority.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	budget, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"generation": 2, "selectorEntry": selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 10_000_000), "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{authority, previous, flat}})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	o, d, e := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 10_000_000, 20_000_000, 0, 0)
	o.Snapshot.RouteLane = SelectedRouteID
	o.Snapshot.StrategyKey = SelectedRouteID
	d.StrategyKey = SelectedRouteID
	encoded, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, sha256Bytes([]byte("manifest")), sha256Bytes([]byte("policies")))})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, d.Action, d.StrategyKey, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "pilot-cost-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	rpc := budgetBuildRPC(t, 5000, 42)
	plan, err := observePhase3BridgeAdmission(ctx, rpc, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, emptyTestBudget().validateExitPlanCaps(plan), "transaction_cap_exceeded")
	// A narrowed durable budget must win over the pilot's maximum envelope.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,limits,transactionMicros}','1000000') WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.admitPhase3Bridge(ctx, rpc, id, o, d, e), "transaction_cap_exceeded")
	// Restore only this disposable fixture before proving the authorized cap.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,limits,transactionMicros}','20000000') WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	badPlan := plan
	badPlan.Exit = append([]phase3BridgeExitCost(nil), plan.Exit...)
	badPlan.Exit[0].Cost.TotalMicros = 20_000_001
	assertBudgetHold(t, db.persistPhase3ExitAdmission(ctx, rpc, id, o, d, badPlan), "bridge_exit_or_transaction_cap_exceeded")
	badPlan = plan
	badPlan.ExitAfterMicros--
	assertBudgetHold(t, db.persistPhase3ExitAdmission(ctx, rpc, id, o, d, badPlan), "exit_cost_sum_mismatch")
	if err = db.admitPhase3Bridge(ctx, rpc, id, o, d, e); err != nil {
		t.Fatal(err)
	}
	var encodedBudget, encodedAuth []byte
	if err = db.pool.QueryRow(ctx, `SELECT s.state->'phase3',o.expected_effects->'phase3' FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE operation_id=$1`, id).Scan(&encodedBudget, &encodedAuth); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encodedBudget, &budget) != nil || json.Unmarshal(encodedAuth, &auth) != nil {
		t.Fatal("decode measured admission")
	}
	reservation := budget.Reservations[id]
	cost := auth.BridgeAdmission.CurrentCost
	if cost.ExecutionCost == nil || reservation.ExecutionCostUpperMicros != cost.NetworkFeeMicros || reservation.ExecutionCostUpperMicros != cost.ExecutionCost.TotalMicros || reservation.UpperMicros <= reservation.ExecutionCostUpperMicros || auth.PilotAuthorityID != pilotBudgetAuthorityID {
		t.Fatal("did not derive and persist expense from executable cost")
	}
	if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, e.Request, e.ExpectedEffects, auth.BuildInput.Effects); err != nil {
		t.Fatal(err)
	}
	// Expiry after admission cannot sneak through a delayed build. Returning
	// custody and settling a sent transaction do not use this entry gate.
	expiredEntry := selectorEntryFixture(time.Now().UTC().Add(-time.Minute), SelectedRouteID, 10_000_000)
	expiredEntry.AllocationOperationID = id
	storeTestSelectorEntry(t, ctx, db, key, expiredEntry)
	assertBudgetHold(t, db.admitPhase3Bridge(ctx, rpc, id, o, d, e), "selector_entry_quote_expired")
	assertBudgetHold(t, authorizePhase3ProductionBuild(ctx, db, rpc, id, e.Request, e.ExpectedEffects, auth.BuildInput.Effects), "selector_entry_quote_expired")
	currentEntry := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 10_000_000)
	currentEntry.AllocationOperationID = id
	storeTestSelectorEntry(t, ctx, db, key, currentEntry)
	// Keep the gross debit within its reservation while simulating corruption of
	// only the expense allowance. Both actual production gates must reject it.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,ARRAY['phase3','reservations',$2,'executionCostUpperMicros'],'1'::jsonb) WHERE route_key=$1`, key, id); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.admitPhase3Bridge(ctx, rpc, id, o, d, e), "fresh_execution_cost_exceeds_reservation")
	assertBudgetHold(t, authorizePhase3ProductionBuild(ctx, db, rpc, id, e.Request, e.ExpectedEffects, auth.BuildInput.Effects), "fresh_execution_cost_exceeds_reservation")
	_, _, message, err := auth.BuildInput.decode()
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	hash := sha256Bytes(wire)
	auth.SignedWireSHA256 = hash
	encodedAuth, _ = json.Marshal(auth)
	if _, err = db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS signed_wire bytea`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,expected_effects=jsonb_set(expected_effects,'{phase3}',$3) WHERE operation_id=$1`, id, wire, encodedAuth); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	assertBudgetHold(t, db.authorizePhase3SendTx(ctx, tx, id, auth.IntentSHA256, hash, cost), "fresh_execution_cost_exceeds_reservation")
	_ = tx.Rollback(ctx)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,ARRAY['phase3','reservations',$2,'executionCostUpperMicros'],to_jsonb($3::bigint)) WHERE route_key=$1`, key, id, reservation.ExecutionCostUpperMicros); err != nil {
		t.Fatal(err)
	}
	// A valid quote reaches the production send authorization. Expiring just
	// that quote then holds the exact same signed wire without rewriting it.
	for _, expired := range []bool{false, true} {
		if expired {
			storeTestSelectorEntry(t, ctx, db, key, expiredEntry)
		}
		tx, err = db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = db.authorizePhase3SendTx(ctx, tx, id, auth.IntentSHA256, hash, cost)
		_ = tx.Rollback(ctx)
		if expired {
			assertBudgetHold(t, err, "selector_entry_quote_expired")
		} else if err != nil {
			t.Fatal("current entry refused final send", err)
		}
	}
}

func TestPilotProtocolCostIncludesBorrowFeeAndRounding(t *testing.T) {
	_, _, borrow, _, _, _, _ := borrowAdmissionFixture(t, 20_000, "")
	_, _, deposit, _, _, _, _ := depositAdmissionFixture(t, "")
	_, _, payoff, _, _, _, _ := payoffAdmissionFixture(t, 20_000)
	for _, tc := range []struct {
		name     string
		evidence KaminoExecutionEvidence
	}{
		{"borrow fee", borrow}, {"deposit receipt rounding", deposit}, {"full repayment rounding", payoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := tc.evidence.Request, tc.evidence.ExpectedEffects
			cost := pilotCostFixture(t, r, e)
			bound, err := classifyPilotExecutionCost(r, e, cost, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				t.Fatal(err)
			}
			var raw uint64
			switch leg {
			case kaminoLegBorrow:
				raw = cost.Debit.Raw - r.AmountRaw
			case kaminoLegDeposit:
				raw = e.Deposit.MaximumDebitRaw - e.Deposit.MinimumDebitRaw
			case kaminoLegRepay:
				raw = e.Repayment.MaximumDebitRaw - e.Repayment.MinimumDebitRaw + 1
			default:
				t.Fatal("unexpected fixture leg")
			}
			value, err := cost.TokenPrice.valueUpper(raw, cost.Debit.Mint, cost.Debit.TokenProgram, 42)
			if err != nil || raw == 0 || bound.ProtocolRoundingMicros != value || bound.TotalMicros != value+cost.NetworkFeeMicros {
				t.Fatalf("omitted fee/rounding: raw=%d %+v %v", raw, bound, err)
			}
		})
	}
}

func TestBudgetCreditRejectsInvertedIntervals(t *testing.T) {
	for _, side := range []string{"token", "usdc"} {
		p := budgetTestPrice("asset", classicTokenProgram, 6, 2, 2)
		p.Credit = &BudgetCreditBounds{TokenLowerSF: p.TokenUpperSF, USDCUpperSF: p.USDCLowerSF}
		if side == "token" {
			p.Credit.TokenLowerSF[0]++
		} else {
			p.Credit.USDCUpperSF[0]--
		}
		_, err := p.valueLower(100, "asset", classicTokenProgram, 42)
		assertBudgetHold(t, err, "invalid_credit_valuation_interval")
	}
}
