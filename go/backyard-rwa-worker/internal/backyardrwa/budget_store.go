package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type phase3OperationAuthorization struct {
	PilotAuthorityID          string                  `json:"pilotAuthorityId,omitempty"`
	BookedExecutionCostMicros int64                   `json:"bookedExecutionCostMicros,omitempty"`
	GoalID                    string                  `json:"goalId"`
	IntentSHA256              string                  `json:"intentSha256"`
	SignedWireSHA256          string                  `json:"signedWireSha256,omitempty"`
	ReservationReleased       bool                    `json:"reservationReleased,omitempty"`
	BookedSpentMicros         int64                   `json:"bookedSpentMicros,omitempty"`
	BuildInput                *phase3BuildInput       `json:"buildInput,omitempty"`
	SendKnownCost             *ValuedTransactionCost  `json:"sendKnownCost,omitempty"`
	BridgeAdmission           *phase3BridgeAdmission  `json:"bridgeAdmission,omitempty"`
	PolicySetup               *policySetupObservation `json:"policySetup,omitempty"`
	PolicySetupCompletion     *policySetupCompletion  `json:"policySetupCompletion,omitempty"`
	SetupBuildCost            *ValuedTransactionCost  `json:"setupBuildCost,omitempty"`
	SetupCompletionCost       *ValuedTransactionCost  `json:"setupCompletionCost,omitempty"`
	// CustodyProof is the durable pre-decision shared-custody ownership
	// binding (doc 26): persisted by the shared locked admission for a
	// positive AUTO-PYUSD spend and re-required by the build and
	// broadcast-intent fences. Nil for every other lane and zero-spend
	// operation (installed behavior unchanged).
	CustodyProof *sharedCustodyProofBinding `json:"custodyProof,omitempty"`
}

// Preserve an admission failure before restart recovery can replace it with a
// generic reason. Only never-submitted states may release their reservation;
// the transition's lease and status CAS protect against concurrent progress.
func (d *Database) RecordPhase3BudgetHold(ctx context.Context, operationID string, hold *BudgetHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == "" {
		return fmt.Errorf("invalid budget hold journal input")
	}
	var status OperationStatus
	if err := d.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&status); err != nil {
		return err
	}
	if status != Decided && status != Built && status != Simulated {
		return budgetHold("budget_hold_requires_never_submitted_operation")
	}
	encoded, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	return d.transition(ctx, operationID, status, Failed,
		`, recovery_reason = $4, expected_effects = jsonb_set(expected_effects, '{budgetHold}', $5::jsonb)`,
		"phase3_budget_hold:"+hold.Reason, string(encoded))
}

