package autodeposit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// RepairUnsignedSchedules restores autonomous progress after an unsigned
// admission failure. An existing packet, execution, selected claim or live
// executor lease keeps its ownership; this is not transaction recovery.
func (s *Store) RepairUnsignedSchedules(ctx context.Context, limit int64) error {
	if s == nil || s.pool == nil {
		return errors.New("autodeposit schedule repair requires a store")
	}
	if limit <= 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT slot.target_id
FROM loyal_yield.balance_sweep_scheduled_slots slot
JOIN loyal_yield.balance_sweep_targets target ON target.id=slot.target_id
JOIN loyal_yield.balance_sweep_wallet_balances_current balance ON balance.target_id=target.id AND balance.mint=target.token_mint
WHERE target.cluster='mainnet-beta' AND slot.status IN('scheduled','failed','released') AND target.token_mint=$2
AND target.wallet_balance_floor_raw IS NOT NULL AND balance.observed_slot>0 AND balance.source_commitment IN('confirmed','finalized')
AND (slot.status IN('failed','released') OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots lot WHERE lot.scheduled_slot_id=slot.id AND lot.status='open' AND lot.remaining_amount_raw>0))
AND (balance.amount_raw-target.wallet_balance_floor_raw<10000 OR
 (slot.status IN('failed','released') AND target.desired_active AND target.chain_status='active' AND EXISTS(
 SELECT 1 FROM loyal_yield.user_yield_positions yp WHERE yp.settings=target.settings AND yp.vault_index=target.vault_index AND yp.wallet_address=target.wallet AND yp.status='active' AND yp.current_liquidity_mint=target.token_mint AND NULLIF(yp.current_reserve,'') IS NOT NULL AND NULLIF(yp.current_market,'') IS NOT NULL)))
AND NOT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims claim WHERE claim.target_id=target.id AND (claim.status='selected' OR claim.autodeposit_executor_lease_expires_at>now()))
ORDER BY slot.target_id LIMIT $1`, limit, USDCMint)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.repairUnsignedTarget(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) repairUnsignedTarget(ctx context.Context, id int64) error {
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Match claim admission's target fence before inspecting claims/lots.
		var floor *int64
		var enabled bool
		err := tx.QueryRow(ctx, `SELECT wallet_balance_floor_raw,desired_active AND chain_status='active'
FROM loyal_yield.balance_sweep_targets WHERE id=$1 AND token_mint=$2 AND cluster='mainnet-beta'
FOR UPDATE SKIP LOCKED`, id, USDCMint).Scan(&floor, &enabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if floor == nil || *floor < 0 {
			return nil
		}
		// Lock every existing claim so lease renewal, signing and selected
		// recovery cannot race this decision. Target locking excludes new claims.
		claims, err := tx.Query(ctx, `SELECT claim_token FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1 ORDER BY claim_token FOR UPDATE`, id)
		if err != nil {
			return err
		}
		for claims.Next() {
		}
		err = claims.Err()
		claims.Close()
		if err != nil {
			return err
		}
		var held bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims
WHERE target_id=$1 AND (status='selected' OR autodeposit_executor_lease_expires_at>now()))`, id).Scan(&held); err != nil {
			return err
		}
		if held {
			return nil
		}
		var amount, slot int64
		err = tx.QueryRow(ctx, `SELECT amount_raw,observed_slot FROM loyal_yield.balance_sweep_wallet_balances_current
WHERE target_id=$1 AND mint=$2 AND source_commitment IN('confirmed','finalized') FOR SHARE`, id, USDCMint).Scan(&amount, &slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if amount < 0 || slot <= 0 {
			return nil
		}
		var caughtUp bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$2),0)
>=COALESCE((SELECT max(event_id) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE target_id=$1 AND mint=$3),0)`, id, ConsumerName, USDCMint).Scan(&caughtUp); err != nil {
			return err
		}
		if !caughtUp {
			return nil
		}
		// Requeue requires a current supported policy and a matching active
		// destination. A temporary missing position never changes desired intent.
		eligible := false
		if enabled {
			var positionID int64
			err = tx.QueryRow(ctx, `SELECT yp.id FROM loyal_yield.balance_sweep_targets target
JOIN loyal_yield.managed_vaults mv ON mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey AND mv.active
JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id AND rp.active AND rp.cluster='mainnet-beta' AND rp.authority=target.authority AND rp.settings=target.settings AND rp.vault_index=target.vault_index AND rp.vault_pubkey=target.vault_pubkey AND 'same_mint_kamino'=ANY(rp.route_modes)
JOIN loyal_yield.user_yield_positions yp ON yp.settings=target.settings AND yp.vault_index=target.vault_index AND yp.wallet_address=target.wallet AND yp.status='active' AND yp.current_liquidity_mint=target.token_mint
WHERE target.id=$1 AND NULLIF(yp.current_reserve,'') IS NOT NULL AND NULLIF(yp.current_market,'') IS NOT NULL
ORDER BY yp.updated_at DESC,yp.id DESC LIMIT 1 FOR SHARE OF mv,rp,yp`, id).Scan(&positionID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			eligible = err == nil
		}
		// Even an old failed packet is retained evidence. Without its family's
		// no-effect proof it cannot authorize fresh scheduling from this slot.
		_, err = tx.Exec(ctx, `WITH unheld_slots AS (
 SELECT slot.id FROM loyal_yield.balance_sweep_scheduled_slots slot
 WHERE slot.target_id=$1 AND slot.token_mint=$2 AND slot.status IN('scheduled','failed','released') AND slot.execution_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claim_items item
 JOIN loyal_yield.balance_sweep_surplus_lots lot ON lot.id=item.lot_id
 JOIN loyal_yield.balance_sweep_lot_claims claim ON claim.claim_token=item.claim_token
 WHERE lot.scheduled_slot_id=slot.id AND (claim.execution_id IS NOT NULL
 OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts a WHERE a.claim_token=claim.claim_token)))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts a WHERE a.scheduled_slot_id=slot.id)
 FOR UPDATE OF slot
), suppressed AS (
 UPDATE loyal_yield.balance_sweep_surplus_lots lot SET status='suppressed',updated_at=now()
 WHERE lot.target_id=$1 AND lot.status='open' AND lot.remaining_amount_raw>0
 AND lot.scheduled_slot_id IN(SELECT id FROM unheld_slots) AND $3::bigint-$4::bigint<10000
 RETURNING lot.id
)
UPDATE loyal_yield.balance_sweep_scheduled_slots slot SET
 status=CASE WHEN $3::bigint-$4::bigint<10000 THEN 'canceled'::loyal_yield.balance_sweep_scheduled_slot_status ELSE 'scheduled'::loyal_yield.balance_sweep_scheduled_slot_status END,
 eligible_after=GREATEST(slot.eligible_after,COALESCE((SELECT max(lot.eligible_after) FROM loyal_yield.balance_sweep_surplus_lots lot WHERE lot.scheduled_slot_id=slot.id AND lot.status='open' AND lot.remaining_amount_raw>0),slot.eligible_after)),
 claim_token=NULL,last_error=NULL,updated_at=now()
WHERE slot.id IN(SELECT id FROM unheld_slots) AND slot.status IN('failed','released')
AND ($3::bigint-$4::bigint<10000 OR ($5::boolean AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots lot WHERE lot.scheduled_slot_id=slot.id AND lot.status='open' AND lot.remaining_amount_raw>0)))`, id, USDCMint, amount, *floor, eligible)
		return err
	})
}
