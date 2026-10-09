package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/mr-tron/base58"
)

// lookupSignedContext is the part of a signed packet the Rust columns cannot
// hold. It lives in operation_context next to signedExpectedReclaimedRentLamports.
type lookupSignedContext struct {
	SignedTransaction  string `json:"signedTransaction"`
	SigningContextSlot int64  `json:"signingContextSlot"`
	BroadcastCount     int    `json:"broadcastCount"`
	Reclaimed          uint64 `json:"signedExpectedReclaimedRentLamports"`
}

// lookupAttemptOf reads the signed packet a source operation carries.
func lookupAttemptOf(op LookupOperation) (LookupAttempt, error) {
	if op.Signature == nil || op.MessageHash == nil || op.Blockhash == nil || op.LastValidBlockHeight == nil {
		return LookupAttempt{}, errors.New("lookup signed packet identity incomplete")
	}
	var c lookupSignedContext
	if err := json.Unmarshal(op.Context, &c); err != nil {
		return LookupAttempt{}, err
	}
	a := LookupAttempt{Intent: op.Intent, SigningContextSlot: c.SigningContextSlot, BroadcastCount: c.BroadcastCount, EstimatedReclaimedLamports: c.Reclaimed,
		Wire: WireIdentity{TransactionSignature: *op.Signature, MessageHash: *op.MessageHash, RecentBlockhash: *op.Blockhash, LastValidBlockHeight: *op.LastValidBlockHeight}}
	if c.SignedTransaction == "" {
		return a, nil
	}
	wire, err := base64.StdEncoding.DecodeString(c.SignedTransaction)
	if err != nil {
		return a, err
	}
	if len(wire) < 65 || wire[0] == 0 || base58.Encode(wire[1:65]) != *op.Signature {
		// Rust re-signed this operation after a handover and never clears our
		// keys: the stored bytes belong to an earlier packet. The columns are
		// the packet; land it by signature alone.
		return LookupAttempt{Intent: op.Intent, Wire: a.Wire}, nil
	}
	sum := sha256.Sum256(wire)
	a.Wire.SignedTransaction, a.Wire.SignedTransactionHash = wire, hex.EncodeToString(sum[:])
	return a, proveLookupWire(a.Intent, a.Wire)
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

// PersistLookupPrepared writes the signed packet onto its source operation
// before any send: the Rust identity columns plus the exact bytes in
// operation_context. A matching source budget reservation must already exist.
func (s *Store) PersistLookupPrepared(ctx context.Context, operation LookupOperation, prepared LookupAttempt) (LookupAttempt, error) {
	i := prepared.Intent
	if operation.Intent.OperationID != i.OperationID || prepared.SigningContextSlot <= 0 || (i.RecentSlot != nil && (*i.RecentSlot > math.MaxInt64 || *i.RecentSlot > uint64(prepared.SigningContextSlot))) || (i.ExpectedDeactivationSlot != nil && *i.ExpectedDeactivationSlot > math.MaxInt64) || prepared.EstimatedFeeLamports > math.MaxInt64 || prepared.EstimatedRentLamports > math.MaxInt64 || prepared.EstimatedReclaimedLamports > math.MaxInt64 {
		return prepared, errors.New("lookup prepared bank/accounting identity incomplete")
	}
	if err := proveLookupWire(i, prepared.Wire); err != nil {
		return prepared, err
	}
	signed, err := json.Marshal(lookupSignedContext{SignedTransaction: base64.StdEncoding.EncodeToString(prepared.Wire.SignedTransaction), SigningContextSlot: prepared.SigningContextSlot, Reclaimed: prepared.EstimatedReclaimedLamports})
	if err != nil {
		return prepared, err
	}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, i, operation.Lease.Owner)
		if err != nil {
			return err
		}
		locked, err := lookupLockSource(ctx, tx, i, operation.Lease)
		if err != nil {
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
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='signed',transaction_signature=$2,message_hash=$3,recent_blockhash=$4,last_valid_block_height=$5,estimated_fee_lamports=$6,estimated_rent_lamports=$7,operation_context=operation_context||$8::jsonb,error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL`, i.OperationID, prepared.Wire.TransactionSignature, prepared.Wire.MessageHash, prepared.Wire.RecentBlockhash, prepared.Wire.LastValidBlockHeight, int64(prepared.EstimatedFeeLamports), int64(prepared.EstimatedRentLamports), signed)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("lookup retained source signature requires reconciliation")
		}
		return nil
	})
	return prepared, err
}
