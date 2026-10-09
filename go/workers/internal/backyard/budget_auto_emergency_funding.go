package backyard

import (
	"bytes"
	"context"
	"math/big"
	"time"
)

// This is a classification, never risk or repayment authority. Admission and
// every fresh execution fence independently verify the actual owned origin.
func emergencyTopupFundingInventory(s Snapshot, d Decision) bool {
	t := s.TopupTranche
	return s.RouteLane == autoAUTOPYUSD.Lane && s.StrategyKey == s.RouteLane && d.StrategyKey == s.RouteLane &&
		d.Action == SwapCollateralToDebtStep && d.Reason == "hard_ltv_buffer_swap" &&
		s.PilotActive && s.HasPosition && s.PositionDebtRaw > 1 && debtCashRaw(s) == 0 &&
		t != nil && t.validate() == nil && (t.Stage == topupTrancheCollateral || t.Stage == topupTrancheHandoff) &&
		t.LastSlot <= s.Slot && t.Lane == s.RouteLane && t.USDCRemainingRaw == 0 && t.StrategyRemainingRaw == 0 &&
		s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0 && s.CollateralIdleRaw > 0 && s.PrimeIdleRaw == s.CollateralIdleRaw &&
		t.CollateralRemainingRaw == uint64(s.CollateralIdleRaw) && t.DebtRemainingRaw == uint64(debtCashRaw(s)) && d.AmountRaw == s.CollateralIdleRaw
}

func emergencyTopupFundingRequest(s Snapshot, d Decision, r JupiterSwapRequest) bool {
	return emergencyTopupFundingInventory(s, d) && r.EmergencyTopupFunding && !r.FullPayoffFunding && !r.EntryReturnReserved && !r.PositionReturnReserved && !r.TopupReturnReserved &&
		r.RouteLane == s.RouteLane && r.Action == d.Action && r.AmountRaw == uint64(d.AmountRaw) && r.MinimumOutputRaw > 0 && r.MinimumOutputRaw < uint64(s.PositionDebtRaw-debtCashRaw(s))
}

// Cost-only evidence for a guaranteed strict partial repayment and the whole
// remaining return. Upper output is an estimate, never a receipt ceiling or
// authority to spend a larger realized balance.
type emergencyTopupFundingProof struct {
	BeforeSlot          int64                  `json:"beforeSlot"`
	Before              []ConfirmedAccount     `json:"before"`
	Projection          phase3KaminoProjection `json:"projection"`
	RepaymentRaw        uint64                 `json:"repaymentRaw"`
	EstimatedSurplusRaw uint64                 `json:"estimatedSurplusRaw"`
}

func validateEmergencyTopupFundingEffects(m RouteManifest, s Snapshot, d Decision, e JupiterExecutionEvidence, accounts []ConfirmedAccount) error {
	r := e.Request
	if !emergencyTopupFundingRequest(s, d, r) || len(e.ExpectedEffects.Accounts) != 2 {
		return budgetHold("emergency_topup_funding_intent_invalid")
	}
	floor, err := jupiterInstructionWireFloor(r.Instruction)
	if err != nil || floor < r.MinimumOutputRaw {
		return budgetHold("emergency_topup_funding_minimum_invalid")
	}
	if _, err := m.measureExecutableDebit(r, e.ExpectedEffects); err != nil {
		return err
	}
	for i, row := range []struct {
		address, mint, program string
		raw                    uint64
	}{
		{autoAUTOPYUSD.CollateralCustody, autoAUTOPYUSD.Kamino.CollateralMint, autoAUTOPYUSD.CollateralTokenProgram, uint64(s.CollateralIdleRaw)},
		{autoAUTOPYUSD.DebtCustody, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.DebtTokenProgram, uint64(debtCashRaw(s))},
	} {
		a := accountAt(accounts, row.address)
		effect := e.ExpectedEffects.Accounts[i]
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != row.program || custody.Raw != row.raw || effect.Address != row.address || effect.Mint != row.mint || effect.Authority != bridgeVault || effect.Owner != row.program || effect.BeforeRaw != row.raw {
			return budgetHold("emergency_topup_funding_custody_changed")
		}
	}
	source, dest := e.ExpectedEffects.Accounts[0], e.ExpectedEffects.Accounts[1]
	if source.AfterRaw != 0 || dest.MinimumAfterRaw == nil || *dest.MinimumAfterRaw != dest.BeforeRaw+r.MinimumOutputRaw || dest.AfterRaw != *dest.MinimumAfterRaw {
		return budgetHold("emergency_topup_funding_effects_invalid")
	}
	return nil
}

