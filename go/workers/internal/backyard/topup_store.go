package backyard

import (
	"context"
	"encoding/json"
	"math"

	"github.com/jackc/pgx/v5"
)

func sameTopupTranche(a, b *topupTranche) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func topupWorkInFlight(t *topupTranche) bool {
	return t != nil && t.Stage != topupTrancheComplete && t.Stage != topupTrancheOrdinary
}

// Completed residue remains receipted collateral, not a lock on independently
// authorized borrowing, withdrawal or risk protection.
func topupOwnedInventory(t *topupTranche) bool { return topupWorkInFlight(t) }

func (d *Database) LoadTopupTranche(ctx context.Context, key string) (*topupTranche, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'topupTranche' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
		return nil, err
	}
	t, err := decodeTopupTranche(raw)
	if err != nil || t == nil {
		return t, err
	}
	if err = d.validateTopupTrancheOrigin(ctx, d.pool, key, raw); err != nil {
		return nil, err
	}
	return t, nil
}

// Latest inventory and the original loan must both match immutable finalized
// operation evidence. A route-JSON amount/reason alone establishes no ownership.
func (d *Database) validateTopupTrancheOrigin(ctx context.Context, q partialWithdrawalQuerier, key string, raw []byte) error {
	var valid bool
	err := q.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM loyal_yield.multiply_route_states s
 JOIN loyal_yield.multiply_operations last ON last.operation_id=s.state->'topupTranche'->>'lastOperationId'
 JOIN loyal_yield.multiply_operations origin ON origin.operation_id=s.state->'topupTranche'->>'originOperationId'
 WHERE s.route_key=$1 AND s.state->'topupTranche'=$2::jsonb
 AND (s.state->'topupTranche'->>'generation')::bigint<=s.state_version
 AND last.route_key=s.route_key AND origin.route_key=s.route_key
 AND last.strategy_key=s.state->'topupTranche'->>'lane' AND origin.strategy_key=last.strategy_key
 AND last.status='reconciled' AND last.confirmation_status='finalized'
 AND origin.status='reconciled' AND origin.confirmation_status='finalized'
 AND last.confirmed_slot=(s.state->'topupTranche'->>'lastSlot')::bigint
 AND last.reconciliation_sha256=s.state->'topupTranche'->>'lastEffectsSha256'
 AND last.expected_effects->'phase3'->'topupResult'=s.state->'topupTranche'
 AND last.expected_effects->'phase3'->>'goalId'=$3
 AND origin.expected_effects->'phase3'->>'goalId'=$3
 AND origin.action=$4 AND origin.expected_effects->'decision'->>'reason'=$5
 AND origin.expected_effects->'phase3'->'topup'->>'originOperationId'=origin.operation_id
 AND origin.expected_effects->'phase3'->'topup'->'loan'=s.state->'topupTranche'->'loan'
 AND origin.expected_effects->'phase3'->'topup'->'allocatedUsdcRaw'=s.state->'topupTranche'->'allocatedUsdcRaw'
 AND last.signed_wire IS NOT NULL AND origin.signed_wire IS NOT NULL
 AND last.expected_effects->'phase3'->>'signedWireSha256'=last.signed_wire_sha256
 AND origin.expected_effects->'phase3'->>'signedWireSha256'=origin.signed_wire_sha256)`, key, string(raw), Phase3GoalID, string(VoltrAllocateToSquads), topupAllocationReason).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return budgetHold("topup_origin_changed")
	}
	return nil
}

type topupOperationState struct {
	key              string
	generation       int64
	current, decided *topupTranche
	auth             phase3OperationAuthorization
	action           Action
}

func (d *Database) readTopupOperationTx(ctx context.Context, tx pgx.Tx, id string) (topupOperationState, error) {
	var out topupOperationState
	if err := d.lockOperationLease(ctx, tx, id); err != nil {
		return out, err
	}
	var current, decided, auth []byte
	if err := tx.QueryRow(ctx, `SELECT s.route_key,s.state_version,s.state->'topupTranche',o.expected_effects->'topupTranche',COALESCE(o.expected_effects->'phase3','{}'::jsonb),COALESCE(o.action,'')
 FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE o.operation_id=$1`, id).Scan(&out.key, &out.generation, &current, &decided, &auth, &out.action); err != nil {
		return out, err
	}
	var err error
	out.current, err = decodeTopupTranche(current)
	if err != nil {
		return out, err
	}
	out.decided, err = decodeTopupTranche(decided)
	if err != nil || json.Unmarshal(auth, &out.auth) != nil {
		return out, budgetHold("invalid_topup_operation")
	}
	if out.current != nil {
		if out.current.Generation > out.generation {
			return out, budgetHold("topup_generation_changed")
		}
		if err = d.validateTopupTrancheOrigin(ctx, tx, out.key, current); err != nil {
			return out, err
		}
	}
	return out, nil
}

func topupPartialHandoff(plan *phase3BridgeAdmission) bool {
	if plan == nil {
		return false
	}
	a, reason, amount, ok := partialWithdrawalStep(plan.Snapshot)
	return ok && a == plan.Decision.Action && reason == plan.Decision.Reason && amount == plan.Decision.AmountRaw
}

// Only an already verified, coherent on-chain emergency proof may carry an
// active tranche into risk protection. This creates no full-repayment consent;
// the operation's existing admission/build/send authority is still required.
func topupRiskHandoff(plan *phase3BridgeAdmission, proof *debtClearRiskProof, operationID string) bool {
	if plan == nil || proof == nil {
		return false
	}
	s := plan.Snapshot
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	if proof.OperationID != operationID || proof.ObservationID != s.ObservationID || proof.Slot != s.Slot || proof.ObservedAt.IsZero() ||
		!sha256Pattern.MatchString(proof.AccountsSHA256) || proof.ValuationSource != s.ValuationSource ||
		(proof.ValuationSource != "confirmed" && proof.ValuationSource != routeRefreshValuationSource) ||
		!s.Fresh || !s.HasPosition || s.PositionDebtRaw <= 0 || s.LiquidationThresholdBPS > 10000 || hard <= TargetLTVBPS ||
		proof.HardLTVBPS != hard || proof.LTVBPS != s.LTVBPS || s.LTVBPS < hard || !decisionsEqual(Decide(s), plan.Decision) {
		return false
	}
	switch plan.Decision.Reason {
	case "hard_ltv_repay", "hard_ltv_buffer_swap", "hard_ltv_usdc_repayment_buffer":
		return true
	default:
		return false
	}
}

// Destination is an ACTUAL admitted operation's intent, not a caller-provided
// SHA. Its existing partial-withdrawal/debt-clear admission remains decisive.
func (d *Database) validateTopupHandoffOrigin(ctx context.Context, q partialWithdrawalQuerier, key string, destination topupHandoffAuthority) error {
	if !destination.valid() {
		return budgetHold("topup_handoff_authority_missing")
	}
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations
 WHERE operation_id=$1 AND route_key=$2 AND expected_effects->'phase3'->>'goalId'=$3
 AND expected_effects->'phase3'->>'intentSha256'=$4
 AND COALESCE((expected_effects->'phase3'->>'reservationReleased')::boolean,false)=false`, destination.OriginOperationID, key, Phase3GoalID, destination.AuthoritySHA256).Scan(&raw); err != nil {
		return budgetHold("topup_handoff_authority_changed")
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(raw, &auth) != nil || auth.Topup == nil || auth.Topup.Handoff != destination || auth.BridgeAdmission == nil ||
		(auth.DebtClear == nil && !topupPartialHandoff(auth.BridgeAdmission) && !topupRiskHandoff(auth.BridgeAdmission, auth.TopupRisk, destination.OriginOperationID)) {
		return budgetHold("topup_handoff_authority_changed")
	}
	return nil
}

