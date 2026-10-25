package webapp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

// Reads the Earn history the Next earn-transactions route serves, including
// the sync write it performs first. Ported from
// web/apps/web/src/lib/yield-optimization/yield-deposit-repository.server.ts
// (syncConfirmedRebalanceHoldingEventsForVault :4245,
// findYieldPositionHistoryEventsForVault :3775) and
// earn-autodeposit-repository.server.ts (findEarnAutodepositHistoryEvents
// :1256).

// syncConfirmedRebalanceHoldingEventsForVault verbatim: one statement, so a
// crash mid-way either committed all of it or none of it, and re-running is
// idempotent on the partial unique index
// user_yield_position_holding_events_rebalance_decision_uidx
// (ON CONFLICT (source_rebalance_decision_id) WHERE ... DO NOTHING).
const txreadsSyncRebalanceSQL = `WITH candidate_events AS (
  SELECT DISTINCT ON (decision.id)
    position.id AS position_id,
    decision.id AS decision_id,
    decision.signature AS source_signature,
    decision.post_snapshot_id AS source_snapshot_id,
    snapshot.observed_slot,
    snapshot.observed_at,
    snapshot_position.reserve,
    snapshot_position.market,
    snapshot_position.liquidity_mint,
    snapshot_position.amount_raw
  FROM loyal_yield.rebalance_decisions decision
  INNER JOIN loyal_yield.managed_vaults vault
    ON vault.id = decision.vault_id
   AND vault.active = TRUE
  INNER JOIN loyal_yield.route_policies policy
    ON policy.id = vault.active_policy_id
   AND policy.active = TRUE
   AND policy.settings = vault.settings
   AND policy.vault_index = vault.vault_index
   AND policy.vault_pubkey = vault.vault_pubkey
  INNER JOIN loyal_yield.user_yield_positions position
    ON position.settings = policy.settings
   AND position.wallet_address = policy.authority
   AND position.vault_index = policy.vault_index
   AND position.vault_pubkey = policy.vault_pubkey
   AND position.status = 'active'::loyal_yield.yield_position_status
  INNER JOIN loyal_yield.vault_position_snapshots snapshot
    ON snapshot.id = decision.post_snapshot_id
   AND snapshot.vault_id = decision.vault_id
  INNER JOIN loyal_yield.vault_position_snapshot_positions snapshot_position
    ON snapshot_position.snapshot_id = snapshot.id
   AND snapshot_position.reserve = decision.target_reserve
   AND snapshot_position.amount_raw > 0
  WHERE decision.status = 'confirmed'::loyal_yield.decision_status
    AND decision.signature IS NOT NULL
    AND decision.post_snapshot_id IS NOT NULL
    AND vault.settings = $1
    AND vault.vault_index = $2
    AND policy.authority = $3
  ORDER BY
    decision.id,
    position.updated_at DESC,
    position.id DESC,
    snapshot_position.amount_raw DESC,
    snapshot_position.id DESC
), inserted_events AS (
  INSERT INTO loyal_yield.user_yield_position_holding_events (
    position_id,
    event_type,
    reserve,
    market,
    liquidity_mint,
    amount_raw,
    principal_delta_raw,
    holding_delta_raw,
    observed_slot,
    observed_at,
    source_signature,
    source_deposit_id,
    source_withdrawal_id,
    source_rebalance_decision_id,
    source_snapshot_id,
    created_at
  )
  SELECT
    candidate.position_id,
    'rebalance_confirmed'::loyal_yield.user_yield_holding_event_type,
    candidate.reserve,
    candidate.market,
    candidate.liquidity_mint,
    candidate.amount_raw,
    NULL,
    NULL,
    candidate.observed_slot,
    candidate.observed_at,
    candidate.source_signature,
    NULL,
    NULL,
    candidate.decision_id,
    candidate.source_snapshot_id,
    $4
  FROM candidate_events candidate
  ON CONFLICT (source_rebalance_decision_id)
    WHERE source_rebalance_decision_id IS NOT NULL
  DO NOTHING
  RETURNING position_id, source_rebalance_decision_id
), latest_projected_rebalance AS (
  SELECT DISTINCT ON (event.position_id)
    event.position_id,
    event.source_rebalance_decision_id
  FROM loyal_yield.user_yield_position_holding_events event
  INNER JOIN candidate_events candidate
    ON candidate.position_id = event.position_id
   AND candidate.decision_id = event.source_rebalance_decision_id
  WHERE event.source_rebalance_decision_id IS NOT NULL
  ORDER BY event.position_id, event.source_rebalance_decision_id DESC
), updated_positions AS (
  UPDATE loyal_yield.user_yield_positions position
  SET
    last_rebalance_decision_id =
      latest_projected_rebalance.source_rebalance_decision_id,
    updated_at = $4
  FROM latest_projected_rebalance
  WHERE position.id = latest_projected_rebalance.position_id
    AND (
      position.last_rebalance_decision_id IS NULL
      OR position.last_rebalance_decision_id <
        latest_projected_rebalance.source_rebalance_decision_id
    )
  RETURNING position.id
)
SELECT
  (SELECT COUNT(*)::int FROM inserted_events),
  (SELECT COUNT(*)::int FROM updated_positions)`

