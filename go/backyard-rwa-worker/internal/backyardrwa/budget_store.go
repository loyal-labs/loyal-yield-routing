package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type phase3OperationAuthorization struct {
	GoalID                string                  `json:"goalId"`
	IntentSHA256          string                  `json:"intentSha256"`
	SignedWireSHA256      string                  `json:"signedWireSha256,omitempty"`
	ReservationReleased   bool                    `json:"reservationReleased,omitempty"`
	BookedSpentMicros     int64                   `json:"bookedSpentMicros,omitempty"`
	BuildInput            *phase3BuildInput       `json:"buildInput,omitempty"`
	SendKnownCost         *ValuedTransactionCost  `json:"sendKnownCost,omitempty"`
	BridgeAdmission       *phase3BridgeAdmission  `json:"bridgeAdmission,omitempty"`
	PolicySetup           *policySetupObservation `json:"policySetup,omitempty"`
	PolicySetupCompletion *policySetupCompletion  `json:"policySetupCompletion,omitempty"`
	SetupBuildCost        *ValuedTransactionCost  `json:"setupBuildCost,omitempty"`
	SetupCompletionCost   *ValuedTransactionCost  `json:"setupCompletionCost,omitempty"`
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
	var budgetBytes, authBytes []byte
	var lane string
	err := tx.QueryRow(ctx, `SELECT COALESCE(route.state->'phase3','null'::jsonb), COALESCE(operation.expected_effects->'phase3','null'::jsonb),COALESCE(operation.strategy_key,'')
		FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route ON route.route_key=operation.route_key
		WHERE operation.operation_id=$1`, operationID).Scan(&budgetBytes, &authBytes, &lane)
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
func (d *Database) persistPhase3ExitAdmission(ctx context.Context, rpc *RPCClient, operationID string, observation Observation, decision Decision, plan phase3BridgeAdmission) error {
	if d == nil || d.pool == nil {
		return budgetHold("bridge_admission_database_unavailable")
	}
	request, _, _, err := plan.Input.decode()
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
	var status, lane, action string
	var decisionBytes []byte
	if err = tx.QueryRow(ctx, `SELECT status,COALESCE(strategy_key,''),COALESCE(action,''),expected_effects->'decision' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&status, &lane, &action, &decisionBytes); err != nil {
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
		return tx.Commit(ctx)
	}
	family := phase3BudgetFamilyForLane(lane)
	for other, row := range budget.Families {
		if other != family && row.ExitMicros != 0 {
			return budgetHold("another_family_has_reserved_exit")
		}
	}
	recovery := decision.Action != VoltrAllocateToSquads
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
	r := BudgetReservation{OperationID: operationID, Family: family, IntentSHA256: intent,
		UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: recovery}
	if err = budget.Admit(r); err != nil {
		return err
	}
	auth = phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: plan.Input, BridgeAdmission: &plan}
	if err = d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Called only after the production cost observation, before signer access.
// A fresh known debit cannot inherit a smaller durable reservation.
func (d *Database) authorizePhase3Build(ctx context.Context, operationID string, request any, effects []byte, knownCost ValuedTransactionCost) error {
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
	if err = budget.AuthorizeIntent(operationID, intent); err != nil {
		return err
	}
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
	auth.BuildInput, err = encodePhase3BuildInput(request, effects)
	if err != nil {
		return err
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

func (d *Database) authorizePhase3SendTx(ctx context.Context, tx pgx.Tx, operationID, intent, wireHash string, cost ValuedTransactionCost) error {
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
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
	auth.SendKnownCost = &cost
	return d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth)
}
