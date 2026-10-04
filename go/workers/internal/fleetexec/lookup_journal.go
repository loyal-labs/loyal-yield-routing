package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

func (s *Store) RequireLookupSchema(ctx context.Context) error {
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.lookup_table_signed_attempts') IS NOT NULL
 AND to_regclass('loyal_yield.lookup_table_provisioner_broadcast_permits') IS NOT NULL
 AND to_regclass('loyal_yield.lookup_table_cluster_budget_reservations') IS NOT NULL
 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='loyal_yield' AND table_name='lookup_table_signed_attempts' AND column_name='signing_context_slot')`).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("registered lookup journal schema is missing")
	}
	return nil
}

const lookupAttemptColumns = `id,attempt_number,source_fencing_token,signing_context_slot,cluster,operation_id,family_id,route_lookup_table_id,
 operation_kind,table_address,authority,payer,COALESCE(recipient,''),generation,mutation_epoch,recent_slot,expected_deactivation_slot,
 prefix_addresses,extension_addresses,transaction_signature,message_hash,signed_transaction,signed_transaction_sha256,recent_blockhash,
 last_valid_block_height,attempt_state,broadcast_count,estimated_fee_lamports,estimated_rent_lamports,estimated_reclaimed_rent_lamports,
 confirmed_slot,finalized_slot,readback_slot`

func scanLookupAttempt(row pgx.Row) (LookupAttempt, error) {
	var a LookupAttempt
	var recent, deactivation *int64
	var fee, rent, reclaimed int64
	err := row.Scan(&a.ID, &a.AttemptNumber, &a.SourceFencingToken, &a.SigningContextSlot, &a.Intent.Cluster, &a.Intent.OperationID, &a.Intent.FamilyID, &a.Intent.TableID,
		&a.Intent.Kind, &a.Intent.TableAddress, &a.Intent.Authority, &a.Intent.Payer, &a.Intent.Recipient, &a.Intent.Generation, &a.Intent.MutationEpoch, &recent, &deactivation,
		&a.Intent.Prefix, &a.Intent.Extension, &a.Wire.TransactionSignature, &a.Wire.MessageHash, &a.Wire.SignedTransaction, &a.Wire.SignedTransactionHash, &a.Wire.RecentBlockhash,
		&a.Wire.LastValidBlockHeight, &a.State, &a.BroadcastCount, &fee, &rent, &reclaimed, &a.ConfirmedSlot, &a.FinalizedSlot, &a.ReadbackSlot)
	if err != nil {
		return a, err
	}
	if recent != nil {
		v := uint64(*recent)
		a.Intent.RecentSlot = &v
	}
	if deactivation != nil {
		v := uint64(*deactivation)
		a.Intent.ExpectedDeactivationSlot = &v
	}
	a.EstimatedFeeLamports, a.EstimatedRentLamports, a.EstimatedReclaimedLamports = uint64(fee), uint64(rent), uint64(reclaimed)
	return a, proveLookupWire(a.Intent, a.Wire)
}
func (s *Store) LoadLookupAttempt(ctx context.Context, operation int64) (*LookupAttempt, error) {
	a, err := scanLookupAttempt(s.pool.QueryRow(ctx, `SELECT `+lookupAttemptColumns+` FROM loyal_yield.lookup_table_signed_attempts WHERE operation_id=$1 AND attempt_state NOT IN ('reconciled','failed','expired')`, operation))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

type lookupLockedSource struct {
	state, familyState, familyKind, tableState string
	context                                    json.RawMessage
}

// Every transition locks the source family, physical table and source operation
// in that order, then rechecks the live owner and fencing token. No RPC occurs
// in this transaction. Packet ownership outlives control changes and leases.
func lookupLockSource(ctx context.Context, tx pgx.Tx, i LookupIntent, lease LookupLease) (lookupLockedSource, error) {
	var out lookupLockedSource
	var cluster, authority, payer string
	err := tx.QueryRow(ctx, `SELECT cluster,provisioning_authority,payer,desired_state,kind FROM loyal_yield.lookup_table_families WHERE id=$1 FOR UPDATE`, i.FamilyID).Scan(&cluster, &authority, &payer, &out.familyState, &out.familyKind)
	if err != nil {
		return out, err
	}
	if cluster != i.Cluster || authority != i.Authority || payer != i.Payer {
		return out, errors.New("lookup source family identity changed")
	}
	var family, epoch int64
	var address, tableAuthority, tablePayer, tableCluster string
	var generation int32
	err = tx.QueryRow(ctx, `SELECT family_id,cluster,table_address,authority,payer,generation,mutation_epoch,desired_state FROM loyal_yield.route_lookup_tables WHERE id=$1 FOR UPDATE`, i.TableID).Scan(&family, &tableCluster, &address, &tableAuthority, &tablePayer, &generation, &epoch, &out.tableState)
	if err != nil {
		return out, err
	}
	if family != i.FamilyID || tableCluster != i.Cluster || address != i.TableAddress || tableAuthority != i.Authority || tablePayer != i.Payer || generation != i.Generation || epoch != i.MutationEpoch {
		return out, errors.New("lookup physical identity/epoch changed")
	}
	var sourceKind string
	var sourceTable, sourceFamily, sourceEpoch int64
	err = tx.QueryRow(ctx, `SELECT family_id,route_lookup_table_id,operation_kind,mutation_epoch,operation_state,operation_context FROM loyal_yield.lookup_table_operations WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() FOR UPDATE`, i.OperationID, lease.Owner, lease.FencingToken).Scan(&sourceFamily, &sourceTable, &sourceKind, &sourceEpoch, &out.state, &out.context)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrStaleOwner
	}
	if err != nil {
		return out, err
	}
	if sourceFamily != i.FamilyID || sourceTable != i.TableID || sourceEpoch != i.MutationEpoch || sourceKind != string(i.Kind) {
		return out, errors.New("lookup operation intent changed")
	}
	return out, nil
}
func lookupSourceMembership(ctx context.Context, tx pgx.Tx, i LookupIntent) error {
	var prefix, suffix []string
	var projected []string
	var projection json.RawMessage
	var storedHash string
	var storedCount int
	var exact bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(address ORDER BY ordinal),'{}'::text[]),COALESCE(min(ordinal)=0 AND max(ordinal)=count(*)-1,true) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, i.TableID).Scan(&prefix, &exact)
	if err != nil {
		return err
	}
	if !exact || !lookupSameAddresses(prefix, i.Prefix) {
		return errors.New("lookup physical ordered membership changed")
	}
	err = tx.QueryRow(ctx, `SELECT addresses,address_hash,address_count FROM loyal_yield.route_lookup_tables WHERE id=$1`, i.TableID).Scan(&projection, &storedHash, &storedCount)
	if err != nil {
		return err
	}
	if json.Unmarshal(projection, &projected) != nil || !lookupSameAddresses(projected, prefix) || storedCount != len(prefix) || (storedHash != lookupOrderedAddressHash(prefix) && !(len(prefix) == 0 && storedHash == "")) {
		return errors.New("lookup physical projection differs from ordered membership")
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(address ORDER BY ordinal),'{}'::text[]),COALESCE(min(ordinal)=0 AND max(ordinal)=count(*)-1,true) FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id=$1`, i.OperationID).Scan(&suffix, &exact)
	if err != nil {
		return err
	}
	if !exact || !lookupSameAddresses(suffix, i.Extension) {
		return errors.New("lookup operation ordered suffix changed")
	}
	return nil
}

