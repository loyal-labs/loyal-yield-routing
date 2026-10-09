package backyard

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func topupRiskEntryFixture(t *testing.T, carry uint64) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := autoEmergencyFundingFixture(t)
	const amount = uint64(10_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], amount)
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], carry)
	tr := *o.Snapshot.TopupTranche
	tr.Stage, tr.Generation = topupTrancheAllocated, 1
	tr.AllocatedUSDCRaw, tr.USDCRemainingRaw, tr.CollateralRemainingRaw = amount, amount, carry
	tr.LastOperationID = tr.OriginOperationID
	tr.DepositQuantumRaw = carry + 1
	enrich := func(_ context.Context, fresh *Observation) error {
		fresh.Snapshot.TopupTranche = &tr
		fresh.Snapshot.PilotActive = true
		fresh.Snapshot.JournalSequenceKnown, fresh.Snapshot.JournalReconciledSequenceRaw = true, 40
		fresh.Snapshot.JournalArmedNAVKnown, fresh.Snapshot.JournalArmedNAVRaw = true, fresh.Snapshot.PriorReportedNAVRaw
		fresh.Snapshot.CapitalMutated, fresh.Snapshot.ProgramIdentityKnown = true, true
		fresh.Snapshot.VoltrProgramDeploySlot, fresh.Snapshot.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
		return nil
	}
	o, _, err := autoObservationForAccounts(m, o.Snapshot.Slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	enrich(context.Background(), &o)
	d := m.DecideOnManifest(o.Snapshot)
	if d.Action != SwapStableToCollateralStep || d.Reason != topupRiskEntryReason {
		t.Fatalf("risk decision: %+v", d)
	}
	prepared, e, err := observeConfirmedJupiterExecutionEvidenceWithEnrichment(context.Background(), rpc, m, d, client, enrich)
	if err != nil {
		t.Fatal(err)
	}
	return prepared, d, e, m, rpc, client, accounts
}

func TestTopupRiskEntryUsesOwnedCashBeforeDust(t *testing.T) {
	for _, carry := range []uint64{0, 1} {
		t.Run(string(rune('0'+carry)), func(t *testing.T) {
			o, d, e, m, rpc, client, _ := topupRiskEntryFixture(t, carry)
			if !e.Request.EntryReturnReserved || !e.Request.TopupReturnReserved || e.Request.FullPayoffFunding {
				t.Fatal("entry classification changed")
			}
			plan, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
			if err != nil {
				t.Fatal(err)
			}
			if !topupFullyFundedReturn(plan) {
				t.Fatal("whole-position recovery missing")
			}
			proof, err := verifyDebtClearEmergency(m, o, d, "origin", time.Now().UTC())
			if err != nil || proof == nil {
				t.Fatal("verified risk missing", err)
			}
			required, err := debtClearRequired(m, e.Request, e.ExpectedEffects, &plan, debtClearRouteState{})
			if err != nil || !required {
				t.Fatal("recovery bypassed existing debt-clear authority", err)
			}
			runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
			assertBudgetHold(t, runtime.admitJupiter(context.Background(), "first-hop", o, d, e), "bridge_admission_database_unavailable")
			noProof := o
			noProof.routeBatch = nil
			if _, err = observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, noProof, d, e); err == nil {
				t.Fatal("snapshot alone authorized recovery")
			}
			recovered := o
			recovered.Snapshot.LTVBPS = 5000
			assertBudgetHold(t, func() error {
				_, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, recovered, d, e)
				return err
			}(), "topup_risk_entry_fresh_risk_required")
			foreign := o
			foreign.Snapshot.SquadsIdleRaw++
			if emergencyTopupEntryInventory(foreign.Snapshot, d) {
				t.Fatal("foreign cash adopted")
			}
			if _, err = observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, foreign, d, e); err == nil {
				t.Fatal("foreign cash admitted")
			}
			auth := phase3OperationAuthorization{BridgeAdmission: &plan}
			assertBudgetHold(t, validateTopupCapitalAuthorization(context.Background(), rpc, auth, o.Snapshot.Slot), "topup_risk_entry_authority_missing")
		})
	}
}

func TestTopupRiskEntryReceiptStartsAuthorizedHandoff(t *testing.T) {
	o, d, e, m, rpc, client, _ := topupRiskEntryFixture(t, 1)
	plan, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	id := sha256Bytes([]byte("risk-entry-receipt"))
	tr := o.Snapshot.TopupTranche
	authority := topupHandoffAuthority{OriginOperationID: id, AuthoritySHA256: sha256Bytes([]byte("admitted-risk-intent"))}
	binding := topupTrancheBinding{Before: tr, Loan: tr.Loan, Lane: tr.Lane, OriginOperationID: tr.OriginOperationID, AllocatedUSDCRaw: tr.AllocatedUSDCRaw, Handoff: authority}
	receipt := topupReceipt(t, e.ExpectedEffects, o.Snapshot.Slot, 0, e.ExpectedEffects.Accounts[1].AfterRaw)
	next, err := reconcileTopupHandoff(*tr, binding, authority, id, d.Action, e.ExpectedEffects, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if next.Stage != topupTrancheHandoff || next.USDCRemainingRaw != 0 || next.CollateralRemainingRaw != e.ExpectedEffects.Accounts[1].AfterRaw || next.Loan != tr.Loan || next.Handoff != authority || !topupFullyFundedReturn(plan) {
		t.Fatal("receipt lost inventory, loan or authority")
	}
}

func TestTopupAllocationRejectsInsufficientRecoveryFunding(t *testing.T) {
	o, d, e, m, rpc, _, _ := debtTopupAllocationFixture(t)
	route := autoAUTOPYUSD
	client := autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
		out := amount
		if in == route.Kamino.CollateralMint {
			out = amount / 6000
		}
		if destination == route.Kamino.CollateralMint {
			out = amount * 600
		}
		return out, out * 9950 / 10000
	}, nil)
	_, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e)
	assertBudgetHold(t, err, "topup_allocation_requires_full_recovery_funding")
}