func (h earnRoutes) syncRebalanceHoldingEvents(ctx context.Context, account EarnAccount) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now()
	_, err := h.YieldDB.Exec(ctx, txreadsSyncRebalanceSQL,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String(), now)
	return err
}

// txreadsPositionSeed carries the position columns the history walk needs.
type txreadsPositionSeed struct {
	id                   int64
	initialReserve       string
	initialMarket        *string
	initialLiquidityMint string
}

type txreadsHoldingEvent struct {
	positionID              int64
	amountRaw               int64
	observedAt              time.Time
	observedSlot            int64
	eventType               string
	id                      int64
	liquidityMint           string
	market                  *string
	principalDeltaRaw       *int64
	reserve                 string
	sourceSignature         *string
	sourceDepositID         *int64
	sourceRebalanceDecision *int64
	sourceSnapshotID        *int64
	sourceWithdrawalID      *int64
}

// txreadsPositionEvent mirrors UserYieldPositionHistoryEventRecord.
type txreadsPositionEvent struct {
	amountRaw                int64
	confirmedAt              time.Time
	confirmedSlot            int64
	eventType                string
	id                       int64
	reserve                  string
	market                   *string
	principalDeltaRaw        *int64
	rebalanceAmountRaw       *int64
	withdrawnAmountRaw       *int64
	liquidityMint            string
	sourceReserve            *string
	sourceMarket             *string
	sourceLiquidityMint      *string
	destinationReserve       *string
	destinationMarket        *string
	destinationLiquidityMint *string
	principalAmountRaw       int64
	positionID               int64
	signature                string
	typ                      string // deposit | withdrawal | rebalance | reconciliation
}

// txreadsAutodepositEvent mirrors EarnAutodepositHistoryEventRecord.
type txreadsAutodepositEvent struct {
	actionType            string // balance_sweep | close | create
	amountRaw             int64
	confirmedAt           time.Time
	confirmedSlot         int64
	depositSignature      *string
	id                    string
	liquidityMint         string
	policyAccount         string
	recurringDelegation   *string
	signature             string
	walletBalanceFloorRaw *int64
}

const txreadsPositionsSQL = `SELECT id, initial_reserve, initial_market, initial_liquidity_mint
FROM loyal_yield.user_yield_positions
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3
ORDER BY updated_at DESC, id DESC`

const txreadsHoldingEventsSQL = `SELECT position_id, amount_raw, observed_at, observed_slot, event_type,
  id, liquidity_mint, market, principal_delta_raw, reserve, source_signature,
  source_deposit_id, source_rebalance_decision_id, source_snapshot_id,
  source_withdrawal_id
FROM loyal_yield.user_yield_position_holding_events
WHERE position_id = ANY($1)`

const txreadsDepositsSQL = `SELECT id, principal_amount_raw
FROM loyal_yield.user_yield_position_deposits WHERE id = ANY($1)`

const txreadsRebalanceAmountsSQL = `SELECT id, amount_raw
FROM loyal_yield.rebalance_decisions WHERE id = ANY($1)`

const txreadswithdrawalsSQL = `SELECT id, mode, withdrawn_amount_raw
FROM loyal_yield.user_yield_position_withdrawals WHERE id = ANY($1)`