// Jupiter has no permission to refresh, borrow, repay, or otherwise mutate
// the lending position. Check the actual raw account, not just its raw ceil.
func validateEmergencyFundingSwapProjection(before []ConfirmedAccount, post phase3KaminoProjection, e JupiterExecutionEvidence) error {
	route := autoAUTOPYUSD
	for _, address := range []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve, route.Kamino.Market} {
		old, next := accountAt(before, address), accountAt(post.Accounts, address)
		if old.Owner != kaminoProgram || next.Owner != old.Owner || next.Executable || next.Lamports == 0 || next.Lamports != old.Lamports || !bytes.Equal(old.Data, next.Data) {
			return budgetHold("emergency_topup_funding_principal_changed")
		}
	}
	for _, effect := range e.ExpectedEffects.Accounts {
		a := accountAt(post.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		minimum := effect.AfterRaw
		if err != nil || a.Owner != effect.Owner || a.Executable || a.Lamports == 0 || (effect.MinimumAfterRaw == nil && cash.Raw != minimum) || (effect.MinimumAfterRaw != nil && cash.Raw < minimum) {
			return budgetHold("emergency_topup_funding_projection_custody_changed")
		}
	}
	return nil
}

func validateEmergencyFundingStrictRepay(accounts []ConfirmedAccount, amount uint64) error {
	route := autoAUTOPYUSD
	old, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	debit := new(big.Int).Lsh(new(big.Int).SetUint64(amount), 60)
	// Do not rely on rounded-up raw principal to prove this cannot clear debt.
	residual := new(big.Int).Sub(littleInt(old.debtAmountSF[:]), debit)
	if amount == 0 || residual.Sign() <= 0 {
		return budgetHold("emergency_topup_funding_not_strict_partial")
	}
	market := accountAt(accounts, route.Kamino.Market)
	if _, err := decodeKaminoMarketEmergency(market, route.Kamino); err != nil {
		return err
	}
	value := new(big.Int).Mul(residual, littleInt(reserve.marketPriceSF[:]))
	scale := new(big.Int).Lsh(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(reserve.mintDecimals)), nil), 60)
	value.Quo(value, scale)
	if value.Sign() <= 0 || value.Cmp(littleInt(market.Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])) <= 0 {
		return budgetHold("emergency_topup_funding_residual_too_small")
	}
	return nil
}

func observeEmergencyTopupFundingPrestate(ctx context.Context, rpc *RPCClient, m RouteManifest, s Snapshot, d Decision, e JupiterExecutionEvidence, through int64) (phase3KaminoProjection, []ConfirmedAccount, int64, error) {
	if !emergencyTopupFundingRequest(s, d, e.Request) {
		return phase3KaminoProjection{}, nil, 0, budgetHold("emergency_topup_funding_intent_invalid")
	}
	if err := validateFreshTopupPrincipal(ctx, rpc, s.TopupTranche.Loan, s, through); err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	slot, rawSlot, raw, err := observeAutoPartialRepaymentPrestateSlots(ctx, rpc, m, s, s.Slot)
	if err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	if rawSlot < s.Slot || slot < rawSlot || slot > through {
		return phase3KaminoProjection{}, nil, 0, budgetHold("emergency_topup_funding_expired")
	}
	if err = s.TopupTranche.Loan.validatePrincipal(raw, autoAUTOPYUSD, rawSlot); err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	if err = validateEmergencyTopupFundingEffects(m, s, d, e, raw); err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	if err = validateEmergencyFundingStrictRepay(raw, uint64(debtCashRaw(s))+e.Request.MinimumOutputRaw); err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	policySlot, policy, err := rpc.GetMultipleAccounts(ctx, []string{e.Request.Policy}, slot)
	if err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	if policySlot < slot || policySlot > through {
		return phase3KaminoProjection{}, nil, 0, budgetHold("emergency_topup_funding_expired")
	}
	slot = policySlot
	a := accountAt(policy, e.Request.Policy)
	if a.Owner != bridgeSquadsProgram || a.Executable || a.Lamports == 0 || sha256Bytes(a.Data) != e.Request.PolicyAccountDataSHA256 {
		return phase3KaminoProjection{}, nil, 0, budgetHold("emergency_topup_funding_policy_changed")
	}
	message, err := m.compileJupiterMessage(e.Request, mustKey(bridgeDelegate))
	if err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	addresses := payoffWindowAddresses(autoAUTOPYUSD, autoAUTOPYUSD.Kamino.Market)
	post, err := rpc.simulatePhase3EntryProjection(ctx, message, addresses, slot)
	if err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	if post.Slot < slot || post.Slot > through {
		return phase3KaminoProjection{}, nil, 0, budgetHold("emergency_topup_funding_expired")
	}
	if err = validateEmergencyFundingSwapProjection(raw, post, e); err != nil {
		return phase3KaminoProjection{}, nil, 0, err
	}
	// Price copies of ACTUAL raw reserves/principal and the enforceable minimum,
	// not a simulated reserve basis or optimistic swap output.
	var before []ConfirmedAccount
	post.Accounts = nil
	for _, address := range addresses {
		before = append(before, accountAt(raw, address))
		post.Accounts = append(post.Accounts, accountAt(raw, address))
	}
	post.Accounts = patchConfirmedTokenRaw(post.Accounts, autoAUTOPYUSD.CollateralCustody, 0)
	post.Accounts = patchConfirmedTokenRaw(post.Accounts, autoAUTOPYUSD.DebtCustody, uint64(debtCashRaw(s))+e.Request.MinimumOutputRaw)
	return post, before, rawSlot, nil
}