// Shared durable fence at admission/build/send; signed recovery keeps the same
// bound predecessor. Routine route generation advances cannot renew inventory.
func (d *Database) authorizeTopupStateTx(ctx context.Context, tx pgx.Tx, id string, admission bool) error {
	s, err := d.readTopupOperationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if !sameTopupTranche(s.current, s.decided) {
		return budgetHold("topup_predecessor_changed")
	}
	if s.auth.Topup == nil {
		if !admission && topupOwnedInventory(s.current) {
			return budgetHold("topup_binding_missing")
		}
		return nil
	}
	b := s.auth.Topup
	if !b.matches(s.current) || !b.Loan.valid() || b.Lane != autoAUTOPYUSD.Lane {
		return budgetHold("topup_predecessor_changed")
	}
	if s.action == VoltrAllocateToSquads {
		if b.OriginOperationID != id || topupWorkInFlight(s.current) {
			return budgetHold("topup_allocation_already_in_flight")
		}
	} else if s.current == nil || b.OriginOperationID != s.current.OriginOperationID || b.Loan != s.current.Loan || b.AllocatedUSDCRaw != s.current.AllocatedUSDCRaw {
		return budgetHold("topup_origin_changed")
	}
	if b.Handoff.valid() {
		return d.validateTopupHandoffOrigin(ctx, tx, s.key, b.Handoff)
	}
	if s.current != nil && s.current.Stage == topupTrancheHandoff {
		return budgetHold("topup_handoff_authority_missing")
	}
	return nil
}

