package backyard

import (
	"testing"
	"time"
)

// Reachability, not a claim that the new ledger already admits this sequence.
// The unchanged planner permits ordinary borrowing beside authenticated dust,
// and leverage-swap admission explicitly supports an existing destination
// balance. A price move before redeposit can then require mixed-custody funding.
func TestTopupCarryOrdinaryBorrowToMixedRiskIsReachable(t *testing.T) {
	m := autoInitializerFixtureManifest(t)
	s := leverageSnapshot(1.5)
	s.LeverageTargetLevel = 1.75
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.MinimumCollateralDepositRaw = 1, 1, 3
	borrow := m.DecideOnManifest(s)
	if borrow.Action != OpenRouteStep || borrow.Reason != leverageUpReason || borrow.AmountRaw <= 0 {
		t.Fatal("fixture does not reach ordinary borrow", borrow)
	}
	s.PositionDebtRaw += borrow.AmountRaw
	s.PositionDebtValueRaw += borrow.AmountRaw
	s.DebtIdleRaw = borrow.AmountRaw
	s.LTVBPS = s.PositionDebtValueRaw * 10_000 / s.PositionCollateralValueRaw
	swap := m.DecideOnManifest(s)
	if swap.Action != SwapDebtToCollateralStep {
		t.Fatal("borrowed cash did not reach the existing collateral conversion", swap)
	}
	// Controlled six-decimal/equal-price model used by leverageSnapshot.
	// Required intervening NAVs are assumed completed; no source is adopted.
	s.CollateralIdleRaw += borrow.AmountRaw
	s.PrimeIdleRaw, s.DebtIdleRaw = s.CollateralIdleRaw, 0
	s.PositionCollateralValueRaw = 1_200_000_000
	s.LTVBPS = s.PositionDebtValueRaw * 10_000 / s.PositionCollateralValueRaw
	funding := m.DecideOnManifest(s)
	if funding.Action != SwapCollateralToDebtStep || funding.Reason != "hard_ltv_buffer_swap" || funding.AmountRaw != s.CollateralIdleRaw || funding.AmountRaw <= 1 {
		t.Fatal("mixed-custody hard-risk sequence is reachable and must remain protected", funding)
	}
}

func TestTopupActiveInventoryKeepsPlannerRiskAndWithdrawalPriority(t *testing.T) {
	m := autoInitializerFixtureManifest(t)
	loan, _ := topupLoanFixture(t)
	origin := sha256Bytes([]byte("active allocation"))
	tranche := &topupTranche{Generation: 1, Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, LastOperationID: origin, LastSlot: 42,
		LastEffectsSHA256: sha256Bytes([]byte("allocation receipt")), AllocatedUSDCRaw: 100_000_000, USDCRemainingRaw: 100_000_000, Stage: topupTrancheAllocated}
	s := leverageSnapshot(1.5)
	s.TopupTranche, s.SquadsIdleRaw = tranche, int64(tranche.USDCRemainingRaw)
	s.LTVBPS = 6100
	if got := m.DecideOnManifest(s); got.Action != SwapUSDCToDebtStep || got.Reason != "hard_ltv_usdc_repayment_buffer" {
		t.Fatal("topup postponed risk planning", got)
	}
	s.LTVBPS, s.WithdrawalDemandRaw = leverageLevelLTVBPS(1.5), 50_000_000
	if got := m.DecideOnManifest(s); got.Action != StageSquadsToVoltr || got.Reason != partialStageReason {
		t.Fatal("topup postponed withdrawal planning", got)
	}
}

func TestTopupHandoffRejectsUnboundMixedCreditAndWrongEdge(t *testing.T) {
	loan, _ := topupLoanFixture(t)
	origin, last := sha256Bytes([]byte("allocation")), sha256Bytes([]byte("deposit"))
	before := topupTranche{Generation: 1, Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, LastOperationID: last, LastSlot: 42,
		LastEffectsSHA256: sha256Bytes([]byte("deposit receipt")), AllocatedUSDCRaw: 100, CollateralRemainingRaw: 1, DepositQuantumRaw: 3, Stage: topupTrancheComplete}
	b := topupTrancheBinding{Loan: loan, Lane: before.Lane, OriginOperationID: origin, AllocatedUSDCRaw: 100, Before: &before}
	destination := topupHandoffAuthority{OriginOperationID: sha256Bytes([]byte("exit")), AuthoritySHA256: sha256Bytes([]byte("intent"))}
	route := autoAUTOPYUSD
	minimum := uint64(200)
	e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 101, AfterRaw: 0},
		{Address: route.DebtCustody, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: minimum, MinimumAfterRaw: &minimum},
	}}
	receipt := topupReceipt(t, e, 43, 0, 201)
	_, err := reconcileTopupHandoff(before, b, destination, destination.OriginOperationID, SwapCollateralToDebtStep, e, receipt)
	assertBudgetHold(t, err, "topup_handoff_custody_changed")
	// Even a known namespace and valid hashes cannot substitute one swap
	// action for a different canonical source/destination pair.
	_, err = reconcileTopupHandoff(before, b, destination, destination.OriginOperationID, SwapUSDCToDebtStep, e, receipt)
	assertBudgetHold(t, err, "topup_handoff_action_unavailable")
}

func TestTopupRiskHandoffUsesVerifiedEmergencyEvidence(t *testing.T) {
	o, decision, _, manifest, plan := debtClearRiskFixture(t)
	id := sha256Bytes([]byte("topup-risk-operation"))
	proof, err := verifyDebtClearEmergency(manifest, o, decision, id, time.Now().UTC())
	if err != nil || proof == nil {
		t.Fatal("fixture lacks actual verified risk", err)
	}
	if !topupRiskHandoff(&plan, proof, id) {
		t.Fatal("verified protection cannot receive active inventory")
	}
	if topupRiskHandoff(&plan, nil, id) {
		t.Fatal("snapshot/reason alone granted risk handoff")
	}
	for name, mutate := range map[string]func(*debtClearRiskProof){
		"different operation":   func(p *debtClearRiskProof) { p.OperationID = sha256Bytes([]byte("other")) },
		"different observation": func(p *debtClearRiskProof) { p.ObservationID = sha256Bytes([]byte("other")) },
		"different slot":        func(p *debtClearRiskProof) { p.Slot++ },
		"unproven accounts":     func(p *debtClearRiskProof) { p.AccountsSHA256 = "" },
		"different ltv":         func(p *debtClearRiskProof) { p.LTVBPS-- },
		"lowered threshold":     func(p *debtClearRiskProof) { p.HardLTVBPS-- },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *proof
			mutate(&changed)
			if topupRiskHandoff(&plan, &changed, id) {
				t.Fatal("foreign proof granted handoff")
			}
		})
	}
	changedPlan := plan
	changedPlan.Decision.AmountRaw++
	if topupRiskHandoff(&changedPlan, proof, id) {
		t.Fatal("changed money instruction reused proof")
	}
	o.ObservedAt = time.Now().Add(-time.Minute)
	if _, err = verifyDebtClearEmergency(manifest, o, decision, id, time.Now().UTC()); err == nil {
		t.Fatal("expired risk read was admitted")
	}
}