// yieldPositionHistoryEvents ports findYieldPositionHistoryEventsForVault:
// the positions of the vault, then each position's holding events replayed in
// chronological order to rebuild the running principal and the movement
// source/destination columns, then sorted descending.
func (h earnRoutes) yieldPositionHistoryEvents(ctx context.Context, account EarnAccount) ([]txreadsPositionEvent, error) {
	seeds, err := h.positionSeeds(ctx, account)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}
	positionIDs := make([]int64, len(seeds))
	for i, seed := range seeds {
		positionIDs[i] = seed.id
	}
	eventsByPosition, err := h.holdingEvents(ctx, positionIDs)
	if err != nil {
		return nil, err
	}
	depositPrincipal, err := h.depositPrincipalByID(ctx, eventsByPosition)
	if err != nil {
		return nil, err
	}
	rebalanceAmount, err := h.rebalanceAmountByID(ctx, eventsByPosition)
	if err != nil {
		return nil, err
	}
	withdrawalByID, err := h.withdrawalsByID(ctx, eventsByPosition)
	if err != nil {
		return nil, err
	}

	var history []txreadsPositionEvent
	for _, seed := range seeds {
		events := eventsByPosition[seed.id]
		history = append(history, replayPositionHistory(seed, events, depositPrincipal, rebalanceAmount, withdrawalByID)...)
	}
	sortPositionEventsDescending(history)
	return history, nil
}

func (h earnRoutes) positionSeeds(ctx context.Context, account EarnAccount) ([]txreadsPositionSeed, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadsPositionsSQL,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seeds []txreadsPositionSeed
	for rows.Next() {
		var seed txreadsPositionSeed
		if err := rows.Scan(&seed.id, &seed.initialReserve, &seed.initialMarket, &seed.initialLiquidityMint); err != nil {
			return nil, err
		}
		seeds = append(seeds, seed)
	}
	return seeds, rows.Err()
}

func (h earnRoutes) holdingEvents(ctx context.Context, positionIDs []int64) (map[int64][]txreadsHoldingEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadsHoldingEventsSQL, positionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byPosition := map[int64][]txreadsHoldingEvent{}
	for rows.Next() {
		var event txreadsHoldingEvent
		if err := rows.Scan(&event.positionID, &event.amountRaw, &event.observedAt, &event.observedSlot,
			&event.eventType, &event.id, &event.liquidityMint, &event.market, &event.principalDeltaRaw,
			&event.reserve, &event.sourceSignature, &event.sourceDepositID, &event.sourceRebalanceDecision,
			&event.sourceSnapshotID, &event.sourceWithdrawalID); err != nil {
			return nil, err
		}
		byPosition[event.positionID] = append(byPosition[event.positionID], event)
	}
	return byPosition, rows.Err()
}

