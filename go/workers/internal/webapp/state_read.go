package webapp

// DB reads for GET /api/smart-accounts/yield-optimization/earn-state.
// Ports L/yield-deposit-repository.server.ts findActiveYieldPositionForVault
// :2679, findReconciledActiveYieldPositionForVault :2849 (whose reconcile
// writers applyHoldingEventToPosition :2169, recordConfirmedYieldRebalance
// :4216 and recordSnapshotReconciledYieldHolding :4384 are ported verbatim),
// findCurrentNonzeroYieldVaultReservePositions :2973 and
// findCurrentYieldVaultIdleTokenBalances :3014.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

const statePositionColumns = `id, settings, vault_index, vault_pubkey, initial_reserve,
  initial_market, initial_liquidity_mint, initial_supply_apy_bps, principal_amount_raw,
  current_reserve, current_market, current_liquidity_mint, current_amount_raw,
  current_observed_slot, current_observed_at, last_holding_event_id,
  last_rebalance_decision_id, status`

const stateEventColumns = `id, position_id, event_type::text, reserve, market, liquidity_mint,
  amount_raw, observed_slot, observed_at, source_rebalance_decision_id, source_snapshot_id`

func scanStatePosition(row pgx.Row) (*stateYieldPositionRow, error) {
	var p stateYieldPositionRow
	err := row.Scan(&p.ID, &p.Settings, &p.VaultIndex, &p.VaultPubkey, &p.InitialReserve,
		&p.InitialMarket, &p.InitialLiquidityMint, &p.InitialSupplyApyBps, &p.PrincipalAmountRaw,
		&p.CurrentReserve, &p.CurrentMarket, &p.CurrentLiquidityMint, &p.CurrentAmountRaw,
		&p.CurrentObservedSlot, &p.CurrentObservedAt, &p.LastHoldingEventID,
		&p.LastRebalanceDecisionID, &p.Status)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func scanStateEvent(row pgx.Row) (*stateHoldingEventRow, error) {
	var e stateHoldingEventRow
	err := row.Scan(&e.ID, &e.PositionID, &e.EventType, &e.Reserve, &e.Market, &e.LiquidityMint,
		&e.AmountRaw, &e.ObservedSlot, &e.ObservedAt, &e.SourceRebalanceDecisionID, &e.SourceSnapshotID)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// activeYieldPositionForVault ports findActiveYieldPositionForVault.
func (h earnRoutes) activeYieldPositionForVault(ctx context.Context, settings, wallet string) (*stateYieldPositionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	position, err := scanStatePosition(h.YieldDB.QueryRow(ctx, `SELECT `+statePositionColumns+`
FROM loyal_yield.user_yield_positions
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND status = 'active'
ORDER BY updated_at DESC, id DESC
LIMIT 1`, settings, int16(earnprep.EarnVaultIndex), wallet))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read active yield position: %w", err)
	}
	return position, nil
}

// stateManagedVaultID ports the managedVaults.findFirst lookup shared by the
// reconcile read, the reserve/idle readers and the policy pair fallbacks.
func (h earnRoutes) stateManagedVaultID(ctx context.Context, settings, vaultPubkey string) (*int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id int64
	err := h.YieldDB.QueryRow(ctx, `SELECT id FROM loyal_yield.managed_vaults
WHERE active = TRUE AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3
ORDER BY last_seen_at DESC, id DESC
LIMIT 1`, settings, int16(earnprep.EarnVaultIndex), vaultPubkey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed vault: %w", err)
	}
	return &id, nil
}

// latestHoldingEventForPosition ports findLatestHoldingEventForPosition.
func (h earnRoutes) latestHoldingEventForPosition(ctx context.Context, positionID int64) (*stateHoldingEventRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	event, err := scanStateEvent(h.YieldDB.QueryRow(ctx, `SELECT `+stateEventColumns+`
FROM loyal_yield.user_yield_position_holding_events
WHERE position_id = $1
ORDER BY observed_slot DESC, observed_at DESC, id DESC
LIMIT 1`, positionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest holding event: %w", err)
	}
	return event, nil
}

// holdingEventUsesCollateralUnitSnapshot ports holdingEventUsesCollateralUnitSnapshot.
func (h earnRoutes) holdingEventUsesCollateralUnitSnapshot(ctx context.Context, event *stateHoldingEventRow) (bool, error) {
	if event == nil || event.EventType != "snapshot_reconciled" || event.SourceSnapshotID == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var metadata map[string]any
	err := h.YieldDB.QueryRow(ctx, `SELECT planning_metadata
FROM loyal_yield.vault_position_snapshot_positions
WHERE snapshot_id = $1 AND reserve = $2 AND liquidity_mint = $3 AND amount_raw = $4
LIMIT 1`, *event.SourceSnapshotID, event.Reserve, event.LiquidityMint, event.AmountRaw).Scan(&metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read snapshot position metadata: %w", err)
	}
	return stateCollateralUnitMetadata(metadata), nil
}

// projectPositionForUserFacingRead ports projectPositionForUserFacingRead and
// principalBackedPositionProjection: a collateral-unit snapshot event shows the
// principal as the holding instead of the collateral amount.
func (h earnRoutes) projectPositionForUserFacingRead(ctx context.Context, position *stateYieldPositionRow, event *stateHoldingEventRow) (*stateYieldPositionRow, error) {
	collateral, err := h.holdingEventUsesCollateralUnitSnapshot(ctx, event)
	if err != nil {
		return nil, err
	}
	if !collateral || position.PrincipalAmountRaw <= 0 {
		return position, nil
	}
	projected := *position
	projected.CurrentAmountRaw = position.PrincipalAmountRaw
	if event != nil {
		projected.CurrentLiquidityMint = event.LiquidityMint
		projected.CurrentMarket = event.Market
		projected.CurrentObservedAt = event.ObservedAt
		projected.CurrentObservedSlot = event.ObservedSlot
		projected.CurrentReserve = event.Reserve
	}
	return &projected, nil
}

func currentStateVaultPositionMatchesEvent(current stateVaultReserveRow, event *stateHoldingEventRow) bool {
	return event != nil &&
		current.Reserve == event.Reserve &&
		stateStringPtrEqual(current.Market, event.Market) &&
		current.LiquidityMint == event.LiquidityMint &&
		current.AmountRaw == event.AmountRaw &&
		current.ObservedSlot == event.ObservedSlot &&
		current.ObservedAt.UnixMilli() == event.ObservedAt.UnixMilli()
}

func currentPositionMatchesHoldingEvent(position *stateYieldPositionRow, event *stateHoldingEventRow) bool {
	return event != nil &&
		position.CurrentReserve == event.Reserve &&
		stateStringPtrEqual(position.CurrentMarket, event.Market) &&
		position.CurrentLiquidityMint == event.LiquidityMint &&
		position.CurrentAmountRaw == event.AmountRaw &&
		position.CurrentObservedSlot == event.ObservedSlot &&
		position.CurrentObservedAt.UnixMilli() == event.ObservedAt.UnixMilli() &&
		position.LastHoldingEventID != nil && *position.LastHoldingEventID == event.ID
}

func stateStringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// stateApplyHoldingEventToPosition ports applyHoldingEventToPosition. A nil
// lastRebalanceDecisionID leaves the stored decision id untouched, exactly as
// the TS omits the field from its update set.
func (h earnRoutes) stateApplyHoldingEventToPosition(ctx context.Context, event *stateHoldingEventRow, lastRebalanceDecisionID *int64, now time.Time) (*stateYieldPositionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	position, err := scanStatePosition(h.YieldDB.QueryRow(ctx, `UPDATE loyal_yield.user_yield_positions
SET current_amount_raw = $2, current_liquidity_mint = $3, current_market = $4,
  current_observed_at = $5, current_observed_slot = $6, current_reserve = $7,
  last_holding_event_id = $8,
  last_rebalance_decision_id = COALESCE($9, last_rebalance_decision_id),
  updated_at = $10
WHERE id = $1
RETURNING `+statePositionColumns, event.PositionID, event.AmountRaw, event.LiquidityMint, event.Market,
		event.ObservedAt, event.ObservedSlot, event.Reserve, event.ID, lastRebalanceDecisionID, now))
	if err != nil {
		return nil, fmt.Errorf("apply yield holding event: %w", err)
	}
	return position, nil
}

// stateHoldingEventIsAtOrAfterCurrentPosition ports
// holdingEventIsAtOrAfterCurrentPosition (observedAt compared at millisecond
// precision, as Date.getTime does).
func stateHoldingEventIsAtOrAfterCurrentPosition(position *stateYieldPositionRow, event *stateHoldingEventRow) bool {
	if event.ObservedSlot != position.CurrentObservedSlot {
		return event.ObservedSlot > position.CurrentObservedSlot
	}
	return event.ObservedAt.UnixMilli() >= position.CurrentObservedAt.UnixMilli()
}

// stateApplyRebalanceHoldingEventToPosition ports
// applyRebalanceHoldingEventToPosition: a newer event moves the position, an
// older one only advances last_rebalance_decision_id.
func (h earnRoutes) stateApplyRebalanceHoldingEventToPosition(ctx context.Context, event *stateHoldingEventRow, lastRebalanceDecisionID int64, now time.Time) (*stateYieldPositionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	position, err := scanStatePosition(h.YieldDB.QueryRow(ctx, `SELECT `+statePositionColumns+`
FROM loyal_yield.user_yield_positions
WHERE id = $1`, event.PositionID))
	if err != nil {
		return nil, fmt.Errorf("find yield position for rebalance event: %w", err)
	}
	if stateHoldingEventIsAtOrAfterCurrentPosition(position, event) {
		return h.stateApplyHoldingEventToPosition(ctx, event, &lastRebalanceDecisionID, now)
	}
	updated, err := scanStatePosition(h.YieldDB.QueryRow(ctx, `UPDATE loyal_yield.user_yield_positions
SET last_rebalance_decision_id = GREATEST(COALESCE(last_rebalance_decision_id, 0::bigint), $2),
  updated_at = $3
WHERE id = $1
RETURNING `+statePositionColumns, event.PositionID, lastRebalanceDecisionID, now))
	if err != nil {
		return nil, fmt.Errorf("apply yield rebalance decision: %w", err)
	}
	return updated, nil
}

// stateInsertRebalanceHoldingEvent ports insertIdempotentRebalanceHoldingEvent:
// the partial unique index on source_rebalance_decision_id makes a repeated
// confirmed-rebalance read reuse the existing event instead of duplicating it.
func (h earnRoutes) stateInsertRebalanceHoldingEvent(ctx context.Context, event stateHoldingEventRow, sourceSignature string, sourceRebalanceDecisionID, sourceSnapshotID int64, createdAt time.Time, positionID int64) (*stateHoldingEventRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inserted, err := scanStateEvent(h.YieldDB.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_position_holding_events (
  position_id, event_type, reserve, market, liquidity_mint, amount_raw,
  principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
  source_signature, source_deposit_id, source_withdrawal_id,
  source_rebalance_decision_id, source_snapshot_id, created_at)
VALUES ($1, 'rebalance_confirmed', $2, $3, $4, $5, NULL, NULL, $6, $7, $8, NULL, NULL, $9, $10, $11)
ON CONFLICT (source_rebalance_decision_id) WHERE source_rebalance_decision_id IS NOT NULL
DO NOTHING
RETURNING `+stateEventColumns, positionID, event.Reserve, event.Market, event.LiquidityMint, event.AmountRaw,
		event.ObservedSlot, event.ObservedAt, sourceSignature, sourceRebalanceDecisionID, sourceSnapshotID, createdAt))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, scanErr := scanStateEvent(h.YieldDB.QueryRow(ctx, `SELECT `+stateEventColumns+`
FROM loyal_yield.user_yield_position_holding_events
WHERE source_rebalance_decision_id = $1
LIMIT 1`, sourceRebalanceDecisionID))
		if scanErr != nil {
			return nil, errors.New("failed to record idempotent yield rebalance event")
		}
		return existing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record yield rebalance event: %w", err)
	}
	return inserted, nil
}

// stateInsertSnapshotHoldingEvent ports insertHoldingEvent for
// recordSnapshotReconciledYieldHolding: a fresh event per reconciliation.
func (h earnRoutes) stateInsertSnapshotHoldingEvent(ctx context.Context, event stateHoldingEventRow, sourceSnapshotID int64, createdAt time.Time) (*stateHoldingEventRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inserted, err := scanStateEvent(h.YieldDB.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_position_holding_events (
  position_id, event_type, reserve, market, liquidity_mint, amount_raw,
  principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
  source_signature, source_deposit_id, source_withdrawal_id,
  source_rebalance_decision_id, source_snapshot_id, created_at)
VALUES ($1, 'snapshot_reconciled', $2, $3, $4, $5, NULL, NULL, $6, $7, NULL, NULL, NULL, NULL, $8, $9)
RETURNING `+stateEventColumns, event.PositionID, event.Reserve, event.Market, event.LiquidityMint, event.AmountRaw,
		event.ObservedSlot, event.ObservedAt, sourceSnapshotID, createdAt))
	if err != nil {
		return nil, fmt.Errorf("record yield holding event: %w", err)
	}
	return inserted, nil
}

// stateVaultReserveRows ports the vaultReservePositionsCurrent read shared by
// the reconcile read and the current-total loader.
func (h earnRoutes) stateVaultReserveRows(ctx context.Context, vaultID int64) ([]stateVaultReserveRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `SELECT reserve, market, liquidity_mint, amount_raw,
  supply_apy_bps, snapshot_id, observed_slot, observed_at, planning_metadata
FROM loyal_yield.vault_reserve_positions_current
WHERE vault_id = $1 AND amount_raw > 0
ORDER BY observed_slot DESC, amount_raw DESC, snapshot_id DESC`, vaultID)
	if err != nil {
		return nil, fmt.Errorf("read vault reserve positions: %w", err)
	}
	defer rows.Close()
	var out []stateVaultReserveRow
	for rows.Next() {
		var row stateVaultReserveRow
		if err := rows.Scan(&row.Reserve, &row.Market, &row.LiquidityMint, &row.AmountRaw,
			&row.SupplyApyBps, &row.SnapshotID, &row.ObservedSlot, &row.ObservedAt, &row.PlanningMetadata); err != nil {
			return nil, fmt.Errorf("read vault reserve positions: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read vault reserve positions: %w", err)
	}
	return out, nil
}

// stateCurrentReserveRows ports findCurrentNonzeroYieldVaultReservePositions
// (position and vault lookups included, user-facing metadata filter applied).
func (h earnRoutes) stateCurrentReserveRows(ctx context.Context, settings, wallet, vaultPubkey string) ([]stateVaultReserveRow, error) {
	position, err := h.activeYieldPositionForVault(ctx, settings, wallet)
	if err != nil || position == nil {
		return nil, err
	}
	vaultID, err := h.stateManagedVaultID(ctx, settings, vaultPubkey)
	if err != nil || vaultID == nil {
		return nil, err
	}
	rows, err := h.stateVaultReserveRows(ctx, *vaultID)
	if err != nil {
		return nil, err
	}
	filtered := make([]stateVaultReserveRow, 0, len(rows))
	for _, row := range rows {
		if stateUserFacingReserveMetadata(row.PlanningMetadata) {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

// stateCurrentIdleRows ports findCurrentYieldVaultIdleTokenBalances.
func (h earnRoutes) stateCurrentIdleRows(ctx context.Context, settings, wallet, vaultPubkey string) ([]stateVaultIdleRow, error) {
	position, err := h.activeYieldPositionForVault(ctx, settings, wallet)
	if err != nil || position == nil {
		return nil, err
	}
	vaultID, err := h.stateManagedVaultID(ctx, settings, vaultPubkey)
	if err != nil || vaultID == nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `SELECT mint, amount_raw, owner, token_account,
  source_commitment, observed_slot, observed_at
FROM loyal_yield.vault_idle_token_balances_current
WHERE vault_id = $1 AND amount_raw > 0
ORDER BY observed_slot DESC, updated_at DESC`, *vaultID)
	if err != nil {
		return nil, fmt.Errorf("read vault idle balances: %w", err)
	}
	defer rows.Close()
	var out []stateVaultIdleRow
	for rows.Next() {
		var row stateVaultIdleRow
		if err := rows.Scan(&row.Mint, &row.AmountRaw, &row.Owner, &row.TokenAccount,
			&row.SourceCommitment, &row.ObservedSlot, &row.ObservedAt); err != nil {
			return nil, fmt.Errorf("read vault idle balances: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read vault idle balances: %w", err)
	}
	return out, nil
}

// reconciledActiveYieldPosition ports findReconciledActiveYieldPositionForVault
// including its three reconcile writers. The vault has no newer
// user-facing reserve row than the position: the stored position is returned
// untouched. A newer row reconciles the position through a confirmed rebalance
// event (idempotent by decision id) or a snapshot reconciliation event.
func (h earnRoutes) reconciledActiveYieldPosition(ctx context.Context, account EarnAccount) (*stateYieldPositionRow, error) {
	position, err := h.activeYieldPositionForVault(ctx, account.Settings.String(), account.Wallet.String())
	if err != nil || position == nil {
		return nil, err
	}
	vaultID, err := h.stateManagedVaultID(ctx, account.Settings.String(), position.VaultPubkey)
	if err != nil {
		return nil, err
	}
	if vaultID == nil {
		return position, nil
	}

	latestEvent, err := h.latestHoldingEventForPosition(ctx, position.ID)
	if err != nil {
		return nil, err
	}
	positionForRead, err := h.projectPositionForUserFacingRead(ctx, position, latestEvent)
	if err != nil {
		return nil, err
	}
	currentRows, err := h.stateVaultReserveRows(ctx, *vaultID)
	if err != nil {
		return nil, err
	}
	var current *stateVaultReserveRow
	for i := range currentRows {
		if stateUserFacingReserveMetadata(currentRows[i].PlanningMetadata) {
			current = &currentRows[i]
			break
		}
	}

	if current == nil {
		return positionForRead, nil
	}
	if current.ObservedSlot <= position.CurrentObservedSlot {
		return positionForRead, nil
	}

	if currentVaultPositionMatches := currentStateVaultPositionMatchesEvent(*current, latestEvent); currentVaultPositionMatches {
		if currentPositionMatchesHoldingEvent(position, latestEvent) {
			return position, nil
		}
		return h.stateApplyHoldingEventToPosition(ctx, latestEvent, nil, time.Now())
	}

	decision, err := h.stateConfirmedRebalanceDecision(ctx, *vaultID, current.Reserve, current.SnapshotID)
	if err != nil {
		return nil, err
	}
	if decision != nil && decision.Signature != nil && decision.ConfirmedSlot != nil {
		event := stateHoldingEventRow{
			PositionID:                position.ID,
			Reserve:                   current.Reserve,
			Market:                    current.Market,
			LiquidityMint:             current.LiquidityMint,
			AmountRaw:                 current.AmountRaw,
			ObservedSlot:              current.ObservedSlot,
			ObservedAt:                current.ObservedAt,
			SourceRebalanceDecisionID: &decision.ID,
		}
		now := time.Now()
		inserted, err := h.stateInsertRebalanceHoldingEvent(ctx, event, *decision.Signature, decision.ID, current.SnapshotID, now, position.ID)
		if err != nil {
			return nil, err
		}
		return h.stateApplyRebalanceHoldingEventToPosition(ctx, inserted, decision.ID, now)
	}

	event := stateHoldingEventRow{
		PositionID:       position.ID,
		Reserve:          current.Reserve,
		Market:           current.Market,
		LiquidityMint:    current.LiquidityMint,
		AmountRaw:        current.AmountRaw,
		ObservedSlot:     current.ObservedSlot,
		ObservedAt:       current.ObservedAt,
		SourceSnapshotID: &current.SnapshotID,
	}
	now := time.Now()
	inserted, err := h.stateInsertSnapshotHoldingEvent(ctx, event, current.SnapshotID, now)
	if err != nil {
		return nil, err
	}
	return h.stateApplyHoldingEventToPosition(ctx, inserted, nil, now)
}

// stateConfirmedRebalanceDecision ports the rebalanceDecisions.findFirst of
// findReconciledActiveYieldPositionForVault.
func (h earnRoutes) stateConfirmedRebalanceDecision(ctx context.Context, vaultID int64, reserve string, snapshotID int64) (*stateRebalanceDecisionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var decision stateRebalanceDecisionRow
	err := h.YieldDB.QueryRow(ctx, `SELECT id, signature, confirmed_slot
FROM loyal_yield.rebalance_decisions
WHERE vault_id = $1 AND status = 'confirmed' AND target_reserve = $2 AND post_snapshot_id = $3
ORDER BY confirmed_slot DESC, id DESC
LIMIT 1`, vaultID, reserve, snapshotID).Scan(&decision.ID, &decision.Signature, &decision.ConfirmedSlot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read confirmed rebalance decision: %w", err)
	}
	return &decision, nil
}