// Book the admitted upper bound only after finalized effect reconciliation.
// This intentionally never refunds quote/fee slack without separate economic
// proof. Principal returning to custody does not reduce this gross counter.
func (d *Database) settlePhase3ReservationTx(ctx context.Context, tx pgx.Tx, operationID string) error {
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	var finalized bool
	var wire []byte
	if err = tx.QueryRow(ctx, `SELECT status='reconciled' AND confirmation_status='finalized' AND reconciled_effects IS NOT NULL,signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&finalized, &wire); err != nil {
		return err
	}
	if !finalized || auth.GoalID != Phase3GoalID || auth.ReservationReleased || auth.BookedSpentMicros != 0 || len(wire) == 0 || auth.SignedWireSHA256 != sha256Bytes(wire) {
		return budgetHold("unproven_budget_settlement")
	}
	reservation, ok := budget.Reservations[operationID]
	if !ok {
		return budgetHold("unreserved_reconciliation")
	}
	if err = budget.Settle(operationID, auth.IntentSHA256, reservation.UpperMicros); err != nil {
		return err
	}
	auth.BookedSpentMicros = reservation.UpperMicros
	auth.BookedExecutionCostMicros = reservation.ExecutionCostUpperMicros
	return d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth)
}

// Called inside the same transaction that marks a provably unspent operation
// failed. Unadmitted operations can fail without an initialized goal budget;
// a one-sided reservation/authorization is corruption, not permission to reset.
func (d *Database) releasePhase3UnspentTx(ctx context.Context, tx pgx.Tx, operationID string) error {
	var hasAuth, hasReservation bool
	if err := tx.QueryRow(ctx, `SELECT operation.expected_effects ? 'phase3',
	 COALESCE((route.state->'phase3'->'reservations') ? operation.operation_id,false)
	 FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route ON route.route_key=operation.route_key
	 WHERE operation.operation_id=$1`, operationID).Scan(&hasAuth, &hasReservation); err != nil {
		return err
	}
	if !hasAuth && !hasReservation {
		return nil
	}
	if !hasAuth || !hasReservation {
		return budgetHold("incoherent_reservation_release")
	}
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if auth.GoalID != Phase3GoalID || auth.ReservationReleased {
		return budgetHold("incoherent_reservation_release")
	}
	if err = budget.releaseUnspent(operationID, auth.IntentSHA256); err != nil {
		return err
	}
	auth.ReservationReleased = true
	return d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth)
}

// Phase3IntentDigest binds the exact production request and expected effects,
// including blockhash, maximum amounts, account graph and policy identities.
// A rebuilt request needs new admission; it cannot inherit another wire's cap.
func Phase3IntentDigest(request any, effects []byte) (string, error) {
	if !json.Valid(effects) {
		return "", budgetHold("invalid_intent_effects")
	}
	encoded, err := json.Marshal(struct {
		Request any             `json:"request"`
		Effects json.RawMessage `json:"effects"`
	}{request, json.RawMessage(effects)})
	if err != nil {
		return "", fmt.Errorf("encode budget intent: %w", err)
	}
	return sha256Bytes(encoded), nil
}

// The route lock is the same lock used by RecordDecision, not a second lease
// or table. Callers must hold it until their authorization/write commits.
func (d *Database) readPhase3BudgetTx(ctx context.Context, tx pgx.Tx, operationID string) (Phase3Budget, phase3OperationAuthorization, error) {
	if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
		return Phase3Budget{}, phase3OperationAuthorization{}, err
	}
	var budgetBytes, authBytes, activationBytes []byte
	var stateVersion int64
	var lane string
	err := tx.QueryRow(ctx, `SELECT COALESCE(route.state->'phase3','null'::jsonb), COALESCE(operation.expected_effects->'phase3','null'::jsonb),COALESCE(operation.strategy_key,''),COALESCE(route.state->'pilotBudgetActivation','null'::jsonb),route.state_version
		FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route ON route.route_key=operation.route_key
		WHERE operation.operation_id=$1`, operationID).Scan(&budgetBytes, &authBytes, &lane, &activationBytes, &stateVersion)
	if err != nil {
		return Phase3Budget{}, phase3OperationAuthorization{}, err
	}
	var budget Phase3Budget
	var auth phase3OperationAuthorization
	if json.Unmarshal(budgetBytes, &budget) != nil || json.Unmarshal(authBytes, &auth) != nil {
		return budget, auth, budgetHold("invalid_durable_budget")
	}
	if err := budget.validate(); err != nil {
		return budget, auth, err
	}
	if budget.Pilot != nil {
		if _, err := validatePersistedPilotActivation(budget, activationBytes, stateVersion); err != nil {
			return budget, auth, err
		}
		if auth.GoalID != "" && auth.PilotAuthorityID != budget.Pilot.AuthorityID {
			return budget, auth, budgetHold("pilot_operation_authority_mismatch")
		}
	} else if auth.PilotAuthorityID != "" || auth.BookedExecutionCostMicros != 0 {
		return budget, auth, budgetHold("pilot_operation_without_authority")
	}
	if reservation, exists := budget.Reservations[operationID]; exists &&
		phase3BudgetFamilyForLane(lane) != reservation.Family {
		return budget, auth, budgetHold("reservation_family_does_not_match_journal_lane")
	}
	return budget, auth, nil
}

func (d *Database) writePhase3BudgetTx(ctx context.Context, tx pgx.Tx, operationID string, budget Phase3Budget, auth phase3OperationAuthorization) error {
	budgetBytes, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	authBytes, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(state,'{phase3}',$4::jsonb,true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp()
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, lease.RouteKey, lease.Owner, lease.FencingToken, string(budgetBytes))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRouteLeaseLost
	}
	result, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3}',$2::jsonb,true),updated_at=clock_timestamp() WHERE operation_id=$1`, operationID, string(authBytes))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("budget operation disappeared")
	}
	return nil
}