func (h earnRoutes) depositPrincipalByID(ctx context.Context, eventsByPosition map[int64][]txreadsHoldingEvent) (map[int64]int64, error) {
	var ids []int64
	for _, events := range eventsByPosition {
		for _, event := range events {
			if event.sourceDepositID != nil {
				ids = append(ids, *event.sourceDepositID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadsDepositsSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]int64{}
	for rows.Next() {
		var id, principal int64
		if err := rows.Scan(&id, &principal); err != nil {
			return nil, err
		}
		byID[id] = principal
	}
	return byID, rows.Err()
}

func (h earnRoutes) rebalanceAmountByID(ctx context.Context, eventsByPosition map[int64][]txreadsHoldingEvent) (map[int64]int64, error) {
	var ids []int64
	for _, events := range eventsByPosition {
		for _, event := range events {
			if event.sourceRebalanceDecision != nil {
				ids = append(ids, *event.sourceRebalanceDecision)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadsRebalanceAmountsSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]int64{}
	for rows.Next() {
		var id, amount int64
		if err := rows.Scan(&id, &amount); err != nil {
			return nil, err
		}
		byID[id] = amount
	}
	return byID, rows.Err()
}

type txreadsWithdrawal struct {
	mode               string
	withdrawnAmountRaw int64
}

func (h earnRoutes) withdrawalsByID(ctx context.Context, eventsByPosition map[int64][]txreadsHoldingEvent) (map[int64]txreadsWithdrawal, error) {
	var ids []int64
	for _, events := range eventsByPosition {
		for _, event := range events {
			if event.sourceWithdrawalID != nil {
				ids = append(ids, *event.sourceWithdrawalID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadswithdrawalsSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]txreadsWithdrawal{}
	for rows.Next() {
		var id int64
		var withdrawal txreadsWithdrawal
		if err := rows.Scan(&id, &withdrawal.mode, &withdrawal.withdrawnAmountRaw); err != nil {
			return nil, err
		}
		byID[id] = withdrawal
	}
	return byID, rows.Err()
}

// replayPositionHistory is findYieldPositionHistoryEventsForPosition's map:
// chronological walk, running principal, movement provenance.
func replayPositionHistory(seed txreadsPositionSeed, events []txreadsHoldingEvent,
	depositPrincipal, rebalanceAmount map[int64]int64, withdrawalByID map[int64]txreadsWithdrawal) []txreadsPositionEvent {
	sortHoldingEventsChronologically(events)
	previousReserve := seed.initialReserve
	previousMarket := seed.initialMarket
	previousLiquidityMint := seed.initialLiquidityMint
	principalAmount := int64(0)

	history := make([]txreadsPositionEvent, 0, len(events))
	for _, event := range events {
		typ := "deposit"
		switch event.eventType {
		case "rebalance_confirmed":
			typ = "rebalance"
		case "snapshot_reconciled":
			typ = "reconciliation"
		case "withdrawal_partial", "withdrawal_full":
			typ = "withdrawal"
		}
		record := txreadsPositionEvent{
			amountRaw:         event.amountRaw,
			confirmedAt:       event.observedAt,
			confirmedSlot:     event.observedSlot,
			eventType:         event.eventType,
			id:                event.id,
			reserve:           event.reserve,
			market:            event.market,
			principalDeltaRaw: event.principalDeltaRaw,
			liquidityMint:     event.liquidityMint,
			positionID:        event.positionID,
			typ:               typ,
		}
		if typ == "rebalance" || typ == "reconciliation" {
			sourceReserve, sourceMarket, sourceLiquidityMint := previousReserve, previousMarket, previousLiquidityMint
			record.sourceReserve = &sourceReserve
			record.sourceMarket = sourceMarket
			record.sourceLiquidityMint = &sourceLiquidityMint
			record.destinationReserve = &event.reserve
			record.destinationMarket = event.market
			record.destinationLiquidityMint = &event.liquidityMint
		}
		switch typ {
		case "deposit":
			principalDelta := event.principalDeltaRaw
			if event.sourceDepositID != nil {
				if found, ok := depositPrincipal[*event.sourceDepositID]; ok {
					principalDelta = &found
				}
			}
			if principalDelta != nil && *principalDelta > 0 {
				principalAmount += *principalDelta
			}
		case "withdrawal":
			var withdrawal txreadsWithdrawal
			hasWithdrawal := false
			if event.sourceWithdrawalID != nil {
				withdrawal, hasWithdrawal = withdrawalByID[*event.sourceWithdrawalID]
			}
			if (hasWithdrawal && withdrawal.mode == "full") || event.eventType == "withdrawal_full" {
				principalAmount = 0
			} else {
				principalDelta := int64(0)
				if event.principalDeltaRaw != nil {
					principalDelta = *event.principalDeltaRaw
				}
				withdrawnPrincipal := int64(0)
				if hasWithdrawal {
					withdrawnPrincipal = withdrawal.withdrawnAmountRaw
				} else if principalDelta < 0 {
					withdrawalAmt := -principalDelta
					withdrawnPrincipal = withdrawalAmt
				}
				if principalAmount > withdrawnPrincipal {
					principalAmount -= withdrawnPrincipal
				} else {
					principalAmount = 0
				}
			}
			if event.sourceWithdrawalID != nil {
				if withdrawal, ok := withdrawalByID[*event.sourceWithdrawalID]; ok {
					amount := withdrawal.withdrawnAmountRaw
					record.withdrawnAmountRaw = &amount
				}
			}
		}
		// The record carries the running principal after this event applied:
		// the TS literal is built after the same update.
		record.principalAmountRaw = principalAmount
		if event.sourceRebalanceDecision != nil {
			if amount, ok := rebalanceAmount[*event.sourceRebalanceDecision]; ok {
				record.rebalanceAmountRaw = &amount
			}
		}
		if event.sourceSignature != nil {
			record.signature = *event.sourceSignature
		} else {
			record.signature = txreadsSyntheticSignature(typ, event)
		}
		previousReserve = event.reserve
		previousMarket = event.market
		previousLiquidityMint = event.liquidityMint
		history = append(history, record)
	}
	return history
}

// txreadsSyntheticSignature is the formatter's fallback
// [type, depositId, withdrawalId, decisionId, snapshotId].filter(Boolean).join(":").
func txreadsSyntheticSignature(typ string, event txreadsHoldingEvent) string {
	parts := []string{typ}
	for _, id := range []*int64{event.sourceDepositID, event.sourceWithdrawalID, event.sourceRebalanceDecision, event.sourceSnapshotID} {
		if id != nil {
			parts = append(parts, pgidString(*id))
		}
	}
	return joinColons(parts)
}

func (h earnRoutes) autodepositHistoryEvents(ctx context.Context, account EarnAccount) ([]txreadsAutodepositEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `SELECT id, policy_account, token_mint, policy_signature,
  policy_confirmed_slot, close_signature, close_slot, closed_at, recurring_delegation,
  wallet_balance_floor_raw, first_seen_at
FROM loyal_yield.balance_sweep_targets
WHERE settings = $1 AND vault_index = $2 AND wallet = $3`,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []txreadsAutodepositEvent
	for rows.Next() {
		var (
			id              int64
			policyAccount   string
			tokenMint       string
			policySignature *string
			policySeenSlot  *int64
			closeSignature  *string
			closeSlot       *int64
			closedAt        *time.Time
			recurringDeleg  *string
			walletFloor     *int64
			firstSeenAt     time.Time
		)
		if err := rows.Scan(&id, &policyAccount, &tokenMint, &policySignature, &policySeenSlot,
			&closeSignature, &closeSlot, &closedAt, &recurringDeleg, &walletFloor, &firstSeenAt); err != nil {
			return nil, err
		}
		if policySignature != nil && *policySignature != "" && policySeenSlot != nil {
			events = append(events, txreadsAutodepositEvent{
				actionType: "create", amountRaw: 0, confirmedAt: firstSeenAt, confirmedSlot: *policySeenSlot,
				depositSignature: nil, id: "autodeposit:create:" + pgidString(id), liquidityMint: tokenMint,
				policyAccount: policyAccount, recurringDelegation: recurringDeleg,
				signature: *policySignature, walletBalanceFloorRaw: walletFloor,
			})
		}
		if closeSignature != nil && closeSlot != nil && closedAt != nil {
			events = append(events, txreadsAutodepositEvent{
				actionType: "close", amountRaw: 0, confirmedAt: *closedAt, confirmedSlot: *closeSlot,
				depositSignature: nil, id: "autodeposit:close:" + pgidString(id), liquidityMint: tokenMint,
				policyAccount: policyAccount, recurringDelegation: recurringDeleg,
				signature: *closeSignature, walletBalanceFloorRaw: walletFloor,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sweeps, err := h.autodepositSweepEvents(ctx, account)
	if err != nil {
		return nil, err
	}
	events = append(events, sweeps...)
	sortAutodepositEventsDescending(events)
	return events, nil
}

const txreadsSweepsSQL = `SELECT e.id, e.signature, e.slot, e.amount_raw, e.received_at, e.decoded_at,
  e.decoded_evidence, t.token_mint, t.policy_account, t.recurring_delegation, t.wallet_balance_floor_raw
FROM loyal_yield.balance_sweep_executions e
INNER JOIN loyal_yield.balance_sweep_targets t ON t.id = e.target_id
WHERE t.settings = $1 AND t.vault_index = $2 AND t.wallet = $3`

func (h earnRoutes) autodepositSweepEvents(ctx context.Context, account EarnAccount) ([]txreadsAutodepositEvent, error) {
	rows, err := h.YieldDB.Query(ctx, txreadsSweepsSQL,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []txreadsAutodepositEvent
	for rows.Next() {
		var (
			id              int64
			signature       string
			slot            int64
			amountRaw       int64
			receivedAt      time.Time
			decodedAt       *time.Time
			decodedEvidence []byte
			tokenMint       string
			policyAccount   string
			recurringDeleg  *string
			walletFloor     *int64
		)
		if err := rows.Scan(&id, &signature, &slot, &amountRaw, &receivedAt, &decodedAt,
			&decodedEvidence, &tokenMint, &policyAccount, &recurringDeleg, &walletFloor); err != nil {
			return nil, err
		}
		confirmedAt := receivedAt
		if decodedAt != nil {
			confirmedAt = *decodedAt
		}
		events = append(events, txreadsAutodepositEvent{
			actionType:            "balance_sweep",
			amountRaw:             amountRaw,
			confirmedAt:           confirmedAt,
			confirmedSlot:         slot,
			depositSignature:      txreadsKaminoDepositSignature(decodedEvidence),
			id:                    "autodeposit:sweep:" + pgidString(id),
			liquidityMint:         tokenMint,
			policyAccount:         policyAccount,
			recurringDelegation:   recurringDeleg,
			signature:             signature,
			walletBalanceFloorRaw: walletFloor,
		})
	}
	return events, rows.Err()
}

func txreadsKaminoDepositSignature(decodedEvidence []byte) *string {
	if decodedEvidence == nil {
		return nil
	}
	var evidence struct {
		KaminoDepositSignature any `json:"kaminoDepositSignature"`
	}
	if json.Unmarshal(decodedEvidence, &evidence) != nil {
		return nil
	}
	if signature, ok := evidence.KaminoDepositSignature.(string); ok {
		return &signature
	}
	return nil
}