// PersistLookupPrepared supplements the existing source signing transition.
// A matching source budget reservation must already exist before signing. The
// returned owned bytes, never a newly rebuilt packet, are eligible for send.
func (s *Store) PersistLookupPrepared(ctx context.Context, operation LookupOperation, prepared LookupAttempt) (LookupAttempt, error) {
	var out LookupAttempt
	i := prepared.Intent
	if operation.Intent.OperationID != i.OperationID || prepared.SigningContextSlot <= 0 || (i.RecentSlot != nil && (*i.RecentSlot > math.MaxInt64 || *i.RecentSlot > uint64(prepared.SigningContextSlot))) || (i.ExpectedDeactivationSlot != nil && *i.ExpectedDeactivationSlot > math.MaxInt64) || prepared.EstimatedFeeLamports > math.MaxInt64 || prepared.EstimatedRentLamports > math.MaxInt64 || prepared.EstimatedReclaimedLamports > math.MaxInt64 {
		return out, errors.New("lookup prepared bank/accounting identity incomplete")
	}
	if err := proveLookupWire(i, prepared.Wire); err != nil {
		return out, err
	}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, i, operation.Lease.Owner)
		if err != nil {
			return err
		}
		locked, err := lookupLockSource(ctx, tx, i, operation.Lease)
		if err != nil {
			return err
		}
		existing, err := scanLookupAttempt(tx.QueryRow(ctx, `SELECT `+lookupAttemptColumns+` FROM loyal_yield.lookup_table_signed_attempts WHERE operation_id=$1 AND attempt_state NOT IN ('reconciled','failed','expired') FOR UPDATE`, i.OperationID))
		if err == nil {
			out = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if locked.state != "leased" || paused || !lookupFamilyAllows(locked.familyState, i.Kind) {
			return errors.New("lookup source controls do not permit new signing")
		}
		if err = lookupSourceMembership(ctx, tx, i); err != nil {
			return err
		}
		var approved bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_cluster_budget_reservations WHERE operation_id=$1 AND fencing_token=$2 AND lease_owner=$3 AND estimated_fee_lamports=$4 AND estimated_rent_lamports=$5 AND reserved_until>clock_timestamp())`, i.OperationID, operation.Lease.FencingToken, operation.Lease.Owner, int64(prepared.EstimatedFeeLamports), int64(prepared.EstimatedRentLamports)).Scan(&approved)
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("lookup signing lacks exact durable source budget")
		}
		if err = lookupUnsignedGuards(ctx, tx, i, locked); err != nil {
			return err
		}
		if i.Kind == LookupDeactivate {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='retiring',accepting_allocations=false,updated_at=clock_timestamp() WHERE id=$1 AND desired_state IN ('active','standby')`, i.TableID); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='signed',transaction_signature=$2,message_hash=$3,recent_blockhash=$4,last_valid_block_height=$5,estimated_fee_lamports=$6,estimated_rent_lamports=$7,operation_context=jsonb_set(operation_context,'{signedExpectedReclaimedRentLamports}',to_jsonb($8::bigint),true),error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL`, i.OperationID, prepared.Wire.TransactionSignature, prepared.Wire.MessageHash, prepared.Wire.RecentBlockhash, prepared.Wire.LastValidBlockHeight, int64(prepared.EstimatedFeeLamports), int64(prepared.EstimatedRentLamports), int64(prepared.EstimatedReclaimedLamports))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("lookup retained source signature requires reconciliation")
		}
		prefix, suffix := append([]string{}, i.Prefix...), append([]string{}, i.Extension...)
		out, err = scanLookupAttempt(tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_signed_attempts(operation_id,attempt_number,source_fencing_token,signing_context_slot,cluster,family_id,route_lookup_table_id,operation_kind,table_address,authority,payer,recipient,generation,mutation_epoch,recent_slot,expected_deactivation_slot,prefix_addresses,extension_addresses,expected_prefix_hash,transaction_signature,message_hash,signed_transaction,signed_transaction_sha256,recent_blockhash,last_valid_block_height,estimated_fee_lamports,estimated_rent_lamports,estimated_reclaimed_rent_lamports)
 VALUES($1,(SELECT COALESCE(max(attempt_number),0)+1 FROM loyal_yield.lookup_table_signed_attempts WHERE operation_id=$1),$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27) RETURNING `+lookupAttemptColumns,
			i.OperationID, operation.Lease.FencingToken, prepared.SigningContextSlot, i.Cluster, i.FamilyID, i.TableID, i.Kind, i.TableAddress, i.Authority, i.Payer, i.Recipient, i.Generation, i.MutationEpoch, i.RecentSlot, i.ExpectedDeactivationSlot, prefix, suffix, lookupOrderedAddressHash(prefix), prepared.Wire.TransactionSignature, prepared.Wire.MessageHash, prepared.Wire.SignedTransaction, prepared.Wire.SignedTransactionHash, prepared.Wire.RecentBlockhash, prepared.Wire.LastValidBlockHeight, int64(prepared.EstimatedFeeLamports), int64(prepared.EstimatedRentLamports), int64(prepared.EstimatedReclaimedLamports)))
		return err
	})
	return out, err
}
