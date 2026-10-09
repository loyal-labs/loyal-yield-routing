package backyard

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Real PostgreSQL atomicity/restart tests. Market pricing and signing are not
// enabled: fixtures seed an admitted wire/reservation, then use production
// finalized reconciliation and durable admission/authority fences.
func TestTopupTrancheDatabasePersistence(t *testing.T) {
	if os.Getenv("PHASE3_TEST_DATABASE_URL") == "" {
		t.Skip("requires parent-owned disposable PostgreSQL")
	}
	accounts := topupReviewedProgramAccounts(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_topup_ledger_test", 120*time.Second)
	defer cancel()
	m := autoInitializerFixtureManifest(t)
	key := "topup-ledger"
	b := emptyTestBudget()
	b.Families["AUTO"] = FamilyBudget{SpentMicros: 10_000, ExitMicros: 5_000_000}
	state, _ := json.Marshal(map[string]any{"generation": 1, "phase3": b})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,1)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "topup-ledger-test", 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ address, owner string }{{bridgeSquadsATA, bridgeVault}, {bridgeStrategyATA, bridgeStrategyAuth}} {
		data := make([]byte, 165)
		putKey(t, data[:32], bridgeUSDC)
		putKey(t, data[32:64], row.owner)
		data[108] = 1
		accounts = append(accounts, ConfirmedAccount{Address: row.address, Owner: bridgeTokenProgram, Lamports: 1, Data: data})
	}
	rpc := topupLoanRPC(t, accounts, "")
	origin, err := observeTopupLoanOrigin(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	slot := origin.ObservedSlot
	sequence := 0
	var current *topupTranche
	readBudget := func() Phase3Budget {
		t.Helper()
		var raw []byte
		var out Phase3Budget
		if err := db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil || json.Unmarshal(raw, &out) != nil {
			t.Fatal("budget read", err)
		}
		return out
	}
	seed := func(d Decision, e ExpectedEffects, s Snapshot) (string, Observation, phase3BridgeAdmission) {
		t.Helper()
		sequence++
		slot++
		binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], uint64(slot))
		id := sha256Bytes([]byte(fmt.Sprintf("%s-%d", key, sequence)))
		s.RouteKind, s.RouteLane, s.StrategyKey, s.Fresh, s.PilotActive, s.Slot = RouteKind, autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true, true, slot
		s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, int64(origin.CollateralRaw), 1000
		s.TopupTranche = current
		o := Observation{Snapshot: s, ObservedAt: time.Now().UTC()}
		o.Snapshot.ObservationID = id
		d.StrategyKey = autoAUTOPYUSD.Lane
		plan := phase3BridgeAdmission{Snapshot: o.Snapshot, Decision: d, Payoff: &KaminoPayoffBound{}, ValidThroughSlot: slot + 32, topupOrigin: &origin}
		envelope := map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "decision": newDecisionEvidence(o, d, m.SHA256, sha256Bytes([]byte("policies"))), "expectedEffects": e}
		if current != nil {
			envelope["topupTranche"] = current
		}
		raw, _ := json.Marshal(envelope)
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(d.Action), d.StrategyKey, raw); err != nil {
			t.Fatal(err)
		}
		return id, o, plan
	}
	var handoffRisk *debtClearRiskProof
	admit := func(id string, o Observation, plan phase3BridgeAdmission) error {
		t.Helper()
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		budget, auth, err := db.readPhase3BudgetTx(ctx, tx, id)
		if err != nil {
			return err
		}
		intent := sha256Bytes([]byte(id + "-intent"))
		if err = db.bindTopupAdmissionTx(ctx, tx, rpc, id, o, plan.Decision, plan, &auth, intent, handoffRisk); err != nil {
			return err
		}
		if auth.GoalID != "" {
			return tx.Commit(ctx)
		}
		exit := budget.Families["AUTO"].ExitMicros
		recovery := plan.Decision.Action == StageSquadsToVoltr || plan.Decision.Action == VoltrRestoreIdle
		if recovery {
			exit -= 1000
		}
		if err = budget.Admit(BudgetReservation{OperationID: id, Family: "AUTO", IntentSHA256: intent, UpperMicros: 1000, ExitAfterMicros: exit, Recovery: recovery}); err != nil {
			return err
		}
		auth.GoalID, auth.IntentSHA256, auth.BridgeAdmission = Phase3GoalID, intent, &plan
		if err = db.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	finalize := func(id string, e ExpectedEffects, after ...uint64) {
		t.Helper()
		receipt := topupReceipt(t, e, slot+1, after...)
		slot++
		wire := []byte("persisted-test-wire-" + id)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciling',signed_wire=$2,signed_wire_sha256=$3,transaction_signature=$4,confirmed_slot=$5,expected_effects=jsonb_set(expected_effects,'{phase3,signedWireSha256}',to_jsonb($3::text),true) WHERE operation_id=$1`, id, wire, sha256Bytes(wire), receipt.Signature, receipt.Slot); err != nil {
			t.Fatal(err)
		}
		before := readBudget()
		if _, ok := before.Reservations[id]; !ok {
			t.Fatal("signed operation lost reservation")
		}
		reconciliation, actual, err := ReconcileConfirmedTransaction(e, receipt)
		if err != nil {
			t.Fatal(err)
		}
		// Corrupt only the decision predecessor: the existing transaction has
		// already updated status/settled budget when the new fence rejects it.
		var saved []byte
		if err = db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		if current != nil {
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{topupTranche,generation}',to_jsonb($2::bigint),true) WHERE operation_id=$1`, id, current.Generation+1); err != nil {
				t.Fatal(err)
			}
			if err = db.markReconciledOnManifest(ctx, m, id, reconciliation, actual, receipt); err == nil {
				t.Fatal("stale generation settled")
			}
			afterFailure := readBudget()
			rawBefore, _ := json.Marshal(before)
			rawAfter, _ := json.Marshal(afterFailure)
			if string(rawBefore) != string(rawAfter) {
				t.Fatal("failed ledger transition changed spend/exit/reservations")
			}
			var status string
			var retained []byte
			if err = db.pool.QueryRow(ctx, `SELECT status,signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &retained); err != nil || status != "reconciling" || string(retained) != string(wire) {
				t.Fatal("rollback lost signed recovery", err)
			}
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=$2 WHERE operation_id=$1`, id, saved); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.markReconciledOnManifest(ctx, m, id, reconciliation, actual, receipt); err != nil {
			t.Fatal(err)
		}
		afterBudget := readBudget()
		if afterBudget.Families["AUTO"].SpentMicros != before.Families["AUTO"].SpentMicros+1000 || afterBudget.Families["AUTO"].ExitMicros < before.Families["AUTO"].ExitMicros-1000 || len(afterBudget.Reservations) != 0 {
			t.Fatal("settlement lost conserved budget")
		}
		if err = db.markReconciledOnManifest(ctx, m, id, reconciliation, actual, receipt); err == nil {
			t.Fatal("finalized receipt replayed")
		}
		// A fresh DB client after EVERY finalized leg must recover exact state.
		url := strings.Replace(os.Getenv("PHASE3_TEST_DATABASE_URL"), "/phase3_budget_test", "/phase3_topup_ledger_test", 1)
		restarted, err := OpenDatabase(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		current, err = restarted.LoadTopupTranche(ctx, key)
		restarted.Close()
		if err != nil {
			t.Fatal("restart origin proof", err)
		}
		binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], uint64(receipt.Slot))
		for _, balance := range receipt.PostTokenBalances {
			for _, a := range accounts {
				if a.Address == balance.Address && len(a.Data) >= 72 {
					binary.LittleEndian.PutUint64(a.Data[64:72], balance.Raw)
				}
			}
		}
	}
	allocation, _, _, err := bridgeExpectedEffects(Decision{Action: VoltrAllocateToSquads, AmountRaw: 100_000_000}, 200_000_000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	allocation.Kind, allocation.ReturnData = "bridge", expectedAdaptorReturnData(100_000_000)
	id, o, plan := seed(Decision{Action: VoltrAllocateToSquads, Reason: topupAllocationReason, AmountRaw: 100_000_000}, allocation, Snapshot{VoltrIdleRaw: 200_000_000})
	withoutOrigin := plan
	withoutOrigin.topupOrigin = nil
	assertBudgetHold(t, admit(id, o, withoutOrigin), "topup_requires_prepricing_finalized_origin")
	badPrice := o
	badPrice.Snapshot.PositionCollateralRaw++
	assertBudgetHold(t, admit(id, badPrice, plan), "topup_snapshot_position_changed")
	for i := range accounts {
		if accounts[i].Address == bridgeSquadsATA {
			accounts[i].Owner = token2022Program
			assertBudgetHold(t, admit(id, o, plan), "topup_snapshot_custody_changed")
			accounts[i].Owner = bridgeTokenProgram
		}
	}
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	originalAuth := loadAutoInitializerAuth(t, ctx, db, id)
	plan.topupOrigin = nil // retry must not need or capture another finalized origin
	if err = admit(id, o, plan); err != nil {
		t.Fatal("retry recaptured origin", err)
	}
	if loadAutoInitializerAuth(t, ctx, db, id).Topup.Loan != originalAuth.Topup.Loan {
		t.Fatal("retry rebased origin")
	}
	finalize(id, allocation, 100_000_000, 0, 100_000_000)
	if current.USDCRemainingRaw != 100_000_000 {
		t.Fatal("allocation was not durable")
	}
	duplicate, dupO, dupPlan := seed(Decision{Action: VoltrAllocateToSquads, Reason: topupAllocationReason, AmountRaw: 100_000_000}, allocation, Snapshot{VoltrIdleRaw: 200_000_000, SquadsIdleRaw: 100_000_000})
	assertBudgetHold(t, admit(duplicate, dupO, dupPlan), "topup_allocation_binding_unavailable")
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1`, duplicate); err != nil {
		t.Fatal(err)
	}
	// Nonmutating NAV preserves the active tranche, including its generation.
	nav, _, _, _ := bridgeExpectedEffects(Decision{Action: ReportNAV}, 100_000_000, 0, 100_000_000)
	nav.Kind, nav.ReturnData = "bridge", expectedAdaptorReturnData(100_000_000)
	id, o, plan = seed(Decision{Action: ReportNAV, Reason: "post_mutation_nav_due"}, nav, Snapshot{VoltrIdleRaw: 100_000_000, SquadsIdleRaw: 100_000_000})
	prior := *current
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, nav, 100_000_000, 0, 100_000_000)
	if *current != prior {
		t.Fatal("routine NAV advanced or dropped ownership")
	}
	// Direct staging starts a legitimate existing partial-withdrawal flow.
	demandSnapshot := Snapshot{VoltrIdleRaw: 100_000_000, SquadsIdleRaw: 100_000_000, WithdrawalDemandRaw: 150_000_000,
		PositionCollateralValueRaw: 1_000_000_000, PositionDebtValueRaw: 1000, LTVBPS: 1, LeverageTargetLevel: 1.5}
	stage, _, _, _ := bridgeExpectedEffects(Decision{Action: StageSquadsToVoltr, AmountRaw: 100_000_000}, 100_000_000, 0, 100_000_000)
	stage.Kind = "bridge"
	id, o, plan = seed(Decision{Action: StageSquadsToVoltr, Reason: partialStageReason, AmountRaw: 100_000_000}, stage, demandSnapshot)
	if !topupPartialHandoff(&plan) {
		t.Fatal("fixture is not an existing authorized partial stage")
	}
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	bound := loadAutoInitializerAuth(t, ctx, db, id).Topup
	if bound.Handoff.OriginOperationID != id {
		t.Fatal("direct stage lost authorized destination")
	}
	finalize(id, stage, 100_000_000, 0)
	if current.Stage != topupTrancheHandoff || current.StrategyRemainingRaw != 100_000_000 {
		t.Fatal("handoff inventory lost")
	}
	partial, err := db.LoadPartialWithdrawal(ctx, key)
	if err != nil || partial == nil || partial.OperationID != id {
		t.Fatal("direct stage did not establish journal-linked continuation", err)
	}
	// Demand disappears, but the same already-authorized stage must restore.
	restore, _, _, _ := bridgeExpectedEffects(Decision{Action: VoltrRestoreIdle, AmountRaw: 100_000_000}, 100_000_000, 100_000_000, 0)
	restore.Kind, restore.ReturnData = "bridge", expectedAdaptorReturnData(0)
	id, o, plan = seed(Decision{Action: VoltrRestoreIdle, Reason: "withdrawal_staged", AmountRaw: 100_000_000}, restore, Snapshot{VoltrIdleRaw: 100_000_000, VoltrStrategyIdleRaw: 100_000_000, PartialWithdrawalOperationID: partial.OperationID, PartialWithdrawalLTVBPS: partial.LTVBPS})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	var saved []byte
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,topup,handoff,authoritySha256}',to_jsonb($2::text),true) WHERE operation_id=$1`, id, sha256Bytes([]byte("not authority"))); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.authorizeTopupStateTx(ctx, tx, id, false); err == nil {
		t.Fatal("syntactically valid SHA granted handoff authority")
	}
	tx.Rollback(ctx)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=$2 WHERE operation_id=$1`, id, saved); err != nil {
		t.Fatal(err)
	}
	finalize(id, restore, 200_000_000, 0, 0)
	if current.Stage != topupTrancheComplete || current.Handoff != bound.Handoff {
		t.Fatal("demand disappearance erased origin or stranded cash")
	}
	// A separate completed tranche now follows every top-up capital leg;
	// each finalize call reopens a DB client and checks fee/reserve atomicity.
	origin, err = observeTopupLoanOrigin(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	id, o, plan = seed(Decision{Action: VoltrAllocateToSquads, Reason: topupAllocationReason, AmountRaw: 100_000_000}, allocation, Snapshot{VoltrIdleRaw: 200_000_000})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, allocation, 100_000_000, 0, 100_000_000)
	minimum := uint64(190_000_000)
	swap := ExpectedEffects{Schema: allocation.Schema, Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: bridgeSquadsATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: 100_000_000, AfterRaw: 0},
		{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: minimum, MinimumAfterRaw: &minimum},
	}}
	id, o, plan = seed(Decision{Action: SwapStableToCollateralStep, Reason: topupSwapReason, AmountRaw: 100_000_000}, swap, Snapshot{VoltrIdleRaw: 100_000_000, SquadsIdleRaw: 100_000_000})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, swap, 0, 207_000_000)
	if current.CollateralRemainingRaw != 207_000_000 {
		t.Fatal("actual swap proceeds replaced by quote minimum")
	}
	deposit := ExpectedEffects{Schema: allocation.Schema, Kind: "kamino-deposit", Conserved: true, Deposit: &ExpectedDeposit{MinimumDebitRaw: 206_999_998, MaximumDebitRaw: 207_000_000}, Accounts: []ExpectedAccountEffect{
		{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 207_000_000, AfterRaw: 0},
		{Address: autoAUTOPYUSD.CollateralLiquiditySupply, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 0, AfterRaw: 207_000_000},
	}}
	id, o, plan = seed(Decision{Action: OpenRouteStep, Reason: topupDepositReason, AmountRaw: 207_000_000}, deposit, Snapshot{VoltrIdleRaw: 100_000_000, CollateralIdleRaw: 207_000_000, PrimeIdleRaw: 207_000_000})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, deposit, 1, 206_999_999)
	if current.Stage != topupTrancheComplete || current.CollateralRemainingRaw != 1 || current.DepositQuantumRaw != 3 {
		t.Fatal("deposit rounding carry lost on restart")
	}
	// Carry plus allocation are one exact owned inventory. Direct staging
	// consumes the USDC part while the authenticated collateral carry stays.
	origin, err = observeTopupLoanOrigin(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	allocationAgain, _, _, _ := bridgeExpectedEffects(Decision{Action: VoltrAllocateToSquads, AmountRaw: 100_000_000}, 100_000_000, 0, 0)
	allocationAgain.Kind, allocationAgain.ReturnData = "bridge", expectedAdaptorReturnData(100_000_000)
	id, o, plan = seed(Decision{Action: VoltrAllocateToSquads, Reason: topupAllocationReason, AmountRaw: 100_000_000}, allocationAgain, Snapshot{VoltrIdleRaw: 100_000_000, CollateralIdleRaw: 1, PrimeIdleRaw: 1})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, allocationAgain, 0, 0, 100_000_000)
	stageAgain, _, _, _ := bridgeExpectedEffects(Decision{Action: StageSquadsToVoltr, AmountRaw: 100_000_000}, 0, 0, 100_000_000)
	stageAgain.Kind = "bridge"
	id, o, plan = seed(Decision{Action: StageSquadsToVoltr, Reason: partialStageReason, AmountRaw: 100_000_000}, stageAgain, Snapshot{SquadsIdleRaw: 100_000_000, CollateralIdleRaw: 1, PrimeIdleRaw: 1, WithdrawalDemandRaw: 50_000_000, PositionCollateralValueRaw: 1_000_000_000, PositionDebtValueRaw: 1000, LTVBPS: 1, LeverageTargetLevel: 1.5})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, stageAgain, 100_000_000, 0)
	if current.Stage != topupTrancheHandoff || current.CollateralRemainingRaw != 1 || current.StrategyRemainingRaw != 100_000_000 {
		t.Fatal("partial inventory handoff lost carry")
	}
	restoreAgain, _, _, _ := bridgeExpectedEffects(Decision{Action: VoltrRestoreIdle, AmountRaw: 100_000_000}, 0, 100_000_000, 0)
	restoreAgain.Kind, restoreAgain.ReturnData = "bridge", expectedAdaptorReturnData(0)
	id, o, plan = seed(Decision{Action: VoltrRestoreIdle, Reason: "withdrawal_staged", AmountRaw: 100_000_000}, restoreAgain, Snapshot{VoltrStrategyIdleRaw: 100_000_000, CollateralIdleRaw: 1, PrimeIdleRaw: 1})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, restoreAgain, 100_000_000, 0, 0)
	if current.Stage != topupTrancheComplete || current.CollateralRemainingRaw != 1 {
		t.Fatal("known rounding carry stranded or erased after handoff")
	}
	// A completed top-up must not seize custody from independently admitted
	// ordinary borrowing. Record its finalized credit, not an invented balance.
	ordinaryMinimum := uint64(100)
	ordinarySwap := ExpectedEffects{Schema: allocation.Schema, Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: autoAUTOPYUSD.DebtCustody, Owner: autoAUTOPYUSD.DebtTokenProgram, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 100, AfterRaw: 0},
		{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 1, AfterRaw: ordinaryMinimum, MinimumAfterRaw: &ordinaryMinimum},
	}}
	id, o, plan = seed(Decision{Action: SwapDebtToCollateralStep, Reason: "ordinary_borrowed_collateral", AmountRaw: 100}, ordinarySwap, Snapshot{DebtIdleRaw: 100, CollateralIdleRaw: 1})
	if err = admit(id, o, plan); err != nil {
		t.Fatal("completed carry blocked ordinary credit", err)
	}
	if loadAutoInitializerAuth(t, ctx, db, id).Topup != nil {
		t.Fatal("completed carry granted or required top-up authority")
	}
	finalize(id, ordinarySwap, 0, 101)
	if current.Stage == topupTrancheComplete || topupWorkInFlight(current) || current.CollateralRemainingRaw != 101 {
		t.Fatal("ordinary credit lost provenance or became top-up authority")
	}
	ordinaryDeposit := ExpectedEffects{Schema: allocation.Schema, Kind: "kamino-deposit", Conserved: true,
		Deposit: &ExpectedDeposit{MinimumDebitRaw: 99, MaximumDebitRaw: 101}, Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 101, AfterRaw: 0},
			{Address: autoAUTOPYUSD.CollateralLiquiditySupply, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 206_999_999, AfterRaw: 207_000_100},
		}}
	id, o, plan = seed(Decision{Action: OpenRouteStep, Reason: "single_loop_redeposit", AmountRaw: 101}, ordinaryDeposit, Snapshot{CollateralIdleRaw: 101})
	if err = admit(id, o, plan); err != nil {
		t.Fatal("ordinary redeposit blocked", err)
	}
	finalize(id, ordinaryDeposit, 2, 207_000_098)
	if current.Stage != topupTrancheComplete || current.CollateralRemainingRaw != 2 || current.DepositQuantumRaw != 3 {
		t.Fatal("new ordinary-deposit remainder lost receipt provenance")
	}
	ordinarySwap.Accounts[1].BeforeRaw = 2
	id, o, plan = seed(Decision{Action: SwapDebtToCollateralStep, Reason: "ordinary_borrowed_collateral", AmountRaw: 100}, ordinarySwap, Snapshot{DebtIdleRaw: 100, CollateralIdleRaw: 2})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, ordinarySwap, 0, 102)
	// Existing hard-risk funding may consume the combined custody without a
	// new top-up handoff or full-debt consent. The parent admission owns authority.
	riskMinimum := uint64(90)
	riskSwap := ExpectedEffects{Schema: allocation.Schema, Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 102, AfterRaw: 0},
		{Address: autoAUTOPYUSD.DebtCustody, Owner: autoAUTOPYUSD.DebtTokenProgram, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: riskMinimum, MinimumAfterRaw: &riskMinimum},
	}}
	id, o, plan = seed(Decision{Action: SwapCollateralToDebtStep, Reason: "hard_ltv_buffer_swap", AmountRaw: 102}, riskSwap, Snapshot{CollateralIdleRaw: 102, LTVBPS: 6100})
	if err = admit(id, o, plan); err != nil {
		t.Fatal("completed carry blocked existing risk funding", err)
	}
	finalize(id, riskSwap, 0, 91)
	if current.Stage != topupTrancheComplete || current.CollateralRemainingRaw != 0 {
		t.Fatal("finalized risk funding retained phantom collateral")
	}
	ordinaryRepay := ExpectedEffects{Schema: allocation.Schema, Kind: "kamino-repay", Conserved: true,
		Repayment: &ExpectedRepayment{MinimumDebitRaw: 91, MaximumDebitRaw: 91}, Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtCustody, Owner: autoAUTOPYUSD.DebtTokenProgram, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 91, AfterRaw: 0},
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: autoAUTOPYUSD.DebtTokenProgram, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 0, AfterRaw: 91},
		}}
	id, o, plan = seed(Decision{Action: DeleverRouteStep, Reason: "hard_ltv_repay", AmountRaw: 91}, ordinaryRepay, Snapshot{DebtIdleRaw: 91})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, ordinaryRepay, 0, 91)
	origin, err = observeTopupLoanOrigin(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	id, o, plan = seed(Decision{Action: VoltrAllocateToSquads, Reason: topupAllocationReason, AmountRaw: 100_000_000}, allocation, Snapshot{VoltrIdleRaw: 200_000_000})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, allocation, 100_000_000, 0, 100_000_000)
	id, o, plan = seed(Decision{Action: SwapStableToCollateralStep, Reason: topupSwapReason, AmountRaw: 100_000_000}, swap, Snapshot{VoltrIdleRaw: 100_000_000, SquadsIdleRaw: 100_000_000})
	if err = admit(id, o, plan); err != nil {
		t.Fatal(err)
	}
	finalize(id, swap, 0, 207_000_000)
	activeRisk := leverageSnapshot(1.5)
	activeRisk.LTVBPS, activeRisk.CollateralIdleRaw, activeRisk.PrimeIdleRaw, activeRisk.ValuationSource = 6100, 207_000_000, 207_000_000, "confirmed"
	funding := riskSwap
	funding.Accounts = append([]ExpectedAccountEffect(nil), riskSwap.Accounts...)
	funding.Accounts[0].BeforeRaw = 207_000_000
	id, o, plan = seed(Decision{Action: SwapCollateralToDebtStep, Reason: "hard_ltv_buffer_swap", AmountRaw: 207_000_000}, funding, activeRisk)
	assertBudgetHold(t, admit(id, o, plan), "topup_handoff_requires_existing_authority")
	// This test supplies the already-verified proof at the private admission
	// seam. The separate actual-account verifier test rejects snapshot-only risk.
	handoffRisk = &debtClearRiskProof{OperationID: id, ObservationID: o.Snapshot.ObservationID,
		AccountsSHA256: hashConfirmedAccounts(accounts), ValuationSource: "confirmed", Slot: o.Snapshot.Slot,
		ObservedAt: o.ObservedAt, LTVBPS: o.Snapshot.LTVBPS, HardLTVBPS: min(o.Snapshot.LiquidationThresholdBPS-1500, int64(6000))}
	if err = admit(id, o, plan); err != nil {
		t.Fatal("verified risk blocked by active tranche", err)
	}
	riskAuth := loadAutoInitializerAuth(t, ctx, db, id)
	if riskAuth.TopupRisk == nil || riskAuth.Topup == nil || riskAuth.Topup.Handoff.OriginOperationID != id {
		t.Fatal("risk handoff lost verified operation evidence")
	}
	finalize(id, funding, 0, 91)
	if current.Stage != topupTrancheHandoff || current.DebtRemainingRaw != 91 || current.USDCRemainingRaw != 0 {
		t.Fatal("emergency funding lost active inventory")
	}
	ordinaryRepay.Accounts[1].BeforeRaw, ordinaryRepay.Accounts[1].AfterRaw = 91, 182
	id, o, plan = seed(Decision{Action: DeleverRouteStep, Reason: "hard_ltv_repay", AmountRaw: 91}, ordinaryRepay, Snapshot{DebtIdleRaw: 91, LTVBPS: 6100})
	if err = admit(id, o, plan); err != nil {
		t.Fatal("risk continuation lost original proof", err)
	}
	finalize(id, ordinaryRepay, 0, 182)
	if current.Stage != topupTrancheComplete || current.DebtRemainingRaw != 0 {
		t.Fatal("partial emergency repayment stranded handoff")
	}
}
