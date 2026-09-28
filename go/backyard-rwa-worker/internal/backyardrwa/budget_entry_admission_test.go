package backyardrwa

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"
)

func entrySwapAdmissionFixture(t *testing.T) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, manifest, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	o.Snapshot.HasPosition = false
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 0, 0
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0
	obligation := accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation)
	binary.LittleEndian.PutUint64(obligation.Data[128:136], 0)
	clear(obligation.Data[1296:1312])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	d := Decision{Action: SwapStableToCollateralStep, AmountRaw: 20_000, StrategyKey: o.Snapshot.RouteLane, Reason: "entry_swap", IdempotencyKey: "entry-swap-admission"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, d, 20_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved = true
	return o, d, e, manifest, rpc, client, accounts
}

func TestEntrySwapAdmissionReservesCompleteReverseAndBridgeReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := entrySwapAdmissionFixture(t)
	plan, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	want := []Action{ReportNAV, SwapCollateralToStableStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.Snapshot != o.Snapshot || plan.QuotedExit == nil || plan.QuotedExit.Input == nil || plan.ValidThroughSlot > 74 {
		t.Fatal("incomplete entry return reservation", plan)
	}
	r, effects, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(r, e.Request) || !reflect.DeepEqual(effects, e.ExpectedEffects) {
		t.Fatal("cost projection replaced current entry", err)
	}
	reverse, _, _, err := plan.QuotedExit.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	upper, _ := withdrawalUSDCExitEstimate(e.Request.QuotedOutputRaw)
	if reverse.(JupiterSwapRequest).AmountRaw != upper {
		t.Fatal("reverse does not price full upper output")
	}
	// Only reverse proceeds are restored: the spent 20,000 USDC must not also
	// survive in projected idle custody and inflate the reserved full sweep.
	if plan.Exit[3].Amount != plan.QuotedExit.EstimatedUpperOutputRaw || plan.Exit[5].Amount != plan.Exit[3].Amount {
		t.Fatal("spent USDC was double counted")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3EntrySwap(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
	// Exercise the actual persisted-input final-send path without a signer.
	_, _, message, err := plan.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := Phase3IntentDigest(e.Request, plan.Input.Effects)
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: plan.Input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_001)
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
		t.Fatal("persisted entry ignored changed source custody")
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_000)
	// Partial entry retains the untouched USDC in its complete return estimate.
	d.AmountRaw = 10_000
	e, err = prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 20_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved = true
	partial, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil || partial.Exit[3].Amount != 10_000+partial.QuotedExit.EstimatedUpperOutputRaw {
		t.Fatal("partial entry lost untouched USDC", err)
	}
	for _, spent := range []int64{Phase3FamilyCapMicros - 1, 0} {
		budget := emptyTestBudget()
		budget.Families["Ethena"] = FamilyBudget{SpentMicros: spent, ExitMicros: 10_000}
		reservation := BudgetReservation{OperationID: "entry", Family: "Ethena", IntentSHA256: intent, UpperMicros: partial.CurrentCost.TotalMicros, ExitAfterMicros: partial.ExitAfterMicros}
		err := budget.Admit(reservation)
		if spent > 0 {
			assertBudgetHold(t, err, "family_cap_exceeded")
		} else if err != nil {
			t.Fatal("valid entry extension failed", err)
		}
	}
}

func TestEntrySwapAdmissionRejectsUnaccountedPositionAndFinalSendCustodyDrift(t *testing.T) {
	for _, variant := range []string{"position", "collateral", "debt", "source", "unreserved", "output"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, client, accounts := entrySwapAdmissionFixture(t)
			switch variant {
			case "position":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[128:136], 1)
			case "collateral":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 1)
			case "debt":
				binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 1)
			case "source":
				binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_001)
			case "unreserved":
				e.Request.EntryReturnReserved = false
			case "output":
				e.ExpectedEffects.Accounts[1].AfterRaw++
			}
			if _, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("unsafe entry admitted")
			}
			if variant != "unreserved" {
				if _, err := observePhase3KnownBuildCost(context.Background(), rpc, e.Request, e.ExpectedEffects); err == nil {
					t.Fatal("changed current entry passed final-send revaluation")
				}
			}
		})
	}
}
