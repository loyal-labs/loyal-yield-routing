package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Selector sampling may replay evidence, but only the serialized worker advances it.
type readOnlyForeignStore struct{ *Database }

func (readOnlyForeignStore) saveForeignEvidence(context.Context, string, string, foreignEvidence) error {
	return nil
}

func (d *Database) foreignCheckpoint(ctx context.Context, route, address string) (foreignCheckpoint, bool, error) {
	var c foreignCheckpoint
	err := d.pool.QueryRow(ctx, `SELECT signature, slot, balance_raw, foreign_raw FROM loyal_yield.backyard_foreign_inflow_cursors WHERE route_key=$1 AND token_account=$2`, route, address).Scan(&c.Signature, &c.Slot, &c.BalanceRaw, &c.ForeignRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

func (d *Database) saveForeignEvidence(ctx context.Context, route, address string, e foreignEvidence) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return d.inTx(ctx, func(tx pgx.Tx) error {
		// The cursor compare-and-swap makes replay and restart idempotent; events
		// and the checkpoint commit together, never across an RPC call.
		if e.Before.Signature == "" {
			_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_foreign_inflow_cursors (route_key,token_account,signature,slot,balance_raw,foreign_raw) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, route, address, e.After.Signature, e.After.Slot, e.After.BalanceRaw, e.After.ForeignRaw)
			if err != nil {
				return err
			}
		} else {
			_, err := tx.Exec(ctx, `UPDATE loyal_yield.backyard_foreign_inflow_cursors SET signature=$3,slot=$4,balance_raw=$5,foreign_raw=$6,updated_at=clock_timestamp() WHERE route_key=$1 AND token_account=$2 AND signature=$7 AND slot=$8 AND balance_raw=$9 AND foreign_raw=$10`, route, address, e.After.Signature, e.After.Slot, e.After.BalanceRaw, e.After.ForeignRaw, e.Before.Signature, e.Before.Slot, e.Before.BalanceRaw, e.Before.ForeignRaw)
			if err != nil {
				return err
			}
		}
		var got foreignCheckpoint
		if err := tx.QueryRow(ctx, `SELECT signature,slot,balance_raw,foreign_raw FROM loyal_yield.backyard_foreign_inflow_cursors WHERE route_key=$1 AND token_account=$2`, route, address).Scan(&got.Signature, &got.Slot, &got.BalanceRaw, &got.ForeignRaw); err != nil {
			return err
		}
		if got != e.After {
			return fmt.Errorf("foreign-inflow checkpoint changed")
		}
		changed := e.Before.Signature == "" || e.Before.ForeignRaw != e.After.ForeignRaw
		for _, transfer := range e.Transfers {
			if transfer.Destination == address && !transfer.Voltr && !transfer.Stage {
				changed = true
			}
		}
		if changed {
			_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_foreign_inflows (route_key,token_account,signature,slot,foreign_before_raw,foreign_after_raw,evidence) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, route, address, e.After.Signature, e.After.Slot, e.Before.ForeignRaw, e.After.ForeignRaw, encoded)
			return err
		}
		return nil
	})
}
