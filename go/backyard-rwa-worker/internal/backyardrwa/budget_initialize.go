package backyardrwa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// This marker is written atomically with the first budget in the existing route
// row. Losing either half is corruption, not a fresh spending allowance.
type phase3Initialization struct {
	GoalID     string    `json:"goalId"`
	Generation int64     `json:"generation"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Phase3BudgetInitializationResult struct {
	ProofLevel     string               `json:"proofLevel"`
	Created        bool                 `json:"created"`
	Closed         bool                 `json:"closed"`
	Initialization phase3Initialization `json:"initialization"`
}

// RunPhase3BudgetInitialization is explicit database bookkeeping, never called
// by Run, Tick, admission or recovery. It neither loads a signer nor enables a
// lane. Phase 2 closure, chain flatness, setup and deployment remain separate
// activation gates. Only the fixed production route and goal can be created.
func RunPhase3BudgetInitialization(ctx context.Context, databaseURL, routeKey string) (result Phase3BudgetInitializationResult, err error) {
	if routeKey != productionRouteKey || databaseURL == "" {
		return result, budgetHold("invalid_phase3_initialization_config")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		return result, budgetHold("phase3_initialization_database_unavailable")
	}
	defer db.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return result, budgetHold("phase3_initialization_owner_unavailable")
	}
	if _, err = db.AcquireRouteLease(ctx, routeKey, "phase3-init:"+Phase3GoalID+":"+hex.EncodeToString(nonce[:]), 30*time.Second); err != nil {
		return result, budgetHold("phase3_initialization_lease_unavailable")
	}
	defer func() {
		// A canceled caller must not leave an otherwise releasable lease behind.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		released, releaseErr := db.ReleaseRouteLease(releaseCtx)
		if err == nil && (releaseErr != nil || !released) {
			err = budgetHold("phase3_initialization_lease_release_unconfirmed")
		}
	}()
	result, err = db.initializePhase3Budget(ctx, routeKey)
	if err != nil {
		var hold *BudgetHold
		if errors.As(err, &hold) {
			return result, hold
		}
		// Never expose a connection string, SQL error or provider response.
		return result, budgetHold("phase3_initialization_not_confirmed")
	}
	return result, nil
}

func (d *Database) initializePhase3Budget(ctx context.Context, routeKey string) (result Phase3BudgetInitializationResult, err error) {
	lease, err := d.currentLease()
	if err != nil || lease.RouteKey != routeKey {
		return result, ErrRouteLeaseLost
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var encoded []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &encoded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, ErrRouteLeaseLost
		}
		return result, err
	}
	var state map[string]json.RawMessage
	var generation int64
	if json.Unmarshal(encoded, &state) != nil || state == nil || version <= 0 || version == math.MaxInt64 || json.Unmarshal(state["generation"], &generation) != nil || generation != version {
		return result, budgetHold("invalid_initialization_route_state")
	}
	budgetJSON, hasBudget := state["phase3"]
	markerJSON, hasMarker := state["phase3Initialization"]
	if _, pendingSetup := state["phase3SetupIntent"]; pendingSetup && !hasBudget {
		return result, budgetHold("orphaned_policy_setup_intent")
	}
	if hasBudget != hasMarker {
		return result, budgetHold("incomplete_phase3_initialization")
	}
	result.ProofLevel = "BOOKKEEPING_NOT_ACTIVATION"
	if hasBudget {
		var budget Phase3Budget
		if json.Unmarshal(budgetJSON, &budget) != nil || budget.validate() != nil ||
			json.Unmarshal(markerJSON, &result.Initialization) != nil || result.Initialization.GoalID != Phase3GoalID ||
			result.Initialization.Generation < 2 || result.Initialization.Generation > version || result.Initialization.CreatedAt.IsZero() {
			return result, budgetHold("invalid_phase3_initialization")
		}
		for family := range budget.Families {
			familyTotal, goalTotal, sumErr := budget.totals(family)
			if sumErr != nil || familyTotal > Phase3FamilyCapMicros || goalTotal > Phase3GoalCapMicros {
				return result, budgetHold("persisted_budget_exceeds_cap")
			}
		}
		// Idempotent inspection does not reopen a closed budget, refund spending,
		// discard reservations or rewrite the original initialization generation.
		result.Closed = budget.Closed
		return result, nil
	}
	var priorSpend, active, recovery bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND (
	 expected_effects ? 'phase3' OR (strategy_key IN ('OnRe/ONyc/USDC','OnRe/ONyc/USDG','OnRe/ONyc/USDS','AUTO/AUTO/PYUSD','Ethena/USDe/PYUSD')
	 AND (signed_wire IS NOT NULL OR transaction_signature IS NOT NULL OR broadcast_intent_at IS NOT NULL))))`, routeKey).Scan(&priorSpend); err != nil {
		return result, err
	}
	if priorSpend {
		return result, budgetHold("phase3_history_prevents_budget_creation")
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1
	 AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling'))`, routeKey).Scan(&active); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&recovery); err != nil {
		return result, err
	}
	if active || recovery {
		return result, budgetHold("unresolved_work_prevents_budget_creation")
	}
	var createdAt time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		return result, err
	}
	result.Initialization = phase3Initialization{GoalID: Phase3GoalID, Generation: version + 1, CreatedAt: createdAt}
	budget := Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{"OnRe": {}, "AUTO": {}, "Ethena": {}}, Reservations: map[string]BudgetReservation{}}
	budgetJSON, err = json.Marshal(budget)
	if err != nil {
		return result, err
	}
	markerJSON, err = json.Marshal(result.Initialization)
	if err != nil {
		return result, err
	}
	updated, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET
	 state=jsonb_set(jsonb_set(jsonb_set(state,'{phase3}',$4::jsonb,true),'{phase3Initialization}',$5::jsonb,true),'{generation}',to_jsonb(state_version+1),true),
	 state_version=state_version+1,updated_at=clock_timestamp()
	 WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`,
		routeKey, lease.Owner, lease.FencingToken, string(budgetJSON), string(markerJSON))
	if err != nil {
		return result, err
	}
	if updated.RowsAffected() != 1 {
		return result, ErrRouteLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Created = true
	return result, nil
}
