package autodeposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// ClaimStatus mirrors loyal_yield.balance_sweep_lot_claim_status.
type ClaimStatus string

const (
	ClaimSelected ClaimStatus = "selected"
	ClaimExecuted ClaimStatus = "executed"
	ClaimReleased ClaimStatus = "released"
	ClaimFailed   ClaimStatus = "failed"
)

// ClaimOutcome is the result of one claim attempt for a target. A noop carries
// the reason the wallet keeps its money this round.
type ClaimOutcome struct {
	Status            ClaimStatus
	Reason            string
	ClaimToken        string
	TargetID          int64
	AmountRaw         int64
	StaleCheckEventID int64
	Lots              []SelectedLot
}

// SweepDecisionReason maps a typed sweep decision onto the claim vocabulary the
// legacy executor already reports.
func SweepDecisionReason(decision SweepAmountDecision) string {
	switch decision.Kind {
	case SweepNoEligibleLots:
		return "no_eligible_lots"
	case SweepNoWalletExcess:
		return "wallet_balance_not_above_floor"
	case SweepAllowanceExhaust:
		return "allowance_exhausted"
	default:
		return ""
	}
}

// ClaimEligibleLotsOnce takes exclusive custody of one target's schedulable
// surplus for a pull, inside one transaction.
//
// The sweep amount comes from the same typed decision function the replay
// verifier uses, but it is computed over lots locked by this transaction, so a
// concurrent claim on another worker sees them gone. The claim row, its lot
// items, the decremented lots, the residual slot and the selected slot commit
// atomically: custody is never half-taken.
func (s *Store) ClaimEligibleLotsOnce(ctx context.Context, targetID int64, claimToken string, scheduledSlotID *int64, walletBalanceRaw, walletBalanceFloorRaw int64, maxAmountPerPeriodRaw, remainingAllowanceRaw *int64) (ClaimOutcome, error) {
	var outcome ClaimOutcome
	if s == nil || s.pool == nil {
		return outcome, errors.New("autodeposit store has no database pool")
	}
	if claimToken == "" {
		return outcome, errors.New("autodeposit claim requires a claim token")
	}
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, err := loadExistingClaim(ctx, tx, claimToken, targetID)
		if err != nil {
			return err
		}
		if existing != nil {
			outcome = *existing
			return nil
		}

		targetActive, err := lockActiveTarget(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if !targetActive {
			outcome = noopClaim(targetID, "target_not_active")
			return nil
		}
		// The user can change protection settings after the RPC/context read.
		// Read the authoritative settings while the target row stays locked.
		var currentFloor, currentMax *int64
		if err := tx.QueryRow(ctx, `
SELECT wallet_balance_floor_raw, max_amount_per_period
FROM loyal_yield.balance_sweep_targets WHERE id = $1`, targetID).Scan(&currentFloor, &currentMax); err != nil {
			return fmt.Errorf("read locked autodeposit protection settings: %w", err)
		}
		if currentFloor == nil || *currentFloor != walletBalanceFloorRaw {
			outcome = noopClaim(targetID, "wallet_balance_floor_changed")
			return nil
		}
		if currentMax != nil && (maxAmountPerPeriodRaw == nil || *currentMax < *maxAmountPerPeriodRaw) {
			maxAmountPerPeriodRaw = currentMax
		}
		// The target row serializes independent claim tokens. A loader's
		// earlier check is insufficient when two executors share a vault.
		var selectedClaimExists bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM loyal_yield.balance_sweep_lot_claims
  WHERE target_id = $1 AND status = 'selected'
)`, targetID).Scan(&selectedClaimExists); err != nil {
			return fmt.Errorf("check selected autodeposit claim: %w", err)
		}
		if selectedClaimExists {
			outcome = noopClaim(targetID, "target_has_selected_claim")
			return nil
		}

		staleCheckEventID, err := currentTargetEventID(ctx, tx, targetID)
		if err != nil {
			return err
		}
		processedEventID, err := projectionOffsetForStaleness(ctx, tx)
		if err != nil {
			return err
		}
		if processedEventID < staleCheckEventID {
			outcome = ClaimOutcome{
				Status:            ClaimNoopStatus(),
				Reason:            "newer_unprocessed_wallet_event",
				TargetID:          targetID,
				StaleCheckEventID: staleCheckEventID,
			}
			return nil
		}

		if scheduledSlotID != nil {
			available, err := lockExecutableSlot(ctx, tx, targetID, *scheduledSlotID)
			if err != nil {
				return err
			}
			if !available {
				outcome = noopClaim(targetID, "scheduled_slot_not_available")
				return nil
			}
		}

		openLots, err := lockEligibleLots(ctx, tx, targetID, scheduledSlotID)
		if err != nil {
			return err
		}
		var eligibleLotAmountRaw int64
		for _, lot := range openLots {
			var ok bool
			eligibleLotAmountRaw, ok = addChecked(eligibleLotAmountRaw, lot.RemainingAmountRaw)
			if !ok || lot.RemainingAmountRaw < 0 {
				return errors.New("autodeposit eligible lot amount overflow or invalid lot")
			}
		}
		decision := ComputeSweepAmount(SweepCaps{
			EligibleLotAmountRaw:  eligibleLotAmountRaw,
			WalletBalanceRaw:      walletBalanceRaw,
			WalletBalanceFloorRaw: walletBalanceFloorRaw,
			MaxAmountPerPeriodRaw: maxAmountPerPeriodRaw,
			RemainingAllowanceRaw: remainingAllowanceRaw,
		})
		if decision.Kind != SweepGo {
			outcome = ClaimOutcome{
				Status:            ClaimNoopStatus(),
				Reason:            SweepDecisionReason(decision),
				TargetID:          targetID,
				StaleCheckEventID: staleCheckEventID,
			}
			return nil
		}

		remainingToClaim := decision.AmountRaw
		var claimed []SelectedLot
		for _, lot := range openLots {
			if remainingToClaim == 0 {
				break
			}
			claimAmount := min64(remainingToClaim, lot.RemainingAmountRaw)
			claimed = append(claimed, SelectedLot{LotID: lot.ID, AmountRaw: claimAmount})
			remainingToClaim -= claimAmount
		}
		if remainingToClaim != 0 {
			outcome = noopClaim(targetID, "insufficient_locked_lots")
			return nil
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_lot_claims
    (claim_token, target_id, amount_raw, status, stale_check_event_id)
VALUES ($1, $2, $3, 'selected', $4)`,
			claimToken, targetID, decision.AmountRaw, staleCheckEventID); err != nil {
			return fmt.Errorf("insert autodeposit lot claim: %w", err)
		}
		for _, item := range claimed {
			if _, err := tx.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_lot_claim_items
    (claim_token, lot_id, amount_raw)
VALUES ($1, $2, $3)`, claimToken, item.LotID, item.AmountRaw); err != nil {
				return fmt.Errorf("insert autodeposit claim item: %w", err)
			}
			tag, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_surplus_lots
SET remaining_amount_raw = remaining_amount_raw - $2,
    status = CASE
        WHEN remaining_amount_raw - $2 = 0 THEN 'consumed'::loyal_yield.balance_sweep_surplus_lot_status
        ELSE 'open'::loyal_yield.balance_sweep_surplus_lot_status
    END,
    updated_at = now()
WHERE id = $1
  AND remaining_amount_raw >= $2`, item.LotID, item.AmountRaw)
			if err != nil {
				return fmt.Errorf("consume autodeposit lot %d: %w", item.LotID, err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("autodeposit lot %d moved while being claimed", item.LotID)
			}
		}

		if err := moveResidualOpenLotsToNextSlot(ctx, tx, scheduledSlotID); err != nil {
			return err
		}
		if err := markSlotSelected(ctx, tx, scheduledSlotID, claimToken); err != nil {
			return err
		}

		outcome = ClaimOutcome{
			Status:            ClaimSelected,
			ClaimToken:        claimToken,
			TargetID:          targetID,
			AmountRaw:         decision.AmountRaw,
			StaleCheckEventID: staleCheckEventID,
			Lots:              claimed,
		}
		return nil
	})
	if err != nil {
		return ClaimOutcome{}, err
	}
	return outcome, nil
}

