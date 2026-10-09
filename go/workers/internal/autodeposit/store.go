package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Store owns the Autodeposit family's durable state in the existing
// loyal_yield schema. It talks to the same tables the legacy Rust trigger and
// TypeScript executor use, so a Go worker and a legacy worker contend on real
// rows rather than on a Go-private queue.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pool. The runtime owns pool lifecycle; the store
// only owns family SQL.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("autodeposit store requires a database pool")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// ProjectionOutcome reports one projection pass. The event-id window is the
// durable progress; the counts are observability only.
type ProjectionOutcome struct {
	PreviousEventID      int64
	LastEventID          int64
	EventsScanned        int
	LotsCreated          int
	OutflowAmountRaw     int64
	LotAmountDepletedRaw int64
}

type walletBalanceEventRow struct {
	EventID               int64
	TargetID              int64
	AmountRaw             int64
	DeltaAmountRaw        *int64
	ObservedAt            time.Time
	TxnSignature          *string
	TargetActive          bool
	WalletBalanceFloorRaw *int64
	// OwnWithdrawal marks an event carried by the vault's own Earn
	// withdrawal: the user moving yield back to the wallet.
	OwnWithdrawal bool
	// OwnPullRaw is the amount of this family's pull whose signature the
	// event carries; its claim already took that amount from the lots.
	OwnPullRaw int64
}

// ProjectSurplusLotsOnce advances the surplus-lot projection by one bounded
// batch, in one transaction. Lots and the projection offset commit together, so
// a crash cannot acknowledge events whose lots are missing, and a replay of an
// unacknowledged event is idempotent on its source event.
func (s *Store) ProjectSurplusLotsOnce(ctx context.Context, batchLimit int64) (ProjectionOutcome, error) {
	var outcome ProjectionOutcome
	if s == nil || s.pool == nil {
		return outcome, errors.New("autodeposit store has no database pool")
	}
	if batchLimit < 1 {
		return outcome, errors.New("autodeposit projection requires a positive batch limit")
	}
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		previousEventID, err := lockProjectionOffset(ctx, tx)
		if err != nil {
			return err
		}
		events, err := fetchEventsAfter(ctx, tx, previousEventID, batchLimit)
		if err != nil {
			return err
		}
		outcome = ProjectionOutcome{
			PreviousEventID: previousEventID,
			LastEventID:     previousEventID,
			EventsScanned:   len(events),
		}
		for _, event := range events {
			outcome.LastEventID = event.EventID
			if event.DeltaAmountRaw == nil {
				created, err := insertInitialSurplusLot(ctx, tx, event)
				if err != nil {
					return err
				}
				if created {
					outcome.LotsCreated++
				}
				continue
			}
			switch delta := *event.DeltaAmountRaw; {
			case delta > 0:
				created, err := insertPositiveDeltaLot(ctx, tx, event, delta)
				if err != nil {
					return err
				}
				if created {
					outcome.LotsCreated++
				}
			case delta < 0:
				outflow, ok := absChecked(delta)
				if !ok {
					return &LotError{Code: LotErrAmountOverflow}
				}
				// Our own pull is the claim's outflow, already consumed from
				// the claimed lots. Depleting it again wrote off twice each
				// deposit; the TS executor's re-claimed lots had masked that.
				external := outflow - min64(outflow, event.OwnPullRaw)
				depleted, err := depleteLotsNewestFirst(ctx, tx, event.TargetID, external)
				if err != nil {
					return err
				}
				outcome.OutflowAmountRaw += outflow
				outcome.LotAmountDepletedRaw += depleted
			}
		}
		if outcome.LastEventID > previousEventID {
			return advanceProjectionOffset(ctx, tx, outcome.LastEventID)
		}
		return nil
	})
	if err != nil {
		return ProjectionOutcome{}, err
	}
	return outcome, nil
}

func absChecked(value int64) (int64, bool) {
	if value < 0 && value == -value {
		return 0, false
	}
	if value < 0 {
		return -value, true
	}
	return value, true
}

