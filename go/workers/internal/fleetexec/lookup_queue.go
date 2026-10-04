package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// This identity is the retained Rust operation_idempotency_key contract.
// Addresses are a set for identity, but durable instruction order is verified
// separately on every reuse. Catalog ordering is not replaced by this hash.
func lookupOperationKey(i LookupIntent, shard int32, desiredHash string) string {
	values := []string{i.Cluster, strconv.FormatInt(i.FamilyID, 10), strconv.FormatInt(i.TableID, 10), string(i.Kind), strconv.FormatInt(int64(i.Generation), 10), strconv.FormatInt(int64(shard), 10), strconv.FormatInt(i.MutationEpoch, 10), desiredHash}
	values = append(values, lookupSortedSet(lookupAddressSet(i.Extension))...)
	return lookupHashValues(values...)
}

func lookupHashValues(values ...string) string {
	h := sha256.New()
	var length [8]byte
	for _, value := range values {
		binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type lookupQueuedOperation struct {
	intent                LookupIntent
	shard                 int32
	manifestID, bindingID *int64
	desiredHash           string
	context               json.RawMessage
	key                   string
}

// The caller holds the source family/physical rows and its planning lease.
// Collision verification happens in the same transaction as the insert.
func enqueueLookupTx(ctx context.Context, tx pgx.Tx, q lookupQueuedOperation) (int64, error) {
	i := q.intent
	if i.TableID <= 0 || i.FamilyID <= 0 || i.MutationEpoch < 0 || i.Generation < 0 || q.shard < 0 || !json.Valid(q.context) || len(i.Extension) > lookupMaximumChunk || len(lookupAddressSet(i.Extension)) != len(i.Extension) {
		return 0, errors.New("lookup source queue intent invalid")
	}
	if q.key == "" {
		q.key = lookupOperationKey(i, q.shard, q.desiredHash)
	}
	var targetGeneration, targetShard *int32
	if i.Kind == LookupCreate || i.Kind == LookupRollover {
		targetGeneration, targetShard = &i.Generation, &q.shard
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_operations(idempotency_key,family_id,route_lookup_table_id,manifest_id,binding_id,operation_kind,operation_state,target_generation,target_shard_ordinal,operation_context,mutation_epoch) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8,$9,$10) ON CONFLICT(idempotency_key) DO NOTHING RETURNING id`, q.key, i.FamilyID, i.TableID, q.manifestID, q.bindingID, i.Kind, targetGeneration, targetShard, q.context, i.MutationEpoch).Scan(&id)
	if err == nil {
		for ordinal, address := range i.Extension {
			if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_operation_addresses(operation_id,address,ordinal) VALUES($1,$2,$3)`, id, address, ordinal); err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var exact bool
	var stored []string
	refreshable := q.manifestID != nil && (i.Kind == LookupCreate || i.Kind == LookupRollover || i.Kind == LookupExtend)
	err = tx.QueryRow(ctx, `SELECT id,family_id=$2 AND route_lookup_table_id=$3 AND manifest_id IS NOT DISTINCT FROM $4::bigint AND binding_id IS NOT DISTINCT FROM $5::bigint AND operation_kind=$6 AND target_generation IS NOT DISTINCT FROM $7::integer AND target_shard_ordinal IS NOT DISTINCT FROM $8::integer AND mutation_epoch=$9 AND ($10 OR operation_context=$11::jsonb),COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id=o.id),'{}'::text[]) FROM loyal_yield.lookup_table_operations o WHERE idempotency_key=$1 FOR UPDATE`, q.key, i.FamilyID, i.TableID, q.manifestID, q.bindingID, i.Kind, targetGeneration, targetShard, i.MutationEpoch, refreshable, q.context).Scan(&id, &exact, &stored)
	if err != nil {
		return 0, err
	}
	if !exact || !lookupSameAddresses(stored, i.Extension) {
		return 0, errors.New("lookup source operation idempotency collision")
	}
	return id, nil
}
