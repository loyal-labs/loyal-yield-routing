package earn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

const PolicyProjectionConsumer = "earn_max_policy_sets_laserstream_v2"

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type EnqueueOutcome struct {
	InsertedJobs, CoalescedAutodeposits int64
	Cursor                              uint64
}

func (s *Store) Enqueue(ctx context.Context, consumer, eventKey string, slot uint64, event any, vaults []watch.Vault, account string) (EnqueueOutcome, error) {
	if slot > math.MaxInt64 {
		return EnqueueOutcome{}, fmt.Errorf("earn slot exceeds PostgreSQL BIGINT")
	}
	if len(vaults) == 0 {
		return EnqueueOutcome{}, fmt.Errorf("earn event has no affected vault")
	}
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return EnqueueOutcome{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnqueueOutcome{}, err
	}
	defer tx.Rollback(ctx)
	var outcome EnqueueOutcome
	outcome.Cursor = slot
	for _, vault := range vaults {
		vaultJSON, err := json.Marshal(vault)
		if err != nil {
			return EnqueueOutcome{}, err
		}
		result, err := tx.Exec(ctx, `INSERT INTO loyal_yield.earn_reconciliation_jobs(consumer_name,event_key,durable_slot,settings,vault_index,vault_pubkey,event_payload,vault_payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(consumer_name,event_key,settings,vault_index,vault_pubkey) DO NOTHING`, consumer, eventKey, int64(slot), vault.Settings, int16(vault.VaultIndex), vault.Vault, eventJSON, vaultJSON)
		if err != nil {
			return EnqueueOutcome{}, fmt.Errorf("enqueue Earn reconciliation job: %w", err)
		}
		outcome.InsertedJobs += result.RowsAffected()
		var targetID int64
		err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_targets WHERE settings=$1 AND vault_pubkey=$2 AND chain_status<>'closed' AND (policy_account=$3 OR subscription_authority=$3 OR recurring_delegation=$3 OR wallet_token_ata=$3) ORDER BY policy_seed DESC LIMIT 1`, vault.Settings, vault.Vault, account).Scan(&targetID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return EnqueueOutcome{}, fmt.Errorf("load Autodeposit reconciliation target: %w", err)
		}
		if err == nil {
			result, err := tx.Exec(ctx, `INSERT INTO loyal_yield.autodeposit_reconciliation_requests(target_id,requested_slot) VALUES($1,$2) ON CONFLICT(target_id) DO UPDATE SET requested_slot=EXCLUDED.requested_slot,next_attempt_at=LEAST(loyal_yield.autodeposit_reconciliation_requests.next_attempt_at,now()),updated_at=now() WHERE EXCLUDED.requested_slot>=loyal_yield.autodeposit_reconciliation_requests.requested_slot`, targetID, int64(slot))
			if err != nil {
				return EnqueueOutcome{}, err
			}
			outcome.CoalescedAutodeposits += result.RowsAffected()
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.laserstream_replay_cursors(consumer_name,durable_slot) VALUES($1,$2) ON CONFLICT(consumer_name) DO UPDATE SET durable_slot=GREATEST(loyal_yield.laserstream_replay_cursors.durable_slot,EXCLUDED.durable_slot),updated_at=now()`, consumer, int64(slot)); err != nil {
		return EnqueueOutcome{}, fmt.Errorf("advance Earn durable cursor: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return EnqueueOutcome{}, err
	}
	return outcome, nil
}

func (s *Store) AdvanceReplayCursor(ctx context.Context, consumer string, slot uint64) error {
	if slot == 0 || slot > math.MaxInt64 {
		return fmt.Errorf("replay cursor slot is invalid")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.laserstream_replay_cursors(consumer_name,durable_slot) VALUES($1,$2) ON CONFLICT(consumer_name) DO UPDATE SET durable_slot=GREATEST(loyal_yield.laserstream_replay_cursors.durable_slot,EXCLUDED.durable_slot),updated_at=now()`, consumer, int64(slot))
	if err != nil {
		return fmt.Errorf("advance replay cursor: %w", err)
	}
	return nil
}

func (s *Store) ReplayCursor(ctx context.Context, consumer string) (uint64, error) {
	var value int64
	err := s.pool.QueryRow(ctx, `SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1`, consumer).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load Earn replay cursor: %w", err)
	}
	if value < 0 {
		return 0, fmt.Errorf("earn replay cursor is negative")
	}
	return uint64(value), nil
}
func (s *Store) ProjectionCursor(ctx context.Context, consumer string) (uint64, error) {
	var value int64
	err := s.pool.QueryRow(ctx, `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, consumer).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load policy projection cursor: %w", err)
	}
	if value < 0 {
		return 0, fmt.Errorf("policy projection cursor is negative")
	}
	return uint64(value), nil
}
func (s *Store) AdvanceProjectionCursor(ctx context.Context, consumer string, slot uint64) error {
	if slot > math.MaxInt64 {
		return fmt.Errorf("projection slot exceeds BIGINT")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.projection_offsets(consumer_name,last_event_id) VALUES($1,$2) ON CONFLICT(consumer_name) DO UPDATE SET last_event_id=GREATEST(loyal_yield.projection_offsets.last_event_id,EXCLUDED.last_event_id),updated_at=now()`, consumer, int64(slot))
	return err
}
func (s *Store) Health(ctx context.Context, consumer string) (cursor, pending, failed, oldest uint64, err error) {
	var a, b, c, d int64
	err = s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1),0),COUNT(*),COUNT(*) FILTER(WHERE last_error IS NOT NULL),COALESCE(GREATEST(0,FLOOR(EXTRACT(EPOCH FROM(now()-MIN(created_at))))::bigint),0) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1 AND completed_at IS NULL`, consumer).Scan(&a, &b, &c, &d)
	if err == nil {
		cursor, pending, failed, oldest = uint64(max(a, 0)), uint64(max(b, 0)), uint64(max(c, 0)), uint64(max(d, 0))
	}
	return
}