// lockProjectionOffset takes the consumer's projection row and locks it for the
// pass. The upsert-then-lock pair is the legacy trigger's row claim; a second
// worker blocks here instead of double-projecting the same events.
func lockProjectionOffset(ctx context.Context, tx pgx.Tx) (int64, error) {
	var inserted int64
	if err := tx.QueryRow(ctx, `
INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
VALUES ($1, 0)
ON CONFLICT (consumer_name) DO UPDATE
SET consumer_name = EXCLUDED.consumer_name
RETURNING last_event_id`, ConsumerName).Scan(&inserted); err != nil {
		return 0, fmt.Errorf("open autodeposit projection offset: %w", err)
	}
	var locked int64
	if err := tx.QueryRow(ctx, `
SELECT last_event_id
FROM loyal_yield.projection_offsets
WHERE consumer_name = $1
FOR UPDATE`, ConsumerName).Scan(&locked); err != nil {
		return 0, fmt.Errorf("lock autodeposit projection offset: %w", err)
	}
	if locked > inserted {
		return locked, nil
	}
	return inserted, nil
}

func advanceProjectionOffset(ctx context.Context, tx pgx.Tx, eventID int64) error {
	tag, err := tx.Exec(ctx, `
UPDATE loyal_yield.projection_offsets
SET last_event_id = $2,
    updated_at = now()
WHERE consumer_name = $1`, ConsumerName, eventID)
	if err != nil {
		return fmt.Errorf("advance autodeposit projection offset: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("autodeposit projection offset row disappeared inside its pass")
	}
	return nil
}

func fetchEventsAfter(ctx context.Context, tx pgx.Tx, lastEventID, limit int64) ([]walletBalanceEventRow, error) {
	rows, err := tx.Query(ctx, `
SELECT
    event.event_id,
    event.target_id,
    event.amount_raw,
    event.delta_amount_raw,
    event.observed_at,
    event.txn_signature,
    target.desired_active
        AND target.chain_status = 'active'
        AND target.cluster = 'mainnet-beta'
        AND `+targetRoutedSQL+` AS target_active,
    target.wallet_balance_floor_raw,
    EXISTS (
        SELECT 1
        FROM loyal_yield.user_yield_position_withdrawals AS withdrawal
        WHERE withdrawal.withdrawal_signature = event.txn_signature
          AND withdrawal.settings = target.settings
          AND withdrawal.vault_index = target.vault_index
          AND withdrawal.vault_pubkey = target.vault_pubkey
    ) AS own_withdrawal,
    COALESCE((
        SELECT attempt.amount_raw
        FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
        WHERE attempt.signature = event.txn_signature
          AND attempt.target_id = event.target_id
          AND attempt.operation_kind = 'pull'
    ), 0) AS own_pull_raw
FROM loyal_yield.balance_sweep_wallet_balance_events AS event
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = event.target_id
WHERE event.event_id > $1
  AND event.mint = target.token_mint
  AND target.cluster = 'mainnet-beta'
  AND target.token_mint = $2
ORDER BY event.event_id ASC
LIMIT $3`, lastEventID, USDCMint, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch autodeposit wallet events: %w", err)
	}
	defer rows.Close()

	var events []walletBalanceEventRow
	for rows.Next() {
		var event walletBalanceEventRow
		if err := rows.Scan(&event.EventID, &event.TargetID, &event.AmountRaw, &event.DeltaAmountRaw,
			&event.ObservedAt, &event.TxnSignature, &event.TargetActive, &event.WalletBalanceFloorRaw, &event.OwnWithdrawal, &event.OwnPullRaw); err != nil {
			return nil, fmt.Errorf("scan autodeposit wallet event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// insertInitialSurplusLot schedules the first observed balance above the floor.
// A paused target, a missing floor, or a balance at or below the floor creates
// no lot: desired enablement alone is not observed eligibility.
func insertInitialSurplusLot(ctx context.Context, tx pgx.Tx, event walletBalanceEventRow) (bool, error) {
	if !event.TargetActive {
		return false, nil
	}
	amountRaw, ok := InitialSurplusAmount(event.AmountRaw, event.WalletBalanceFloorRaw)
	if !ok {
		return false, nil
	}
	return insertScheduledLot(ctx, tx, event, amountRaw,
		"derived",
		"initial wallet ATA balance above the configured floor scheduled for autodeposit after one hour")
}

// insertPositiveDeltaLot schedules an inflow. The user's own Earn withdrawal
// landing in the wallet is no inflow: depositing it again would undo the
// withdrawal. Earn retracts the lot instead when it records the withdrawal
// after this projection ran (SuppressWithdrawalLots).
func insertPositiveDeltaLot(ctx context.Context, tx pgx.Tx, event walletBalanceEventRow, deltaAmountRaw int64) (bool, error) {
	if !event.TargetActive || event.OwnWithdrawal {
		return false, nil
	}
	amountRaw, ok := PositiveDeltaSurplusAmount(event.AmountRaw, deltaAmountRaw, event.WalletBalanceFloorRaw)
	if !ok {
		return false, nil
	}
	return insertScheduledLot(ctx, tx, event, amountRaw,
		"derived",
		"wallet balance increase scheduled for autodeposit after one hour")
}

// insertScheduledLot files the new lot into the target's current open scheduled
// slot, coalescing the slot deadline to the later of the two. A batch due date
// never moves earlier because a newer lot arrived.
func insertScheduledLot(ctx context.Context, tx pgx.Tx, event walletBalanceEventRow, amountRaw int64, confidence, reason string) (bool, error) {
	eligibleAfter := ScheduledEligibleAfter(event.ObservedAt)
	rows, err := tx.Query(ctx, `
WITH current_slot AS (
    SELECT id
    FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE target_id = $1
      AND token_mint = $9
      AND status = 'scheduled'
    ORDER BY eligible_after ASC, id ASC
    LIMIT 1
    FOR UPDATE
),
updated_current_slot AS (
    UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
    SET eligible_after = GREATEST(slot.eligible_after, $6),
        updated_at = now()
    WHERE slot.id IN (SELECT id FROM current_slot)
    RETURNING slot.id
),
inserted_slot AS (
    INSERT INTO loyal_yield.balance_sweep_scheduled_slots
        (target_id, token_mint, eligible_after, status)
    SELECT $1, $9, $6, 'scheduled'
    WHERE NOT EXISTS (SELECT 1 FROM updated_current_slot)
    RETURNING id
),
selected_slot AS (
    SELECT id FROM updated_current_slot
    UNION ALL
    SELECT id FROM inserted_slot
    LIMIT 1
)
INSERT INTO loyal_yield.balance_sweep_surplus_lots
    (target_id, source_event_id, source_signature, original_amount_raw,
     remaining_amount_raw, classification, eligible_after, status, confidence, reason,
     scheduled_slot_id)
SELECT $1, $2, $3, $4, $4, $5::loyal_yield.balance_sweep_surplus_classification,
       $6, 'open', $7, $8, selected_slot.id
FROM selected_slot
ON CONFLICT (source_event_id) DO NOTHING
RETURNING id`,
		event.TargetID, event.EventID, event.TxnSignature, amountRaw,
		SurplusLotClassificationDBValue, eligibleAfter, confidence, reason, USDCMint)
	if err != nil {
		return false, fmt.Errorf("schedule autodeposit surplus lot for event %d: %w", event.EventID, err)
	}
	defer rows.Close()
	created := rows.Next()
	return created, rows.Err()
}

// depleteLotsNewestFirst applies an externally observed outflow to open lots.
// Lot statuses stay open/depleted here: the money left the wallet without this
// family moving it, which is evidence about surplus, not a completed sweep.
// Callers pass only the part of an outflow that is not this family's pull.
func depleteLotsNewestFirst(ctx context.Context, tx pgx.Tx, targetID, outflowAmountRaw int64) (int64, error) {
	if outflowAmountRaw == 0 {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `
SELECT lot.id, lot.remaining_amount_raw
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = $1
  AND event.mint = target.token_mint
  AND target.cluster = 'mainnet-beta'
  AND target.token_mint = $2
  AND lot.status = 'open'
  AND lot.remaining_amount_raw > 0
ORDER BY lot.created_at DESC, lot.id DESC
FOR UPDATE OF lot`, targetID, USDCMint)
	if err != nil {
		return 0, fmt.Errorf("lock autodeposit lots for depletion: %w", err)
	}
	type openLot struct {
		id        int64
		remaining int64
	}
	var lots []openLot
	for rows.Next() {
		var lot openLot
		if err := rows.Scan(&lot.id, &lot.remaining); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan autodeposit lot for depletion: %w", err)
		}
		lots = append(lots, lot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var remaining int64 = outflowAmountRaw
	var depletedAmount int64
	for _, lot := range lots {
		if remaining == 0 {
			break
		}
		consumed := min64(remaining, lot.remaining)
		remaining -= consumed
		depletedAmount += consumed
		nextRemaining := lot.remaining - consumed
		nextStatus := "open"
		if nextRemaining == 0 {
			nextStatus = "depleted"
		}
		if _, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_surplus_lots
SET remaining_amount_raw = $2,
    status = $3::loyal_yield.balance_sweep_surplus_lot_status,
    updated_at = now()
WHERE id = $1`, lot.id, nextRemaining, nextStatus); err != nil {
			return 0, fmt.Errorf("deplete surplus lot %d: %w", lot.id, err)
		}
	}
	return depletedAmount, nil
}