// ReservePhase3 persists both sides of admission atomically. A producer must
// derive r from fresh, checked executable debits/fees and the complete exit
// graph. Missing persisted goal state is a HOLD, never an implicit reset.
func (d *Database) ReservePhase3(ctx context.Context, r BudgetReservation) error {
	if _, err := d.currentLease(); err != nil {
		return err
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, r.OperationID)
	if err != nil {
		return err
	}
	if budget.Pilot != nil {
		return budgetHold("pilot_requires_measured_execution_admission")
	}
	var status, lane string
	if err = tx.QueryRow(ctx, `SELECT status,COALESCE(strategy_key,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, r.OperationID).Scan(&status, &lane); err != nil {
		return err
	}
	if status != string(Decided) {
		return budgetHold("admission_after_construction")
	}
	if family := phase3BudgetFamilyForLane(lane); family == "" || family != r.Family {
		return budgetHold("reservation_family_does_not_match_journal_lane")
	}
	if auth.GoalID != "" && (auth.GoalID != Phase3GoalID || auth.IntentSHA256 != r.IntentSHA256) {
		return budgetHold("reservation_identity_mismatch")
	}
	if err = budget.Admit(r); err != nil {
		return err
	}
	auth = phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: r.IntentSHA256}
	if budget.Pilot != nil {
		auth.PilotAuthorityID = budget.Pilot.AuthorityID
	}
	if err = d.writePhase3BudgetTx(ctx, tx, r.OperationID, budget, auth); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Production bridge admission adds a measured reservation; it does not replace
// authorizePhase3Build, the pinned signer check, or the final-send cost fence.
// Existing caps, goal identity and missing-budget HOLD remain unchanged.
func (d *Database) admitPhase3Bridge(ctx context.Context, rpc *RPCClient, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
	plan, err := observePhase3BridgeAdmission(ctx, rpc, observation, decision, evidence)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}

// Shared durable boundary for measured paths that terminate at bridge idle.
// The historical JSON field name remains bridgeAdmission; its Input.Kind binds
// the current action (bridge or Kamino), not the type of the whole return graph.
// The public form loads the embedded reviewed manifest exactly once and is
// byte-identical to the installed behavior.
func (d *Database) persistPhase3ExitAdmission(ctx context.Context, rpc *RPCClient, operationID string, observation Observation, decision Decision, plan phase3BridgeAdmission) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, operationID, observation, decision, plan)
}

// persistPhase3ExitAdmissionOnManifest is the identical locked admission with
// every embedded resolution made explicit through the same manifest: the
// build-input decode, the pilot execution-cost classification, the selector
// entry authority (which allocates the candidate AUTO entry only while the
// reviewed binding resolves), the reserved-cost validation and the initializer
// snapshot/decision rechecks. Installed lanes resolve identically through the
// embedded manifest.
func (d *Database) persistPhase3ExitAdmissionOnManifest(ctx context.Context, rpc *RPCClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, plan phase3BridgeAdmission) error {
	if d == nil || d.pool == nil {
		return budgetHold("bridge_admission_database_unavailable")
	}
	request, effects, _, err := plan.Input.decodeWithManifest(manifest)
	if err != nil {
		return err
	}
	intent, err := Phase3IntentDigest(request, plan.Input.Effects)
	if err != nil {
		return err
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if err = budget.validateExitPlanCaps(plan); err != nil {
		return err
	}
	if err = budget.validatePilotReleaseAuthority(request); err != nil {
		return err
	}
	if plan.Snapshot.PilotActive && budget.Pilot == nil {
		return budgetHold("pilot_planning_authority_required")
	}
	if budget.Pilot != nil {
		plan.CurrentCost, err = manifest.observePilotExecutionCost(ctx, rpc, request, effects, plan.CurrentCost)
		if err != nil {
			return err
		}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, plan.CurrentCost.ValidThroughSlot)
	}
	var status, lane, routeKey, action, lastReconciledAction string
	var durableUnwind bool
	var decisionBytes []byte
	if err = tx.QueryRow(ctx, `SELECT op.status,COALESCE(op.strategy_key,''),COALESCE(op.route_key,''),COALESCE(op.action,''),op.expected_effects->'decision',
	 COALESCE((SELECT prior.action FROM loyal_yield.multiply_operations prior WHERE prior.route_key=op.route_key AND prior.strategy_key=op.strategy_key AND prior.status='reconciled' ORDER BY prior.confirmed_slot DESC NULLS LAST,prior.updated_at DESC,prior.operation_id DESC LIMIT 1),''),
	 COALESCE(route.state->'selectorUnwind','null'::jsonb)<>'null'::jsonb
	 FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE op.operation_id=$1`, operationID).Scan(&status, &lane, &routeKey, &action, &decisionBytes, &lastReconciledAction, &durableUnwind); err != nil {
		return err
	}
	var recorded decisionEvidence
	if json.Unmarshal(decisionBytes, &recorded) != nil || status != string(Decided) || lane != decision.StrategyKey || action != string(decision.Action) ||
		recorded.StrategyKey != lane || recorded.AmountRaw != decision.AmountRaw || recorded.Reason != decision.Reason ||
		recorded.ObservationID != observation.Snapshot.ObservationID || recorded.ObservationSlot > observation.Snapshot.Slot || recorded.ObservationSlot <= 0 {
		return budgetHold("bridge_admission_journal_mismatch")
	}
	// Recheck time after acquiring the existing route lock. Contention cannot
	// promote an expired observation into a fresh authorization.
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return err
	}
	if slot < plan.CurrentCost.ObservationSlot || slot > plan.ValidThroughSlot {
		return budgetHold("stale_bridge_admission_snapshot")
	}
	// Shared locked-admission custody seam (doc 26 §2): a FIRST admission of a
	// positive prepared AUTO-PYUSD spend requires the strict pre-decision
	// proof carried on this admission's own observation, re-validated under the
	// route lock. This must run BEFORE the selector-entry write below so the
	// proof's generation is still the lock's when the fence compares it. A
	// retry never binds a carried proof — no fresh ownership proof is
	// obtainable once the decided row exists — and instead re-validates the
	// PERSISTED admitted binding under the current lease/generation in the
	// retry branch below. Every other lane and zero-spend operation binds
	// nothing here.
	var custody sharedCustodyProofBinding
	if auth.GoalID == "" {
		custody, err = bindSharedCustodyAdmissionProofOnManifest(ctx, tx, routeKey, lane, observation, effects)
		if err != nil {
			return err
		}
	}
	if err = d.authorizeSelectorEntryTxOnManifest(ctx, manifest, tx, operationID, budget, request, effects, slot, true); err != nil {
		return err
	}
	if auth.GoalID != "" {
		// Retry preserves all prior authorization and wire identity.
		if auth.GoalID != Phase3GoalID || auth.IntentSHA256 != intent || auth.BridgeAdmission == nil || auth.ReservationReleased {
			return budgetHold("reservation_identity_mismatch")
		}
		if err = budget.AuthorizeIntent(operationID, intent); err != nil {
			return err
		}
		reserved := budget.Reservations[operationID]
		if plan.CurrentCost.TotalMicros > reserved.UpperMicros || plan.ExitAfterMicros > reserved.ExitAfterMicros {
			return budgetHold("fresh_bridge_cost_exceeds_reservation")
		}
		if err = manifest.validateReservedExecutionCost(budget, reserved, request, effects, plan.CurrentCost); err != nil {
			return err
		}
		// Reachable retry semantics (doc 26 §2): the decided row blocks any
		// fresh ownership proof, so this re-arms the EXACT binding the first
		// measured admission persisted — same effects digest, spend, custody
		// and lease identity — against the CURRENT lock row. No generation is
		// consumed and the reservation is not replenished: the retry commits
		// without a phase3 write. History identity is preserved: the journal
		// evidence recheck above still pins the recorded observation, and the
		// ownership proof stays strict about nonterminal rows.
		if err = validatePersistedSharedCustodyBindingOnManifest(ctx, tx, routeKey, lane, effects, auth.CustodyProof); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	family := phase3BudgetFamilyForLane(lane)
	for other, row := range budget.Families {
		if other != family && row.ExitMicros != 0 {
			return budgetHold("another_family_has_reserved_exit")
		}
	}
	recovery := decision.Action != VoltrAllocateToSquads
	if decision.Reason == "hard_ltv_partial_repay" || plan.RepaymentProjection != nil {
		r, ok := request.(KaminoPrimeUSDCRequest)
		if budget.Pilot == nil || !ok || decision.Action != DeleverRouteStep || decision.Reason != "hard_ltv_partial_repay" || plan.RepaymentProjection == nil || plan.Payoff == nil || budget.Families[family].ExitMicros == 0 || !decisionsEqual(manifest.DecideOnManifest(observation.Snapshot), decision) {
			return budgetHold("partial_repayment_requires_reserved_pilot_position")
		}
		if _, err = validatePartialRepaymentProjection(r, effects, observation.Snapshot, *plan.RepaymentProjection); err != nil {
			return err
		}
	}
	if decision.Action == InitializeKaminoObligation {
		r, ok := request.(KaminoInitializationRequest)
		if budget.Pilot == nil || !ok || r.RouteLane != lane || !manifest.initializationSnapshotReady(observation.Snapshot) || !decisionsEqual(manifest.DecideOnManifest(observation.Snapshot), decision) || budget.Families[family].ExitMicros != 0 || plan.ExitAfterMicros != 0 || len(plan.Exit) != 0 {
			return budgetHold("initializer_requires_flat_pilot_admission")
		}
		recovery = false
	}
	if decision.Action == SwapDebtToCollateralStep {
		entry, ok := request.(JupiterSwapRequest)
		if !ok || !entry.PositionReturnReserved || plan.LeverageProjection == nil || plan.Payoff == nil || budget.Families[family].ExitMicros == 0 {
			return budgetHold("leverage_requires_reserved_position")
		}
		recovery = false
	}
	if decision.Action == SwapStableToCollateralStep {
		entry, ok := request.(JupiterSwapRequest)
		if !ok || !entry.EntryReturnReserved || budget.Families[family].ExitMicros == 0 {
			return budgetHold("entry_requires_reserved_bridge_custody")
		}
		// Entry extends a pre-existing bridge reserve within family/goal caps;
		// it is not an unwind that must fit inside the cheaper cash-only exit.
		// Budget.Admit still forbids consuming the prior reserve for headroom.
		recovery = false
	}
	if decision.Action == OpenRouteStep {
		entry, ok := request.(KaminoPrimeUSDCRequest)
		_, leg, entryErr := kaminoPrimeUSDCInstruction(entry)
		if !ok || entryErr != nil {
			return budgetHold("invalid_kamino_entry_admission")
		}
		switch leg {
		case kaminoLegDeposit:
			if plan.Snapshot.PositionDebtRaw > 0 {
				if plan.DepositProjection == nil || plan.Payoff == nil || !plan.Snapshot.HasPosition || plan.Snapshot.PositionCollateralRaw <= 0 || budget.Families[family].ExitMicros == 0 {
					return budgetHold("redeposit_requires_reserved_position")
				}
			} else if plan.DepositProjection == nil || plan.Snapshot.HasPosition || plan.Snapshot.PositionCollateralRaw != 0 || budget.Families[family].ExitMicros == 0 {
				return budgetHold("deposit_requires_reserved_collateral_custody")
			}
		case kaminoLegBorrow:
			if plan.BorrowProjection == nil || plan.Payoff == nil || !plan.Snapshot.HasPosition || plan.Snapshot.PositionCollateralRaw <= 0 || plan.Snapshot.PositionDebtRaw != 0 || budget.Families[family].ExitMicros == 0 {
				return budgetHold("borrow_requires_reserved_position")
			}
		default:
			return budgetHold("invalid_kamino_entry_admission")
		}
		recovery = false
	}
	// An already-flat maintenance report is fee spend. Staging/restoration
	// cannot adopt unreserved capital through this exception.
	if decision.Action == ReportNAV && budget.Families[family].ExitMicros == 0 && len(plan.Exit) == 0 {
		recovery = false
	}
	exitAfter := plan.ExitAfterMicros
	// A mid-unwind recovery step must not shrink the committed reserve to its
	// own re-priced plan tail: quote drift between planning windows would
	// strand the discarded slack and hold the next step on
	// recovery_exceeds_reserved_exit. Retain the unspent prior reserve minus
	// this transaction's admitted upper bound instead. Admit still refuses
	// when this upper bound plus the plan tail exceeds the prior reserve, and
	// a genuinely terminal tail of zero still clears the reserve.
	if recovery && exitAfter > 0 && plan.CurrentCost.TotalMicros <= budget.Families[family].ExitMicros {
		if retained := budget.Families[family].ExitMicros - plan.CurrentCost.TotalMicros; retained > exitAfter {
			exitAfter = retained
		}
	}
	maintenance, retainedExit, maintenanceErr := phase3MaintenanceNAVReserve(budget, family, observation.Snapshot, decision, plan, Action(lastReconciledAction), durableUnwind)
	if maintenanceErr != nil {
		return maintenanceErr
	}
	if maintenance {
		recovery, exitAfter = false, retainedExit
	}
	r := BudgetReservation{OperationID: operationID, Family: family, IntentSHA256: intent,
		UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: exitAfter, Recovery: recovery}
	if budget.Pilot != nil {
		r.ExecutionCostUpperMicros = plan.CurrentCost.ExecutionCost.TotalMicros
	}
	if err = budget.Admit(r); err != nil {
		return err
	}
	auth = phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: plan.Input, BridgeAdmission: &plan}
	if custody.SpendRaw > 0 {
		// writePhase3BudgetTx performs this transaction's generation increment
		// (any earlier in-transaction increment, e.g. a new selector entry, has
		// already run above). The durable binding deliberately records the
		// ADMITTED generation — read again under the lock held since the proof
		// validation — so the build fence compares it against the
		// post-admission route state, not the pre-admission one the carried
		// proof was observed under. This is the ONLY custody re-stamp beside
		// the narrow build seam below: the shared writer is never made to
		// renew custody authority on unrelated phase3 transitions (coordinator
		// decision on doc 28's generic restamping proposal).
		var admittedGeneration int64
		if err = tx.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&admittedGeneration); err != nil {
			return err
		}
		custody.Generation = admittedGeneration + 1
		custodyBinding := custody
		auth.CustodyProof = &custodyBinding
	}
	if plan.RepaymentProjection != nil {
		if err = d.persistPartialRepaymentUnwindTx(ctx, tx, plan, budget, intent); err != nil {
			return err
		}
	}
	if budget.Pilot != nil {
		auth.PilotAuthorityID = budget.Pilot.AuthorityID
	}
	if err = d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Maintenance pays its fee from unused budget, retaining the complete exit reserve.
// The measured plan remains unchanged: only the durable reservation retains
// additional headroom. An actual unwind still consumes its existing reserve.
func phase3MaintenanceNAVReserve(budget Phase3Budget, family string, s Snapshot, decision Decision, plan phase3BridgeAdmission, lastReconciledAction Action, durableUnwind bool) (bool, int64, error) {
	if decision.Action != ReportNAV || budget.Pilot == nil || !s.PilotActive ||
		(decision.Reason != "post_mutation_nav_due" && decision.Reason != "nav_due") ||
		s.Unwind || durableUnwind || s.UnwindRefreshRequired || s.CutoverDrain || s.WithdrawalDemandRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return false, plan.ExitAfterMicros, nil
	}
	if s.HasPosition && (s.LiquidationThresholdBPS <= 1500 || s.LTVBPS >= min(s.LiquidationThresholdBPS-1500, int64(6000))) {
		return false, plan.ExitAfterMicros, nil
	}
	if decision.Reason == "post_mutation_nav_due" {
		switch lastReconciledAction {
		case OpenRouteStep, SwapStableToCollateralStep, SwapDebtToCollateralStep:
		default:
			return false, plan.ExitAfterMicros, nil
		}
	}
	if !decisionsEqual(Decide(s), decision) {
		return false, 0, budgetHold("maintenance_nav_decision_mismatch")
	}
	prior := budget.Families[family].ExitMicros
	exposed := s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 || s.SquadsIdleRaw != 0 || s.CollateralIdleRaw != 0 || s.PrimeIdleRaw != 0 || s.DebtIdleRaw != 0 || len(plan.Exit) != 0
	if exposed && (prior <= 0 || plan.ExitAfterMicros <= 0 || len(plan.Exit) == 0) {
		return false, 0, budgetHold("maintenance_nav_requires_reserved_exposure")
	}
	return true, max(prior, plan.ExitAfterMicros), nil
}

// Called only after the production cost observation, before signer access.
// A fresh known debit cannot inherit a smaller durable reservation.
func (d *Database) authorizePhase3Build(ctx context.Context, rpc *RPCClient, operationID string, request any, effects []byte, knownCost ValuedTransactionCost) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	return d.authorizePhase3BuildOnManifest(ctx, manifest, rpc, operationID, request, effects, knownCost)
}

// authorizePhase3BuildOnManifest is the exact locked build authorization body;
// only the pilot effects decode is resolved through the explicit reviewed
// manifest, so a candidate AUTO reservation decodes the same effects the
// reviewed manifest compiled while installed lanes keep the public path above.
func (d *Database) authorizePhase3BuildOnManifest(ctx context.Context, manifest RouteManifest, rpc *RPCClient, operationID string, request any, effects []byte, knownCost ValuedTransactionCost) error {
	intent, err := Phase3IntentDigest(request, effects)
	if err != nil {
		return err
	}
	if _, err = d.currentLease(); err != nil {
		return err
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if auth.GoalID != Phase3GoalID || auth.IntentSHA256 != intent {
		return budgetHold("unreserved_build_intent")
	}
	// Shared build-authorization custody seam (doc 26 §3): a positive
	// AUTO-PYUSD spend requires the persisted admission binding, bound to the
	// exact effects being built and the CURRENT route generation. The row has
	// no built effects yet, so this reads only the in-memory build input and
	// the persisted authorization; MarkBuilt still runs after this gate.
	var custodyLane, custodyRouteKey string
	var custodyGeneration int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(op.strategy_key,''), op.route_key, route.state_version
		FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route ON route.route_key=op.route_key
		WHERE op.operation_id=$1`, operationID).Scan(&custodyLane, &custodyRouteKey, &custodyGeneration); err != nil {
		return err
	}
	advanceCustodyBinding := false
	if custodyLane == autoAUTOPYUSD.Lane {
		decodedEffects, decodeErr := decodeExpectedEffectsWithManifest(manifest, effects)
		if decodeErr != nil {
			return decodeErr
		}
		if err = requireSharedCustodyBuildBinding(custodyLane, custodyRouteKey, auth, decodedEffects, custodyGeneration); err != nil {
			return err
		}
		advanceCustodyBinding = auth.CustodyProof != nil
	}
	if err = budget.validatePilotReleaseAuthority(request); err != nil {
		return err
	}
	if err = budget.AuthorizeIntent(operationID, intent); err != nil {
		return err
	}
	observed, err := validatePilotProjectedReleaseRisk(ctx, rpc, auth.BridgeAdmission, knownCost.ObservationSlot)
	if err != nil {
		return err
	}
	knownCost.ObservationSlot = max(knownCost.ObservationSlot, observed)
	reservation := budget.Reservations[operationID]
	if auth.BridgeAdmission != nil && (knownCost.ObservationSlot < auth.BridgeAdmission.CurrentCost.ObservationSlot || knownCost.ObservationSlot > auth.BridgeAdmission.ValidThroughSlot) {
		return budgetHold("stale_bridge_admission_snapshot")
	}
	if knownCost.TotalMicros <= 0 || knownCost.TotalMicros > reservation.UpperMicros {
		return &BudgetHold{Reason: "fresh_build_cost_exceeds_reservation", Details: map[string]string{
			"knownCostMicros":     strconv.FormatInt(knownCost.TotalMicros, 10),
			"reservedUpperMicros": strconv.FormatInt(reservation.UpperMicros, 10),
			"observationSlot":     strconv.FormatInt(knownCost.ObservationSlot, 10),
			"messageSha256":       knownCost.MessageSHA256,
		}}
	}
	var selectorEffects ExpectedEffects
	if budget.Pilot != nil {
		decoded, decodeErr := decodeExpectedEffectsWithManifest(manifest, effects)
		if decodeErr != nil {
			return decodeErr
		}
		selectorEffects = decoded
		knownCost, err = manifest.observePilotExecutionCost(ctx, rpc, request, decoded, knownCost)
		if err != nil {
			return err
		}
		if err = manifest.validateReservedExecutionCost(budget, reservation, request, decoded, knownCost); err != nil {
			return err
		}
		if auth.BridgeAdmission != nil && knownCost.ObservationSlot > auth.BridgeAdmission.ValidThroughSlot {
			return budgetHold("stale_bridge_admission_snapshot")
		}
	}
	entrySlot := knownCost.ObservationSlot
	if budget.Pilot != nil {
		entrySlot, err = rpc.ConfirmedSlot(ctx)
		if err != nil {
			return err
		}
		validThrough := knownCost.ValidThroughSlot
		if auth.BridgeAdmission != nil {
			validThrough = min(validThrough, auth.BridgeAdmission.ValidThroughSlot)
		}
		if entrySlot < knownCost.ObservationSlot || entrySlot > validThrough {
			return budgetHold("stale_bridge_admission_snapshot")
		}
	}
	if err = d.authorizeSelectorEntryTxOnManifest(ctx, manifest, tx, operationID, budget, request, selectorEffects, entrySlot, false); err != nil {
		return err
	}
	auth.BuildInput, err = encodePhase3BuildInput(request, effects)
	if err != nil {
		return err
	}
	if advanceCustodyBinding {
		// Coordinator-authorized NARROW build seam (doc 28 §B minimal
		// alternative; the generic writePhase3BudgetTx restamping was
		// rejected — wire/recovery/accounting writers must never renew
		// custody authority). The write below consumes one generation, so
		// record that post-build generation on the binding deliberately —
		// re-read in THIS transaction, after every earlier write in it — but
		// only after the gate above actually validated the binding at the
		// pre-build generation. Without this, a repeated authorizePhase3Build
		// (crash after this commit, before MarkBuilt, same live lease) finds
		// binding.Generation one behind the route lock and bricks on
		// custody_attribution_proof_drift. Unrelated state changes still
		// refuse at the gate.
		var currentGeneration int64
		if err = tx.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, custodyRouteKey).Scan(&currentGeneration); err != nil {
			return err
		}
		advanced := *auth.CustodyProof
		advanced.Generation = currentGeneration + 1
		auth.CustodyProof = &advanced
	}
	if err = d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d *Database) bindPhase3WireTx(ctx context.Context, tx pgx.Tx, operationID, wireSHA256 string) error {
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if auth.GoalID != Phase3GoalID || !sha256Pattern.MatchString(wireSHA256) {
		return budgetHold("unreserved_signed_wire")
	}
	if err = budget.AuthorizeIntent(operationID, auth.IntentSHA256); err != nil {
		return err
	}
	if auth.SignedWireSHA256 != "" && auth.SignedWireSHA256 != wireSHA256 {
		return budgetHold("signed_wire_reservation_mismatch")
	}
	auth.SignedWireSHA256 = wireSHA256
	return d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth)
}