// ClaimNoopStatus is the not-actionable claim status.
func ClaimNoopStatus() ClaimStatus { return "noop" }

func noopClaim(targetID int64, reason string) ClaimOutcome {
	return ClaimOutcome{Status: ClaimNoopStatus(), Reason: reason, TargetID: targetID}
}

// lockActiveTarget locks the target row and reports observed-and-desired
// eligibility. A paused desired state or a non-active chain status both refuse:
// the pause path must also keep recovery available, which loadAutodepositPull
// recovery rows do not consult here.
func lockActiveTarget(ctx context.Context, tx pgx.Tx, targetID int64) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `
SELECT COALESCE(desired_active AND chain_status = 'active' AND cluster='mainnet-beta'
AND EXISTS(SELECT 1 FROM loyal_yield.managed_vaults mv JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id WHERE mv.active AND mv.settings=balance_sweep_targets.settings AND mv.vault_index=balance_sweep_targets.vault_index AND mv.vault_pubkey=balance_sweep_targets.vault_pubkey AND rp.active AND rp.cluster='mainnet-beta' AND rp.authority=balance_sweep_targets.authority AND 'same_mint_kamino'=ANY(rp.route_modes)), false)
FROM loyal_yield.balance_sweep_targets
WHERE id = $1
  AND token_mint = $2
FOR UPDATE`, targetID, USDCMint).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock autodeposit target %d: %w", targetID, err)
	}
	return active, nil
}

