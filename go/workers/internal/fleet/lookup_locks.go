package fleet

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type LookupRows struct {
	IDs   []int64
	Share bool
}

type LookupDesiredHeadKey struct {
	FamilyID, VaultID int64
	BindingOrdinal    int32
}

type LookupLockPlan struct {
	ControlCluster, ControlOwner string
	Heads                        LookupRows
	Families                     LookupRows
	DesiredHeads                 []LookupDesiredHeadKey
	Bindings                     LookupRows
	Tables                       LookupRows
	Authority                    string
	Operations                   LookupRows
}

func LockLookupRows(ctx context.Context, tx pgx.Tx, p LookupLockPlan) error {
	if p.ControlCluster != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioner_controls(cluster,paused,reason,updated_by,control_epoch) VALUES($1,false,'implicit unpaused provisioner control',$2,0) ON CONFLICT(cluster) DO NOTHING`, p.ControlCluster, p.ControlOwner); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT paused FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1 FOR UPDATE`, p.ControlCluster); err != nil {
			return err
		}
	}
	for _, class := range []struct {
		name, sql string
		rows      LookupRows
	}{
		{"heads", `SELECT h.family_id FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_families f ON f.id=h.family_id WHERE h.family_id=ANY($1::bigint[]) AND f.kind='shared_market' ORDER BY h.family_id FOR %s OF h`, p.Heads},
		{"families", `SELECT id FROM loyal_yield.lookup_table_families WHERE id=ANY($1::bigint[]) ORDER BY id FOR %s`, p.Families},
	} {
		if err := lockLookupClass(ctx, tx, class.name, class.sql, class.rows); err != nil {
			return err
		}
	}
	if len(p.DesiredHeads) > 0 {
		families, vaults, ordinals := make([]int64, len(p.DesiredHeads)), make([]int64, len(p.DesiredHeads)), make([]int32, len(p.DesiredHeads))
		for i, key := range p.DesiredHeads {
			families[i], vaults[i], ordinals[i] = key.FamilyID, key.VaultID, key.BindingOrdinal
		}
		if _, err := tx.Exec(ctx, `SELECT h.family_id FROM loyal_yield.lookup_table_vault_desired_heads h JOIN unnest($1::bigint[],$2::bigint[],$3::integer[]) k(family_id,vault_id,binding_ordinal) USING (family_id,vault_id,binding_ordinal) ORDER BY h.family_id,h.vault_id,h.binding_ordinal FOR UPDATE OF h`, families, vaults, ordinals); err != nil {
			return fmt.Errorf("lock lookup desired heads: %w", err)
		}
	}
	for _, class := range []struct {
		name, sql string
		rows      LookupRows
	}{
		{"bindings", `SELECT id FROM loyal_yield.lookup_table_vault_bindings WHERE id=ANY($1::bigint[]) ORDER BY id FOR %s`, p.Bindings},
		{"tables", `SELECT id FROM loyal_yield.route_lookup_tables WHERE id=ANY($1::bigint[]) ORDER BY id FOR %s`, p.Tables},
	} {
		if err := lockLookupClass(ctx, tx, class.name, class.sql, class.rows); err != nil {
			return err
		}
	}
	if p.Authority != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Authority); err != nil {
			return err
		}
	}
	return lockLookupClass(ctx, tx, "operations", `SELECT id FROM loyal_yield.lookup_table_operations WHERE id=ANY($1::bigint[]) ORDER BY id FOR %s`, p.Operations)
}

func lockLookupClass(ctx context.Context, tx pgx.Tx, name, sql string, rows LookupRows) error {
	if len(rows.IDs) == 0 {
		return nil
	}
	mode := "UPDATE"
	if rows.Share {
		mode = "SHARE"
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(sql, mode), rows.IDs); err != nil {
		return fmt.Errorf("lock lookup %s: %w", name, err)
	}
	return nil
}

func QueryLookupIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]int64, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