func topupCapitalReason(d Decision) bool {
	return d.Reason == topupAllocationReason || d.Reason == topupSwapReason || d.Reason == topupDepositReason
}

func (d *Database) bindTopupAdmissionTx(ctx context.Context, tx pgx.Tx, rpc *RPCClient, id string, o Observation, decision Decision, plan phase3BridgeAdmission, auth *phase3OperationAuthorization, intent string, risk *debtClearRiskProof) error {
	s, err := d.readTopupOperationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if !sameTopupTranche(s.current, s.decided) || !sameTopupTranche(s.current, o.Snapshot.TopupTranche) {
		return budgetHold("topup_predecessor_changed")
	}
	newAllocation := decision.Action == VoltrAllocateToSquads && decision.Reason == topupAllocationReason && o.Snapshot.PositionDebtRaw > 0
	if auth.Topup != nil {
		// Once admitted, retries NEVER recapture the finalized origin.
		if !auth.Topup.matches(s.current) {
			return budgetHold("topup_predecessor_changed")
		}
		if topupCapitalReason(decision) {
			return validateFreshTopupPrincipal(ctx, rpc, auth.Topup.Loan, o.Snapshot, plan.ValidThroughSlot)
		}
		return nil
	}
	if !newAllocation && !topupOwnedInventory(s.current) {
		return nil
	}
	b := &topupTrancheBinding{Before: s.current}
	if newAllocation {
		if (s.current != nil && s.current.Stage != topupTrancheComplete) || decision.StrategyKey != autoAUTOPYUSD.Lane || decision.AmountRaw <= 0 || plan.Payoff == nil || !o.Snapshot.PilotActive {
			return budgetHold("topup_allocation_binding_unavailable")
		}
		carry := uint64(0)
		if s.current != nil {
			carry = s.current.CollateralRemainingRaw
		}
		if o.Snapshot.SquadsIdleRaw != 0 || o.Snapshot.DebtIdleRaw != 0 || o.Snapshot.VoltrStrategyIdleRaw != 0 || o.Snapshot.CollateralIdleRaw < 0 || uint64(o.Snapshot.CollateralIdleRaw) != carry {
			return budgetHold("topup_allocation_custody_changed")
		}
		if plan.topupOrigin == nil || !plan.topupOrigin.valid() || plan.topupOrigin.ObservedSlot > o.Snapshot.Slot {
			return budgetHold("topup_requires_prepricing_finalized_origin")
		}
		b.Loan, b.Lane, b.OriginOperationID, b.AllocatedUSDCRaw = *plan.topupOrigin, decision.StrategyKey, id, uint64(decision.AmountRaw)
	} else {
		b.Loan, b.Lane, b.OriginOperationID, b.AllocatedUSDCRaw = s.current.Loan, s.current.Lane, s.current.OriginOperationID, s.current.AllocatedUSDCRaw
		if decision.Action == ReportNAV {
			b.Handoff = s.current.Handoff
		} else if !topupCapitalReason(decision) {
			if s.current.Handoff.valid() {
				b.Handoff = s.current.Handoff
			} else {
				if auth.DebtClear == nil && !topupPartialHandoff(&plan) {
					if !topupRiskHandoff(&plan, risk, id) {
						return budgetHold("topup_handoff_requires_existing_authority")
					}
					proof := *risk
					auth.TopupRisk = &proof
				}
				b.Handoff = topupHandoffAuthority{OriginOperationID: id, AuthoritySHA256: intent}
			}
		}
	}
	if topupCapitalReason(decision) {
		switch decision.Reason {
		case topupAllocationReason:
			if !newAllocation {
				return budgetHold("topup_allocation_binding_unavailable")
			}
		case topupSwapReason:
			if decision.Action != SwapStableToCollateralStep || s.current == nil || s.current.Stage != topupTrancheAllocated || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) != s.current.USDCRemainingRaw || o.Snapshot.CollateralIdleRaw < 0 || uint64(o.Snapshot.CollateralIdleRaw) != s.current.CollateralRemainingRaw {
				return budgetHold("topup_swap_binding_changed")
			}
		case topupDepositReason:
			if decision.Action != OpenRouteStep || s.current == nil || s.current.Stage != topupTrancheCollateral || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) != s.current.CollateralRemainingRaw || o.Snapshot.SquadsIdleRaw != 0 {
				return budgetHold("topup_deposit_binding_changed")
			}
		}
		if s.current != nil && s.current.Stage == topupTrancheHandoff {
			return budgetHold("topup_handoff_in_progress")
		}
		if err = validateFreshTopupPrincipal(ctx, rpc, b.Loan, o.Snapshot, plan.ValidThroughSlot); err != nil {
			return err
		}
	}
	auth.Topup = b
	return nil
}