func currentTargetEventID(ctx context.Context, tx pgx.Tx, targetID int64) (int64, error) {
	var eventID int64
	err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(event_id), 0)
FROM loyal_yield.balance_sweep_wallet_balance_events AS event
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = event.target_id
WHERE event.target_id = $1
  AND event.mint = target.token_mint
  AND target.token_mint = $2`, targetID, USDCMint).Scan(&eventID)
	if err != nil {
		return 0, fmt.Errorf("read autodeposit target event id: %w", err)
	}
	return eventID, nil
}

func projectionOffsetForStaleness(ctx context.Context, tx pgx.Tx) (int64, error) {
	var offset int64
	err := tx.QueryRow(ctx, `
SELECT COALESCE(last_event_id, 0)
FROM loyal_yield.projection_offsets
WHERE consumer_name = $1`, ConsumerName).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read autodeposit projection offset: %w", err)
	}
	return offset, nil
}

func lockExecutableSlot(ctx context.Context, tx pgx.Tx, targetID, scheduledSlotID int64) (bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `
SELECT id
FROM loyal_yield.balance_sweep_scheduled_slots
WHERE id = $1
  AND target_id = $2
  AND token_mint = $3
  AND status IN ('scheduled', 'requested')
  AND eligible_after <= now()
FOR UPDATE`, scheduledSlotID, targetID, USDCMint).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock autodeposit slot %d: %w", scheduledSlotID, err)
	}
	return true, nil
}

type lockedLot struct {
	ID                 int64
	RemainingAmountRaw int64
}

func lockEligibleLots(ctx context.Context, tx pgx.Tx, targetID int64, scheduledSlotID *int64) ([]lockedLot, error) {
	rows, err := tx.Query(ctx, `
SELECT lot.id, lot.remaining_amount_raw
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = $1
  AND event.mint = target.token_mint
  AND target.token_mint = $2
  AND lot.status = 'open'
  AND lot.remaining_amount_raw > 0
  AND ($3::bigint IS NOT NULL OR lot.eligible_after <= now())
  AND ($3::bigint IS NULL OR lot.scheduled_slot_id = $3::bigint)
ORDER BY lot.eligible_after ASC, lot.created_at ASC, lot.id ASC
FOR UPDATE SKIP LOCKED`, targetID, USDCMint, scheduledSlotID)
	if err != nil {
		return nil, fmt.Errorf("lock autodeposit eligible lots: %w", err)
	}
	defer rows.Close()
	var lots []lockedLot
	for rows.Next() {
		var lot lockedLot
		if err := rows.Scan(&lot.ID, &lot.RemainingAmountRaw); err != nil {
			return nil, fmt.Errorf("scan autodeposit eligible lot: %w", err)
		}
		lots = append(lots, lot)
	}
	return lots, rows.Err()
}

// moveResidualOpenLotsToNextSlot keeps unswept open lots schedulable in a fresh
// slot once their slot is taken by a claim, so a partial sweep never strands the
// remainder behind a selected slot.
func moveResidualOpenLotsToNextSlot(ctx context.Context, tx pgx.Tx, scheduledSlotID *int64) error {
	if scheduledSlotID == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `
WITH residual AS (
    SELECT
        slot.target_id,
        slot.token_mint,
        MAX(lot.eligible_after) AS eligible_after
    FROM loyal_yield.balance_sweep_scheduled_slots AS slot
    JOIN loyal_yield.balance_sweep_surplus_lots AS lot
      ON lot.scheduled_slot_id = slot.id
    WHERE slot.id = $1
      AND lot.status = 'open'
      AND lot.remaining_amount_raw > 0
    GROUP BY slot.target_id, slot.token_mint
),
inserted_slot AS (
    INSERT INTO loyal_yield.balance_sweep_scheduled_slots
        (target_id, token_mint, eligible_after, status)
    SELECT target_id, token_mint, eligible_after, 'scheduled'
    FROM residual
    RETURNING id
)
UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET scheduled_slot_id = inserted_slot.id,
    updated_at = now()
FROM inserted_slot
WHERE lot.scheduled_slot_id = $1
  AND lot.status = 'open'
  AND lot.remaining_amount_raw > 0`, *scheduledSlotID)
	if err != nil {
		return fmt.Errorf("reschedule residual autodeposit lots: %w", err)
	}
	_ = tag
	return nil
}

func markSlotSelected(ctx context.Context, tx pgx.Tx, scheduledSlotID *int64, claimToken string) error {
	_, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET status = 'selected',
    claim_token = $2,
    last_error = NULL,
    updated_at = now()
WHERE (
      $1::bigint IS NOT NULL
      AND slot.id = $1::bigint
      AND slot.status IN ('scheduled', 'requested')
)
   OR (
      $1::bigint IS NULL
      AND slot.id IN (
          SELECT DISTINCT lot.scheduled_slot_id
          FROM loyal_yield.balance_sweep_lot_claim_items AS item
          JOIN loyal_yield.balance_sweep_surplus_lots AS lot
            ON lot.id = item.lot_id
          WHERE item.claim_token = $2
            AND lot.scheduled_slot_id IS NOT NULL
      )
   )`, scheduledSlotID, claimToken)
	if err != nil {
		return fmt.Errorf("mark autodeposit slot selected: %w", err)
	}
	return nil
}

// LoadExistingClaim returns the durable state of a claim token, or nil when the
// token is unknown. Recovery paths call this before doing anything else so a
// retried execution resumes its own claim instead of taking a new one.
func (s *Store) LoadExistingClaim(ctx context.Context, claimToken string, targetID int64) (*ClaimOutcome, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("autodeposit store has no database pool")
	}
	var outcome *ClaimOutcome
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		loaded, err := loadExistingClaim(ctx, tx, claimToken, targetID)
		if err != nil {
			return err
		}
		outcome = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

func loadExistingClaim(ctx context.Context, tx pgx.Tx, claimToken string, targetID int64) (*ClaimOutcome, error) {
	byToken, err := loadExistingClaimByToken(ctx, tx, claimToken)
	if err != nil {
		return nil, err
	}
	if byToken == nil {
		return nil, nil
	}
	if byToken.TargetID != targetID {
		mismatch := noopClaim(targetID, "claim_token_target_mismatch")
		return &mismatch, nil
	}
	return byToken, nil
}