func observePhase3EmergencyTopupFundingAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s := o.Snapshot
	if !s.Fresh || s.Slot <= 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteKind != RouteKind || !emergencyTopupFundingRequest(s, d, e.Request) {
		return phase3BridgeAdmission{}, budgetHold("emergency_topup_funding_admission_unavailable")
	}
	proof, err := verifyDebtClearEmergency(m, o, d, "emergency-topup-funding", time.Now().UTC())
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if proof == nil {
		return phase3BridgeAdmission{}, budgetHold("emergency_topup_funding_fresh_risk_required")
	}
	through := s.Slot + observationLagSlots()
	post, before, beforeSlot, err := observeEmergencyTopupFundingPrestate(ctx, rpc, m, s, d, e, through)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, e.Request, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, e.Request, e.ExpectedEffects, current, post)
	if err != nil {
		return plan, err
	}
	upper, err := withdrawalUSDCExitEstimate(e.Request.QuotedOutputRaw)
	if err != nil {
		return plan, err
	}
	plan.EmergencyTopupFunding = &emergencyTopupFundingProof{BeforeSlot: beforeSlot, Before: before, Projection: post, RepaymentRaw: uint64(debtCashRaw(s)) + e.Request.MinimumOutputRaw, EstimatedSurplusRaw: upper - e.Request.MinimumOutputRaw}
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, through)
	if err = validateEmergencyTopupFundingProof(m, &plan); err != nil {
		return plan, err
	}
	if _, err = validateEmergencyTopupFundingPlan(ctx, rpc, m, &plan, current.ObservationSlot); err != nil {
		return plan, err
	}
	return plan, nil
}

