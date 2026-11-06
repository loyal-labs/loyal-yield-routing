package backyard

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// recordNAVEvidence persists what the deposit quote must agree with: the
// worker's current full-coverage NAV certificate and, once per reconciled
// REPORT_NAV, the on-chain report fields that publication left behind.
func (d *Database) recordNAVEvidence(ctx context.Context, v *Vault, o Observation) error {
	now := time.Now()
	if d == nil || o.routeBatch == nil || !o.Snapshot.Fresh || !o.Snapshot.netKnown(now) || o.Snapshot.TotalVaultNAVRaw < 0 {
		return nil
	}
	vault, err := voltr.DecodeVault(chainAccount(accountAt(o.routeBatch.Accounts, v.VoltrVault), v.VoltrVault))
	if err != nil {
		return err
	}
	net := o.Snapshot.net
	return d.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_certified_nav(vault,manifest_sha256,total_nav_raw,certified_slot,max_slot,valid_until) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (vault) DO UPDATE SET manifest_sha256=EXCLUDED.manifest_sha256,total_nav_raw=EXCLUDED.total_nav_raw,certified_slot=EXCLUDED.certified_slot,max_slot=EXCLUDED.max_slot,valid_until=EXCLUDED.valid_until,updated_at=now()
WHERE backyard_certified_nav.certified_slot<=EXCLUDED.certified_slot`, v.VoltrVault, net.manifest, strconv.FormatInt(o.Snapshot.TotalVaultNAVRaw, 10), net.slot, net.slot+min(observationLagSlots(), adaptorMaxReportAgeSlots), net.validUntil); err != nil {
			return err
		}
		var operation string
		var confirmed int64
		err := tx.QueryRow(ctx, `SELECT operation_id,confirmed_slot FROM loyal_yield.multiply_operations WHERE route_key=$1 AND action=$2 AND status='reconciled' AND confirmed_slot IS NOT NULL ORDER BY confirmed_slot DESC, operation_id DESC LIMIT 1`, v.RouteKey, string(ReportNAV)).Scan(&operation, &confirmed)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && confirmed > o.Snapshot.Slot) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_confirmed_nav_publications(vault,operation_id,locked_profit_raw,last_report_unix,total_value_raw,confirmed_slot) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (vault) DO UPDATE SET operation_id=EXCLUDED.operation_id,locked_profit_raw=EXCLUDED.locked_profit_raw,last_report_unix=EXCLUDED.last_report_unix,total_value_raw=EXCLUDED.total_value_raw,confirmed_slot=EXCLUDED.confirmed_slot,updated_at=now()
WHERE backyard_confirmed_nav_publications.operation_id<>EXCLUDED.operation_id AND backyard_confirmed_nav_publications.confirmed_slot<EXCLUDED.confirmed_slot`, v.VoltrVault, operation, strconv.FormatUint(vault.LastUpdatedLockedProfitRaw, 10), int64(vault.LastLockedProfitReportUnix), strconv.FormatUint(vault.TotalValueRaw, 10), confirmed)
		return err
	})
}