func loadExistingClaimByToken(ctx context.Context, tx pgx.Tx, claimToken string) (*ClaimOutcome, error) {
	var (
		status       string
		targetID     int64
		amountRaw    int64
		staleCheckID int64
	)
	err := tx.QueryRow(ctx, `
SELECT claim.claim_token, claim.target_id, claim.amount_raw, claim.status::text AS status, claim.stale_check_event_id
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = claim.target_id
WHERE claim.claim_token = $1
  AND target.token_mint = $2
FOR UPDATE`, claimToken, USDCMint).Scan(&claimToken, &targetID, &amountRaw, &status, &staleCheckID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load autodeposit claim %s: %w", claimToken, err)
	}

	rows, err := tx.Query(ctx, `
SELECT item.lot_id, item.amount_raw
FROM loyal_yield.balance_sweep_lot_claim_items AS item
JOIN loyal_yield.balance_sweep_surplus_lots AS lot
  ON lot.id = item.lot_id
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_lot_claims AS claim
  ON claim.claim_token = item.claim_token
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = claim.target_id
WHERE item.claim_token = $1
  AND lot.target_id = claim.target_id
  AND event.mint = target.token_mint
  AND target.token_mint = $2
ORDER BY item.lot_id ASC`, claimToken, USDCMint)
	if err != nil {
		return nil, fmt.Errorf("load autodeposit claim items: %w", err)
	}
	defer rows.Close()
	var lots []SelectedLot
	for rows.Next() {
		var lot SelectedLot
		if err := rows.Scan(&lot.LotID, &lot.AmountRaw); err != nil {
			return nil, fmt.Errorf("scan autodeposit claim item: %w", err)
		}
		lots = append(lots, lot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &ClaimOutcome{
		Status:            ClaimStatus(status),
		ClaimToken:        claimToken,
		TargetID:          targetID,
		AmountRaw:         amountRaw,
		StaleCheckEventID: staleCheckID,
		Lots:              lots,
	}, nil
}

// CompleteClaimOnce links a finished claim to its execution and closes the slot
// in one transaction. A claim that is no longer selected reports noop with a
// reason instead of rewriting someone else's custody.
func (s *Store) CompleteClaimOnce(ctx context.Context, claimToken string, executionID int64) (ClaimOutcome, error) {
	var outcome ClaimOutcome
	if s == nil || s.pool == nil {
		return outcome, errors.New("autodeposit store has no database pool")
	}
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, err := loadExistingClaimByToken(ctx, tx, claimToken)
		if err != nil {
			return err
		}
		if existing == nil {
			outcome = ClaimOutcome{Status: ClaimNoopStatus(), Reason: "claim_not_found"}
			return nil
		}
		outcome = *existing
		if outcome.Status != ClaimSelected {
			outcome.Reason = "claim_not_selected"
			return nil
		}
		if len(outcome.Lots) == 0 {
			outcome.Reason = "claim_has_no_supported_lots"
			return nil
		}
		tag, err := tx.Exec(ctx, `
WITH matched_lots AS (
    SELECT item.lot_id, item.amount_raw
    FROM loyal_yield.balance_sweep_lot_claim_items AS item
    JOIN loyal_yield.balance_sweep_surplus_lots AS lot
      ON lot.id = item.lot_id
    JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
      ON event.event_id = lot.source_event_id
    JOIN loyal_yield.balance_sweep_lot_claims AS claim
      ON claim.claim_token = item.claim_token
    JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = claim.target_id
    WHERE item.claim_token = $1
      AND lot.target_id = claim.target_id
      AND event.mint = target.token_mint
      AND target.token_mint = $3
),
inserted AS (
    INSERT INTO loyal_yield.balance_sweep_execution_lots
        (execution_id, lot_id, amount_raw)
    SELECT $2, lot_id, amount_raw
    FROM matched_lots
    ON CONFLICT (execution_id, lot_id) DO NOTHING
    RETURNING lot_id
)
UPDATE loyal_yield.balance_sweep_lot_claims
SET status = 'executed',
    execution_id = $2,
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND EXISTS (SELECT 1 FROM matched_lots)`, claimToken, executionID, USDCMint)
		if err != nil {
			return fmt.Errorf("complete autodeposit claim: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_scheduled_slots
SET status = 'executed',
    execution_id = $2,
    updated_at = now()
WHERE claim_token = $1`, claimToken, executionID); err != nil {
				return fmt.Errorf("complete autodeposit slot: %w", err)
			}
		}
		if tag.RowsAffected() == 0 {
			outcome.Reason = "claim_has_no_supported_lots"
			return nil
		}
		outcome.Status = ClaimExecuted
		return nil
	})
	if err != nil {
		return ClaimOutcome{}, err
	}
	return outcome, nil
}

// ReleaseClaimOnce returns an unspent claim's lots to the open pool and fails
// the slot. The locked claim must still belong to this live executor lease,
// and no pull attempt or execution may hold custody.
func (s *Store) ReleaseClaimOnce(ctx context.Context, claimToken, leaseToken string) (ClaimOutcome, error) {
	var outcome ClaimOutcome
	if s == nil || s.pool == nil {
		return outcome, errors.New("autodeposit store has no database pool")
	}
	if leaseToken == "" {
		return outcome, fmt.Errorf("%w: release requires the executor lease token", ErrOwnershipLost)
	}
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, err := loadExistingClaimByToken(ctx, tx, claimToken)
		if err != nil {
			return err
		}
		if existing == nil {
			outcome = ClaimOutcome{Status: ClaimNoopStatus(), Reason: "claim_not_found"}
			return nil
		}
		outcome = *existing
		if outcome.Status != ClaimSelected {
			outcome.Reason = "claim_not_selected"
			return nil
		}
		var ownsLease, unspent bool
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(autodeposit_executor_lease_token = $2
         AND autodeposit_executor_lease_expires_at > now(), false),
       execution_id IS NULL AND NOT EXISTS (
         SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
         WHERE attempt.claim_token = claim.claim_token
           AND (attempt.execution_id IS NOT NULL
                OR (attempt.operation_kind = 'pull'
                    AND attempt.attempt_state = ANY($3::text[])))
       )
FROM loyal_yield.balance_sweep_lot_claims AS claim
WHERE claim.claim_token = $1`, claimToken, leaseToken, ClaimHoldingPullAttemptStates).Scan(&ownsLease, &unspent); err != nil {
			return fmt.Errorf("check autodeposit claim release authority: %w", err)
		}
		if !ownsLease {
			return fmt.Errorf("%w: claim %s cannot be released by this executor", ErrOwnershipLost, claimToken)
		}
		if !unspent {
			return fmt.Errorf("%w: claim %s has a holding pull attempt or execution", ErrClaimCustodyHeld, claimToken)
		}
		if len(outcome.Lots) == 0 {
			outcome.Reason = "claim_has_no_supported_lots"
			return nil
		}
		tag, err := tx.Exec(ctx, `
WITH matched_items AS (
    SELECT item.lot_id, item.amount_raw
    FROM loyal_yield.balance_sweep_lot_claim_items AS item
    JOIN loyal_yield.balance_sweep_surplus_lots AS lot
      ON lot.id = item.lot_id
    JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
      ON event.event_id = lot.source_event_id
    JOIN loyal_yield.balance_sweep_lot_claims AS claim
      ON claim.claim_token = item.claim_token
    JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = claim.target_id
    WHERE item.claim_token = $1
      AND lot.target_id = claim.target_id
      AND event.mint = target.token_mint
      AND target.token_mint = $2
),
restored AS (
    UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
    SET remaining_amount_raw = LEAST(
            lot.original_amount_raw,
            lot.remaining_amount_raw + item.amount_raw
        ),
        status = 'open',
        updated_at = now()
    FROM matched_items AS item
    WHERE lot.id = item.lot_id
    RETURNING lot.id
)
UPDATE loyal_yield.balance_sweep_lot_claims
SET status = 'released',
	autodeposit_executor_lease_token = NULL,
	autodeposit_executor_lease_expires_at = NULL,
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND EXISTS (SELECT 1 FROM restored)`, claimToken, USDCMint)
		if err != nil {
			return fmt.Errorf("release autodeposit claim: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_scheduled_slots
SET status = 'failed',
    claim_token = NULL,
    last_error = 'claim released before autodeposit pull',
    updated_at = now()
WHERE claim_token = $1`, claimToken); err != nil {
				return fmt.Errorf("fail autodeposit slot after release: %w", err)
			}
		}
		if tag.RowsAffected() == 0 {
			outcome.Reason = "claim_has_no_supported_lots"
			return nil
		}
		outcome.Status = ClaimReleased
		return nil
	})
	if err != nil {
		return ClaimOutcome{}, err
	}
	return outcome, nil
}

// ErrClaimCustodyHeld distinguishes a financial hold from executor ownership.
var ErrClaimCustodyHeld = errors.New("autodeposit claim holds custody")

// LoadFrozenDepositPlan reads the claim's immutable deposit plan under the
// executor's live claim lease.
func (s *Store) LoadFrozenDepositPlan(ctx context.Context, claimToken string, targetID int64, leaseToken string) (DepositPlan, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT autodeposit_deposit_plan FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1 AND target_id=$2 AND status='selected' AND autodeposit_executor_lease_token=$3 AND autodeposit_executor_lease_expires_at>now()`, claimToken, targetID, leaseToken).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositPlan{}, ErrOwnershipLost
	}
	if err != nil {
		return DepositPlan{}, err
	}
	return UnmarshalDepositPlan(raw)
}