func (d *Database) authorizePhase3SendTx(ctx context.Context, tx pgx.Tx, operationID, intent, wireHash string, cost ValuedTransactionCost, confirmedSlot int64) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	return d.authorizePhase3SendTxOnManifest(ctx, manifest, tx, operationID, intent, wireHash, cost, confirmedSlot)
}

// authorizePhase3SendTxOnManifest is the exact locked final-send fence; the
// persisted executable input and the pilot reservation fence resolve through
// the explicit reviewed manifest, while the wire/status/goal identity, intent
// and cost gates stay byte-identical. The public form above is unchanged.
func (d *Database) authorizePhase3SendTxOnManifest(ctx context.Context, manifest RouteManifest, tx pgx.Tx, operationID, intent, wireHash string, cost ValuedTransactionCost, confirmedSlot int64) error {
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if auth.BuildInput != nil {
		request, _, _, err := auth.BuildInput.decodeWithManifest(manifest)
		if err != nil {
			return err
		}
		if err = budget.validatePilotReleaseAuthority(request); err != nil {
			return err
		}
	}
	if auth.PolicySetup != nil {
		if _, err := d.validatePolicySetupReservationTx(ctx, tx, operationID, budget, auth, Signed); err != nil {
			return err
		}
		if auth.SetupBuildCost == nil {
			return budgetHold("setup_payment_not_build_authorized")
		}
	}
	var wire []byte
	if err = tx.QueryRow(ctx, `SELECT signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='signed'`, operationID).Scan(&wire); err != nil {
		return err
	}
	if auth.GoalID != Phase3GoalID || len(wire) == 0 || auth.SignedWireSHA256 != sha256Bytes(wire) {
		return budgetHold("signed_wire_reservation_mismatch")
	}
	if auth.IntentSHA256 != intent || auth.SignedWireSHA256 != wireHash {
		return budgetHold("final_send_identity_changed")
	}
	if err = budget.AuthorizeIntent(operationID, auth.IntentSHA256); err != nil {
		return err
	}
	if cost.TotalMicros <= 0 || cost.TotalMicros > budget.Reservations[operationID].UpperMicros {
		return budgetHold("fresh_send_cost_exceeds_reservation")
	}
	if budget.Pilot != nil {
		request, effects, _, err := auth.BuildInput.decodeWithManifest(manifest)
		if err != nil {
			return err
		}
		if err = manifest.validateReservedExecutionCost(budget, budget.Reservations[operationID], request, effects, cost); err != nil {
			return err
		}
	}
	if auth.BuildInput != nil {
		request, effects, _, err := auth.BuildInput.decodeWithManifest(manifest)
		if err != nil {
			return err
		}
		if err = d.authorizeSelectorEntryTxOnManifest(ctx, manifest, tx, operationID, budget, request, effects, confirmedSlot, false); err != nil {
			return err
		}
	}
	auth.SendKnownCost = &cost
	return d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth)
}
