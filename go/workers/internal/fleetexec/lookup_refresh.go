package fleetexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	sdk "github.com/solana-foundation/solana-go/v2"
)

func lookupProducedSlot(slots []uint64, wanted uint64) bool {
	for _, slot := range slots {
		if slot == wanted {
			return true
		}
	}
	return false
}
func lookupDerivedAddress(authority string, slot uint64) (string, error) {
	key, err := sdk.PublicKeyFromBase58(authority)
	if err != nil {
		return "", errors.New("lookup reservation authority invalid")
	}
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], slot)
	address, _, err := sdk.FindProgramAddress([][]byte{key[:], seed[:]}, sdk.MustPublicKeyFromBase58(lookupProgram))
	return address.String(), err
}

// Refresh uses only actual produced bank entries, not fabricated numeric slots.
// The proposed account is explicitly absent at/after this bank before any lock.
func (w *LookupWorker) refreshCreate(ctx context.Context, op LookupOperation, old LookupSnapshot) (LookupOperation, error) {
	if op.Intent.RecentSlot == nil || !old.Absent || len(op.Intent.Prefix) != 0 || (op.Intent.Kind != LookupCreate && op.Intent.Kind != LookupRollover) {
		return op, errors.New("lookup create refresh lacks empty unsigned reservation")
	}
	for _, slot := range old.SlotHashes[:min(256, len(old.SlotHashes))] {
		if slot > math.MaxInt64 || slot > uint64(old.Slot) {
			continue
		}
		address, err := lookupDerivedAddress(op.Intent.Authority, slot)
		if err != nil {
			return op, err
		}
		if address == op.Intent.TableAddress {
			continue
		}
		snapshot, err := w.chain.LookupSnapshot(ctx, address, old.Slot)
		if err != nil {
			return op, err
		}
		if !snapshot.Absent || !lookupProducedSlot(snapshot.SlotHashes, slot) {
			continue
		}
		updated, changed, err := w.store.refreshLookupReservation(ctx, op, snapshot, slot)
		if err != nil {
			return op, err
		}
		if changed {
			return updated, nil
		}
	}
	return op, errors.New("lookup actual recent banks have no unoccupied source PDA")
}

func (s *Store) refreshLookupReservation(ctx context.Context, op LookupOperation, prospective LookupSnapshot, recent uint64) (LookupOperation, bool, error) {
	result := op
	changed := false
	address, err := lookupDerivedAddress(op.Intent.Authority, recent)
	if err != nil {
		return result, false, err
	}
	if !prospective.Absent || prospective.Address != address || prospective.Slot <= 0 || recent > uint64(prospective.Slot) || !lookupProducedSlot(prospective.SlotHashes, recent) || recent > math.MaxInt64 {
		return result, false, errors.New("lookup proposed PDA lacks actual absent recent bank evidence")
	}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, op.Intent, op.Lease.Owner)
		if err != nil {
			return err
		}
		source, err := lookupLockSource(ctx, tx, op.Intent, op.Lease)
		if err != nil {
			return err
		}
		if paused || !lookupFamilyAllows(source.familyState, op.Intent.Kind) {
			return ErrLookupPaused
		}
		if source.state != "leased" || (op.Intent.Kind != LookupCreate && op.Intent.Kind != LookupRollover) || len(op.Intent.Prefix) != 0 || (source.tableState != "preparing" && source.tableState != "warming") {
			return errors.New("lookup reservation is no longer empty and unsigned")
		}
		if err = lookupSourceMembership(ctx, tx, op.Intent); err != nil {
			return err
		}
		if err = lookupUnsignedGuards(ctx, tx, op.Intent, source); err != nil {
			return err
		}
		var rekeyable bool
		if err = tx.QueryRow(ctx, `SELECT o.transaction_signature IS NULL AND o.message_hash IS NULL AND o.recent_blockhash IS NULL AND o.last_valid_block_height IS NULL AND t.create_signature IS NULL AND t.address_count=0 AND t.usable_address_count=0 FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.route_lookup_tables t ON t.id=o.route_lookup_table_id WHERE o.id=$1`, op.Intent.OperationID).Scan(&rekeyable); err != nil {
			return err
		}
		if !rekeyable {
			return errors.New("lookup owned packet prevents reservation refresh")
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, op.Intent.Authority); err != nil {
			return err
		}
		var occupied bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.route_lookup_tables WHERE table_address=$1)`, address).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return nil
		}
		var operationContext map[string]json.RawMessage
		if err = json.Unmarshal(source.context, &operationContext); err != nil || operationContext == nil {
			return errors.New("lookup source refresh context is not an object")
		}
		operationContext["recent_slot"], _ = json.Marshal(recent)
		delete(operationContext, "recentSlot")
		encoded, err := json.Marshal(operationContext)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET table_address=$3,updated_at=clock_timestamp() WHERE id=$1 AND table_address=$2 AND mutation_epoch=$4 AND address_count=0 AND usable_address_count=0 AND create_signature IS NULL AND desired_state IN ('preparing','warming')`, op.Intent.TableID, op.Intent.TableAddress, address, op.Intent.MutationEpoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_context=$4,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND operation_state='leased' AND transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL`, op.Intent.OperationID, op.Lease.Owner, op.Lease.FencingToken, encoded)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		result.Intent.TableAddress = address
		result.Intent.RecentSlot = &recent
		result.Context = encoded
		changed = true
		return nil
	})
	return result, changed, err
}
