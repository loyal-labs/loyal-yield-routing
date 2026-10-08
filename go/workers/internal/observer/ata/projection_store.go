package ata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Projector) applyBatch(ctx context.Context, fetchedAfter int64, events []projectionEvent) (ProjectionOutcome, error) {
	tx, err := p.yield.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("begin ATA projection: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var cursor int64
	// This exact retained consumer row is also the old/new mutual exclusion fence.
	if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.projection_offsets(consumer_name,last_event_id)
VALUES($1,0) ON CONFLICT(consumer_name) DO UPDATE SET consumer_name=EXCLUDED.consumer_name
RETURNING last_event_id`, p.consumer).Scan(&cursor); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("lock ATA projection offset: %w", err)
	}
	if cursor < fetchedAfter || cursor < 0 {
		return ProjectionOutcome{}, errors.New("ATA projection offset moved backwards during capture fetch")
	}
	outcome := ProjectionOutcome{PreviousEventID: cursor, LastEventID: cursor, FetchedEvents: len(events)}
	for _, event := range events {
		if event.id <= cursor {
			continue
		}
		// Source IDs are global to the stream, not scoped to a chain. Consume
		// known foreign namespaces without projecting them into unscoped Yield
		// financial tables. Unknown namespaces are rejected by readCapture.
		if event.cluster != p.cluster {
			outcome.LastEventID = event.id
			continue
		}
		if err := lockProjectionTarget(ctx, tx, event); err != nil {
			return ProjectionOutcome{}, fmt.Errorf("bind ATA event %d target custody: %w", event.id, err)
		}
		inserted, err := recordProjectionEvent(ctx, tx, event)
		if err != nil {
			return ProjectionOutcome{}, fmt.Errorf("project ATA event %d: %w", event.id, err)
		}
		if inserted {
			outcome.InsertedEvents++
		}
		outcome.LastEventID = event.id
	}
	if outcome.LastEventID > cursor {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.projection_offsets SET last_event_id=$2,updated_at=now() WHERE consumer_name=$1`, p.consumer, outcome.LastEventID); err != nil {
			return ProjectionOutcome{}, fmt.Errorf("checkpoint ATA projection: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		// Do not report progress on an ambiguous response. Next Tick reads the
		// durable offset and reuses the capture IDs, never synthesizes new IDs.
		return ProjectionOutcome{}, fmt.Errorf("commit ATA projection: %w", err)
	}
	return outcome, nil
}

func lockProjectionTarget(ctx context.Context, tx pgx.Tx, event projectionEvent) error {
	var cluster *string
	var wallet, walletATA, vault, vaultATA, mint string
	if err := tx.QueryRow(ctx, `SELECT cluster,wallet,wallet_token_ata,vault_pubkey,vault_token_ata,token_mint
FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, event.targetID).Scan(&cluster, &wallet, &walletATA, &vault, &vaultATA, &mint); err != nil {
		return err
	}
	// Capture has no setup generation. Bind only the actual custody identities,
	// never fabricate a generation or reinterpret closed/paused desired state.
	if cluster == nil || *cluster != event.cluster || wallet != event.wallet || walletATA != event.walletATA ||
		vault != event.vault || vaultATA != event.vaultATA || mint != event.mint {
		return errors.New("captured custody identity differs from locked target")
	}
	return nil
}

func recordProjectionEvent(ctx context.Context, tx pgx.Tx, event projectionEvent) (bool, error) {
	var previous *int64
	err := tx.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.balance_sweep_wallet_balances_current
WHERE target_id=$1 AND mint=$2 FOR UPDATE`, event.targetID, event.mint).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var delta *int64
	if previous != nil {
		if *previous < 0 {
			return false, errors.New("negative current ATA balance")
		}
		value := event.amountRaw - *previous // Both nonnegative BIGINT amounts; subtraction cannot overflow.
		delta = &value
	}
	result, err := tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
(event_id,target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,amount_raw,observed_slot,observed_at,
source,source_commitment,txn_signature,account_data_hash,raw_evidence,previous_amount_raw,delta_amount_raw)
VALUES($1,$2,$3,NULLIF($4,''),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT(event_id) DO NOTHING`, projectionEventArgs(event, previous, delta)...)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() == 0 {
		// Intentional correction to the retained implementation: a conflicting
		// stable ID cannot silently overwrite current state, and an exact retry
		// must preserve the already committed previous balance and financial delta.
		var matches bool
		err := tx.QueryRow(ctx, `SELECT ROW(target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,amount_raw,
observed_slot,observed_at,source,source_commitment,txn_signature,account_data_hash,raw_evidence)
IS NOT DISTINCT FROM ROW($2::bigint,$3::text,NULLIF($4::text,''),$4::text,$5::text,$6::bigint,
$7::bigint,$8::timestamptz,$9::text,$10::text,$11::text,$12::text,$13::jsonb)
FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, projectionEventArgs(event, nil, nil)[:13]...).Scan(&matches)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, errors.New("stable captured event ID conflicts with destination evidence")
		}
		return false, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
(target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,amount_raw,owner,observed_slot,observed_at,
source,source_commitment,txn_signature,account_data_hash,raw_evidence)
VALUES($1,$2,NULLIF($3,''),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT(target_id,mint) DO UPDATE SET wallet=EXCLUDED.wallet,wallet_usdc_ata=EXCLUDED.wallet_usdc_ata,
wallet_token_ata=EXCLUDED.wallet_token_ata,amount_raw=EXCLUDED.amount_raw,owner=EXCLUDED.owner,
observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,source=EXCLUDED.source,
source_commitment=EXCLUDED.source_commitment,txn_signature=EXCLUDED.txn_signature,
account_data_hash=EXCLUDED.account_data_hash,raw_evidence=EXCLUDED.raw_evidence,updated_at=now()
WHERE EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot`,
		event.targetID, event.wallet, event.walletATA, event.mint, event.amountRaw, event.owner, event.slot,
		event.observedAt, event.source, event.commitment, event.signature, event.hash, event.evidence)
	return true, err
}

func projectionEventArgs(event projectionEvent, previous, delta *int64) []any {
	return []any{event.id, event.targetID, event.wallet, event.walletATA, event.mint, event.amountRaw,
		event.slot, event.observedAt, event.source, event.commitment, event.signature, event.hash, event.evidence, previous, delta}
}