func validateFreshTopupPrincipal(ctx context.Context, rpc *RPCClient, loan topupLoan, snapshot Snapshot, throughSlot int64) error {
	if rpc == nil {
		return budgetHold("topup_principal_read_unavailable")
	}
	pin := reviewedTopupKaminoIdentity()
	addresses := append([]string{pin.program, pin.programData}, payoffWindowAddresses(autoAUTOPYUSD, bridgeSquadsATA, bridgeStrategyATA)...)
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, snapshot.Slot, nil, "confirmed")
	if err != nil {
		return err
	}
	if slot > throughSlot {
		return budgetHold("stale_topup_admission")
	}
	if err = validateTopupKaminoCapability(accounts, slot); err != nil {
		return err
	}
	if err = loan.validatePrincipal(accounts, autoAUTOPYUSD, slot); err != nil {
		return err
	}
	bound, err := decodeKaminoPayoffBound(accounts, autoAUTOPYUSD, slot)
	if err != nil || snapshot.PositionCollateralRaw <= 0 || uint64(snapshot.PositionCollateralRaw) != loan.CollateralRaw || !sameAccruingDebt(bound, snapshot.PositionDebtRaw) {
		return budgetHold("topup_snapshot_position_changed")
	}
	for _, row := range []struct {
		address, mint, owner, program string
		raw                           int64
	}{
		{bridgeSquadsATA, bridgeUSDC, bridgeVault, bridgeTokenProgram, snapshot.SquadsIdleRaw},
		{autoAUTOPYUSD.CollateralCustody, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, autoAUTOPYUSD.CollateralTokenProgram, snapshot.CollateralIdleRaw},
		{bridgeStrategyATA, bridgeUSDC, bridgeStrategyAuth, bridgeTokenProgram, snapshot.VoltrStrategyIdleRaw},
	} {
		a := accountAt(accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(row.owner)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Owner != row.program || a.Executable || a.Lamports == 0 || row.raw < 0 || cash.Raw != uint64(row.raw) {
			return budgetHold("topup_snapshot_custody_changed")
		}
	}
	return nil
}

