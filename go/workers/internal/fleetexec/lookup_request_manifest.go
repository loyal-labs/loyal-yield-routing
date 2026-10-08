package fleetexec

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

func sealLookupSharedRequestTx(ctx context.Context, tx pgx.Tx, r LookupPlanningRequest, slot int64) (int64, int64, error) {
	if r.SharedHash == nil || *r.SharedHash == "" {
		return 0, 0, errors.New("lookup sealed shared request hash missing")
	}
	var familyID, revisionID int64
	var planner, catalog string
	err := tx.QueryRow(ctx, `SELECT f.id,h.catalog_revision_id,f.planner_version,f.catalog_version FROM loyal_yield.lookup_table_families f JOIN loyal_yield.lookup_table_shared_market_catalog_heads h ON h.family_id=f.id WHERE f.cluster=$1 AND f.kind='shared_market' AND f.desired_state='active'`, r.Cluster).Scan(&familyID, &revisionID, &planner, &catalog)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT address,semantic_class,account_role,ordinal,is_writable FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 AND semantic_class='shared_market' ORDER BY ordinal`, r.ID)
	if err != nil {
		return 0, 0, err
	}
	var descriptors []LookupManifestAddress
	for rows.Next() {
		var a LookupManifestAddress
		if err = rows.Scan(&a.Address, &a.SemanticClass, &a.Role, &a.Ordinal, &a.Writable); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if a.Ordinal != int32(len(descriptors)) {
			rows.Close()
			return 0, 0, errors.New("lookup shared request ordinal drift")
		}
		descriptors = append(descriptors, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	if len(descriptors) != r.SharedCount || lookupManifestHash(descriptors) != *r.SharedHash {
		return 0, 0, errors.New("lookup shared request exact sealed identity differs")
	}
	var id int64
	if r.SharedManifestID != nil {
		var valid bool
		id = *r.SharedManifestID
		err = tx.QueryRow(ctx, `SELECT family_id=$2 AND subject_kind='shared_market' AND desired_set_hash=$3 AND address_count=$4 AND sealed_at IS NOT NULL FROM loyal_yield.lookup_table_manifests WHERE id=$1 FOR SHARE`, id, familyID, *r.SharedHash, r.SharedCount).Scan(&valid)
		if err != nil {
			return 0, 0, err
		}
		if !valid {
			return 0, 0, errors.New("lookup retained shared manifest differs from sealed request")
		}
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market',$2,$3,$4,$5,$6,$7) ON CONFLICT(family_id,subject_kind,subject_key,desired_set_hash) DO NOTHING RETURNING id`, familyID, "route:"+r.Fingerprint, *r.SharedHash, len(descriptors), slot, planner, catalog).Scan(&id)
		if err == nil {
			for _, a := range descriptors {
				if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,semantic_class,account_role,ordinal,is_writable) VALUES($1,$2,$3,$4,$5,$6)`, id, a.Address, a.SemanticClass, a.Role, a.Ordinal, a.Writable); err != nil {
					return 0, 0, err
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
				return 0, 0, err
			}
			return id, revisionID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, err
		}
		err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.lookup_table_manifests WHERE family_id=$1 AND subject_kind='shared_market' AND subject_key=$2 AND desired_set_hash=$3 AND sealed_at IS NOT NULL`, familyID, "route:"+r.Fingerprint, *r.SharedHash).Scan(&id)
		if err != nil {
			return 0, 0, err
		}
	}
	rows, err = tx.Query(ctx, `SELECT address,semantic_class,account_role,ordinal,is_writable FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return 0, 0, err
	}
	var persisted []LookupManifestAddress
	for rows.Next() {
		var a LookupManifestAddress
		if err = rows.Scan(&a.Address, &a.SemanticClass, &a.Role, &a.Ordinal, &a.Writable); err != nil {
			rows.Close()
			return 0, 0, err
		}
		persisted = append(persisted, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	if !slices.Equal(descriptors, persisted) {
		return 0, 0, errors.New("lookup shared sealed descriptor collision")
	}
	return id, revisionID, nil
}