// Shape and principal proof only. The caller MUST also verify current risk and
// the durable origin/ancestry under the route lock; JSON data grants no consent.
func validateEmergencyTopupFundingProof(m RouteManifest, p *phase3BridgeAdmission) error {
	if p == nil || p.EmergencyTopupFunding == nil || p.Input == nil {
		return budgetHold("emergency_topup_funding_proof_missing")
	}
	request, effects, message, err := p.Input.decodeWithManifest(m)
	r, ok := request.(JupiterSwapRequest)
	if err != nil || !ok || !emergencyTopupFundingRequest(p.Snapshot, p.Decision, r) {
		return budgetHold("emergency_topup_funding_proof_invalid")
	}
	proof := p.EmergencyTopupFunding
	upper, err := withdrawalUSDCExitEstimate(r.QuotedOutputRaw)
	if err != nil || upper < r.MinimumOutputRaw || proof.EstimatedSurplusRaw != upper-r.MinimumOutputRaw || proof.RepaymentRaw != uint64(debtCashRaw(p.Snapshot))+r.MinimumOutputRaw || proof.Projection.MessageSHA256 != sha256Bytes(message) || proof.Projection.UnitsConsumed == 0 || proof.BeforeSlot < p.Snapshot.Slot || proof.BeforeSlot > proof.Projection.Slot || proof.Projection.Slot < p.Snapshot.Slot || proof.Projection.Slot > p.ValidThroughSlot || p.Payoff == nil || p.ExitCycles < 1 || len(p.Exit) < 3 || p.Exit[0].Action != ReportNAV || p.Exit[1].Action != DeleverRouteStep || p.Exit[2].Action != ReportNAV {
		return budgetHold("emergency_topup_funding_proof_invalid")
	}
	if err = validateEmergencyFundingStrictRepay(proof.Projection.Accounts, proof.RepaymentRaw); err != nil {
		return err
	}
	// Historical raw prestate is retained separately. It never substitutes
	// for the fresh actual capture required by the execution fence.
	pre := proof.Before
	if err = validateTopupLoanCapture(proof.Projection.Accounts, autoAUTOPYUSD, proof.BeforeSlot); err != nil {
		return err
	}
	if len(pre) != len(proof.Projection.Accounts) {
		return budgetHold("emergency_topup_funding_proof_invalid")
	}
	for _, a := range pre {
		if a.Address == autoAUTOPYUSD.CollateralCustody || a.Address == autoAUTOPYUSD.DebtCustody {
			continue
		}
		b := accountAt(proof.Projection.Accounts, a.Address)
		if a.Owner != b.Owner || a.Lamports != b.Lamports || a.Executable != b.Executable || !bytes.Equal(a.Data, b.Data) {
			return budgetHold("emergency_topup_funding_principal_changed")
		}
	}
	if err = p.Snapshot.TopupTranche.Loan.validatePrincipal(pre, autoAUTOPYUSD, proof.BeforeSlot); err != nil {
		return err
	}
	if err = validateEmergencyTopupFundingEffects(m, p.Snapshot, p.Decision, JupiterExecutionEvidence{r, effects}, pre); err != nil {
		return err
	}
	if err = validateEmergencyFundingSwapProjection(pre, proof.Projection, JupiterExecutionEvidence{r, effects}); err != nil {
		return err
	}
	request, repayEffects, _, err := p.Exit[1].Template.decodeWithManifest(m)
	repay, ok := request.(KaminoPrimeUSDCRequest)
	if err != nil || !ok || repay.FullPayoff || repay.RepaymentRelease || repay.AmountRaw != proof.RepaymentRaw || repay.RouteLane != autoAUTOPYUSD.Lane || len(repay.ObligationReserves) != 2 || repay.ObligationReserves[0] != autoAUTOPYUSD.Kamino.CollateralReserve || repay.ObligationReserves[1] != autoAUTOPYUSD.Kamino.DebtReserve || repayEffects.Repayment == nil || repayEffects.Repayment.MinimumDebitRaw != proof.RepaymentRaw || repayEffects.Repayment.MaximumDebitRaw != proof.RepaymentRaw {
		return budgetHold("emergency_topup_funding_repay_invalid")
	}
	if _, leg, err := kaminoPrimeUSDCInstruction(repay); err != nil || leg != kaminoLegRepay {
		return budgetHold("emergency_topup_funding_repay_invalid")
	}
	return nil
}

func validateEmergencyTopupFundingPlan(ctx context.Context, rpc *RPCClient, m RouteManifest, p *phase3BridgeAdmission, slot int64) (int64, error) {
	if err := validateEmergencyTopupFundingProof(m, p); err != nil {
		return 0, err
	}
	if slot > p.ValidThroughSlot {
		return 0, budgetHold("emergency_topup_funding_expired")
	}
	request, effects, _, err := p.Input.decodeWithManifest(m)
	if err != nil {
		return 0, err
	}
	s := p.Snapshot
	s.Slot = max(s.Slot, slot)
	fresh, _, _, err := observeEmergencyTopupFundingPrestate(ctx, rpc, m, s, p.Decision, JupiterExecutionEvidence{request.(JupiterSwapRequest), effects}, p.ValidThroughSlot)
	if err != nil {
		return 0, err
	}
	if err = validatePilotReleaseProjection(p, p.EmergencyTopupFunding.Projection, fresh, autoAUTOPYUSD); err != nil {
		return 0, err
	}
	return fresh.Slot, nil
}