// Runs after verified finalized receipt reconciliation and budget settlement,
// inside the SAME existing route-locked transaction. Failure rolls both back.
func (d *Database) settleTopupTrancheTx(ctx context.Context, tx pgx.Tx, id string, effects ExpectedEffects, receipt ConfirmedTransactionEvidence) error {
	s, err := d.readTopupOperationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = d.authorizeTopupStateTx(ctx, tx, id, false); err != nil {
		return err
	}
	if s.auth.Topup == nil && s.current == nil {
		return nil
	}
	if s.auth.GoalID != Phase3GoalID || s.auth.BookedSpentMicros <= 0 || s.auth.ReservationReleased || !receipt.Finalized {
		return budgetHold("topup_settlement_unproven")
	}
	b := s.auth.Topup
	var next *topupTranche
	if b == nil {
		next, err = reconcileOrdinaryTopupCollateral(*s.current, id, effects, receipt)
		if err != nil || next == nil {
			return err
		}
	} else if s.action == ReportNAV {
		if effects.Kind != "bridge" || effects.Deposit != nil || effects.Repayment != nil {
			return budgetHold("topup_nav_mutation_unproven")
		}
		for _, e := range effects.Accounts {
			if e.BeforeRaw != e.AfterRaw || e.MinimumAfterRaw != nil {
				return budgetHold("topup_nav_mutation_unproven")
			}
		}
		next = s.current // no inventory transition, generation or handoff from NAV
	} else if b.Handoff.valid() {
		if s.current == nil {
			return budgetHold("topup_handoff_inventory_unavailable")
		}
		next, err = reconcileTopupHandoff(*s.current, *b, b.Handoff, id, s.action, effects, receipt)
	} else {
		next, err = reconcileTopupTranche(s.current, *b, id, s.action, effects, receipt)
	}
	if err != nil {
		return err
	}
	if next == nil {
		return budgetHold("topup_settlement_unproven")
	}
	if s.action != ReportNAV {
		if s.generation <= 0 || s.generation == math.MaxInt64 {
			return budgetHold("topup_generation_changed")
		}
		next.Generation = s.generation + 1
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,topupResult}',$2::jsonb,true) WHERE operation_id=$1 AND status='reconciled' AND confirmation_status='finalized'`, id, string(raw)); err != nil {
		return err
	}
	if s.action == ReportNAV {
		return nil
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(state,'{topupTranche}',$4::jsonb,true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp()
 WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND state_version=$5`, s.key, lease.Owner, lease.FencingToken, string(raw), s.generation)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRouteLeaseLost
	}
	if b != nil && b.Handoff.OriginOperationID == id && s.auth.DebtClear == nil && topupPartialHandoff(s.auth.BridgeAdmission) {
		var existing []byte
		if err = tx.QueryRow(ctx, `SELECT state->'partialWithdrawal' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, s.key).Scan(&existing); err != nil {
			return err
		}
		partial, err := decodePartialWithdrawal(existing)
		if err != nil {
			return err
		}
		if partial == nil {
			target, ok := partialWithdrawalTargetLTVBPS(s.auth.BridgeAdmission.Snapshot)
			if !ok || target > leverageMaxLTVBPS {
				return budgetHold("topup_partial_handoff_target_unavailable")
			}
			encoded, _ := json.Marshal(partialWithdrawalState{Lane: next.Lane, OperationID: id, Generation: next.Generation, LTVBPS: target})
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{partialWithdrawal}',$2::jsonb,true) WHERE route_key=$1`, s.key, string(encoded)); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{partialWithdrawal}',$2::jsonb,true) WHERE operation_id=$1`, id, string(encoded)); err != nil {
				return err
			}
		}
	}
	return nil
}
