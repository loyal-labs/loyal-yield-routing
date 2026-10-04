package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type LookupPlanningRequest struct {
	ID, VaultID                       int64
	Cluster, Fingerprint              string
	SharedManifestID, VaultManifestID *int64
	SharedHash, VaultHash             *string
	SharedCount, VaultCount           int
	Lease                             LookupLease
}
type LookupManifestAddress struct {
	Address, SemanticClass, Role string
	Ordinal                      int32
	Writable                     bool
}
type LookupManifest struct {
	ID, FamilyID, VaultID int64
	Hash                  string
	Addresses             []LookupManifestAddress
}

func lookupManifestHash(addresses []LookupManifestAddress) string {
	h := sha256.New()
	var length [8]byte
	var ordinal [4]byte
	for _, a := range addresses {
		for _, value := range []string{a.Address, a.SemanticClass, a.Role} {
			binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
			h.Write(length[:])
			h.Write([]byte(value))
		}
		binary.LittleEndian.PutUint32(ordinal[:], uint32(a.Ordinal))
		h.Write(ordinal[:])
		flag := byte(0)
		if a.Writable {
			flag = 1
		}
		h.Write([]byte{flag})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The read-only census and lease share economic ordering so the account reads
// normally cover the request which wins the lease, including rising priorities.
const lookupPlanningCandidates = `FROM loyal_yield.lookup_table_provisioning_requests r
 LEFT JOIN LATERAL (SELECT COALESCE(sum(p.annual_yield_gain_usd_micros),0)::numeric yield,COALESCE(sum(p.economic_priority),0)::numeric priority,count(*) consumers FROM loyal_yield.lookup_table_provisioning_request_consumers c JOIN loyal_yield.rebalance_opportunities p ON p.id=c.opportunity_id WHERE c.provisioning_request_id=r.id AND p.opportunity_state='waiting_alt' AND p.expires_at>clock_timestamp()) live ON true
 WHERE r.cluster=$1 AND r.request_status IN ('requested','queued','failed','planning')
 AND ((r.request_status='failed' AND r.next_attempt_at IS NOT NULL AND r.next_attempt_at<=clock_timestamp()) OR (r.request_status<>'failed' AND (r.next_attempt_at IS NULL OR r.next_attempt_at<=clock_timestamp())))
 AND (r.request_status<>'planning' OR r.lease_expires_at<=clock_timestamp())
 AND NOT COALESCE((SELECT paused FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1),false)
 ORDER BY live.yield/GREATEST(1,r.desired_shared_address_count+r.desired_vault_address_count) DESC,live.priority DESC,live.consumers DESC,r.updated_at,r.requested_at,r.id`

// Planning has its own existing request lease. Its live economic priority is
// recomputed from consumers, rather than trusting stale request priority fields.
func (s *Store) LeaseLookupPlanningRequest(ctx context.Context, cluster, owner string, ttl time.Duration) (*LookupPlanningRequest, error) {
	if cluster == "" || owner == "" || ttl < 10*time.Second || ttl > 5*time.Minute || ttl%time.Second != 0 {
		return nil, errors.New("lookup request lease configuration invalid")
	}
	var r LookupPlanningRequest
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT r.id `+lookupPlanningCandidates+`
 FOR UPDATE OF r SKIP LOCKED LIMIT 1)
 UPDATE loyal_yield.lookup_table_provisioning_requests r SET request_status='planning',lease_owner=$2,lease_expires_at=clock_timestamp()+$3::bigint*interval '1 second',fencing_token=fencing_token+1,attempt_count=attempt_count+1,updated_at=clock_timestamp()
 FROM candidate WHERE r.id=candidate.id RETURNING r.id,r.vault_id,r.cluster,r.requirements_fingerprint,r.shared_manifest_id,r.vault_manifest_id,r.desired_shared_hash,r.desired_vault_hash,r.desired_shared_address_count,r.desired_vault_address_count,r.lease_owner,r.fencing_token,r.lease_expires_at`, cluster, owner, int64(ttl/time.Second)).Scan(&r.ID, &r.VaultID, &r.Cluster, &r.Fingerprint, &r.SharedManifestID, &r.VaultManifestID, &r.SharedHash, &r.VaultHash, &r.SharedCount, &r.VaultCount, &r.Lease.Owner, &r.Lease.FencingToken, &r.Lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// SealLookupVaultDemand unions ALL sealed, non-cancelled source cohorts for the
// vault. It never lets the current opportunity replace earlier complete demand.
// The caller must subsequently allocate/queue and advance its request lease.
func (s *Store) SealLookupVaultDemand(ctx context.Context, request LookupPlanningRequest, sourceSlot int64) (LookupManifest, error) {
	var result LookupManifest
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		result, err = sealLookupVaultDemandTx(ctx, tx, request, sourceSlot)
		return err
	})
	return result, err
}

func sealLookupVaultDemandTx(ctx context.Context, tx pgx.Tx, request LookupPlanningRequest, sourceSlot int64) (LookupManifest, error) {
	var result LookupManifest
	if sourceSlot <= 0 {
		return result, errors.New("lookup manifest source bank is missing")
	}
	err := func() error {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT request_status='planning' AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND sealed_at IS NOT NULL FROM loyal_yield.lookup_table_provisioning_requests WHERE id=$1 AND cluster=$4 AND vault_id=$5 FOR UPDATE`, request.ID, request.Lease.Owner, request.Lease.FencingToken, request.Cluster, request.VaultID).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrStaleOwner
		}
		rows, err := tx.Query(ctx, `SELECT id,planner_version,catalog_version FROM loyal_yield.lookup_table_families WHERE cluster=$1 AND kind='vault_shards' AND desired_state='active' ORDER BY logical_name,id FOR SHARE`, request.Cluster)
		if err != nil {
			return err
		}
		var planner, catalog string
		count := 0
		for rows.Next() {
			if err = rows.Scan(&result.FamilyID, &planner, &catalog); err != nil {
				rows.Close()
				return err
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("lookup vault demand requires one active source vault-shards family")
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-vault-manifest:'||$1::bigint::text||':'||$2::bigint::text,0))`, result.FamilyID, request.VaultID); err != nil {
			return err
		}
		var cohortCount int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 AND semantic_class='vault'`, request.ID).Scan(&cohortCount); err != nil {
			return err
		}
		if cohortCount != request.VaultCount {
			return errors.New("lookup sealed request vault cohort count changed")
		}
		rows, err = tx.Query(ctx, `SELECT a.address,a.account_role,a.is_writable FROM loyal_yield.lookup_table_provisioning_requests r JOIN loyal_yield.lookup_table_provisioning_request_addresses a ON a.request_id=r.id WHERE r.cluster=$1 AND r.vault_id=$2 AND r.sealed_at IS NOT NULL AND r.request_status<>'cancelled' AND a.semantic_class='vault' ORDER BY a.address,r.id,a.ordinal`, request.Cluster, request.VaultID)
		if err != nil {
			return err
		}
		type descriptor struct {
			roles    map[string]bool
			writable bool
		}
		aggregate := map[string]*descriptor{}
		for rows.Next() {
			var address, roles string
			var writable bool
			if err = rows.Scan(&address, &roles, &writable); err != nil {
				rows.Close()
				return err
			}
			entry := aggregate[address]
			if entry == nil {
				entry = &descriptor{roles: map[string]bool{}}
				aggregate[address] = entry
			}
			for _, role := range strings.Split(roles, ",") {
				if role = strings.TrimSpace(role); role != "" {
					entry.roles[role] = true
				}
			}
			entry.writable = entry.writable || writable
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		addresses := make([]string, 0, len(aggregate))
		for address := range aggregate {
			addresses = append(addresses, address)
		}
		slices.Sort(addresses)
		for ordinal, address := range addresses {
			entry := aggregate[address]
			result.Addresses = append(result.Addresses, LookupManifestAddress{Address: address, SemanticClass: "vault", Ordinal: int32(ordinal), Role: strings.Join(lookupSortedSet(entry.roles), ","), Writable: entry.writable})
		}
		result.VaultID = request.VaultID
		result.Hash = lookupManifestHash(result.Addresses)
		subject := fmt.Sprintf("vault:%d:aggregate", request.VaultID)
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,vault_id,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'vault',$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(family_id,subject_kind,subject_key,desired_set_hash) DO NOTHING RETURNING id`, result.FamilyID, subject, request.VaultID, result.Hash, len(result.Addresses), sourceSlot, planner, catalog).Scan(&result.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			var sealed bool
			if err = tx.QueryRow(ctx, `SELECT id,sealed_at IS NOT NULL FROM loyal_yield.lookup_table_manifests WHERE family_id=$1 AND subject_kind='vault' AND subject_key=$2 AND desired_set_hash=$3`, result.FamilyID, subject, result.Hash).Scan(&result.ID, &sealed); err != nil {
				return err
			}
			if !sealed {
				return errors.New("lookup aggregate identity collides with unsealed manifest")
			}
			rows, err = tx.Query(ctx, `SELECT address,semantic_class,account_role,ordinal,is_writable FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=$1 ORDER BY ordinal`, result.ID)
			if err != nil {
				return err
			}
			var persisted []LookupManifestAddress
			for rows.Next() {
				var a LookupManifestAddress
				if err = rows.Scan(&a.Address, &a.SemanticClass, &a.Role, &a.Ordinal, &a.Writable); err != nil {
					rows.Close()
					return err
				}
				persisted = append(persisted, a)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if !slices.Equal(persisted, result.Addresses) {
				return errors.New("lookup aggregate immutable descriptor collision")
			}
			return nil
		}
		if err != nil {
			return err
		}
		for _, a := range result.Addresses {
			if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,semantic_class,account_role,ordinal,is_writable) VALUES($1,$2,$3,$4,$5,$6)`, result.ID, a.Address, a.SemanticClass, a.Role, a.Ordinal, a.Writable); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, result.ID)
		return err
	}()
	return result, err
}
