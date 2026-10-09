package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// ExecuteFresh signs and persists the route this process just admitted. The
// publication transaction rechecks every custody fence the database holds
// before the signed wire becomes recoverable. Nothing is broadcast here.
func (w *Worker) ExecuteFresh(ctx context.Context, admission fleet.ExecutionAdmission) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if admission.Lease.Cluster != w.config.Cluster || admission.Lease.Owner != w.config.Owner {
		return 0, ErrStaleOwner
	}
	deadline := leaseWorkDeadline(admission.Lease.ExpiresAt, w.config.LeaseTTL)
	if !deadline.After(time.Now()) {
		return 0, ErrStaleOwner
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	wire, err := w.signer.SignPreparedRoute(admission.Preparation.Transaction, admission.LastValidBlockHeight)
	if err != nil {
		return 0, err
	}
	var balance *FeePayerBalance
	if admission.FeePayer != base58.Encode(w.signer.FeePayer[32:]) {
		// Rust admits a shard's spend against a balance read within two
		// seconds of the admission, so it is read here, last.
		if w.balances == nil {
			return 0, errors.New("fee-only payer balance reader is not configured")
		}
		slot, accounts, err := fleet.ReadAccounts(ctx, w.balances, []string{admission.FeePayer}, rpc.CommitmentConfirmed, admission.Evidence.Slot)
		if err != nil || accounts[0] == nil || accounts[0].Lamports > math.MaxInt64 {
			return 0, fmt.Errorf("fee-only payer balance unavailable: %v", err)
		}
		balance = &FeePayerBalance{Lamports: int64(accounts[0].Lamports), Slot: slot, At: time.Now()}
	}
	return w.store.PersistFreshAdmission(ctx, admission, wire, balance)
}

// FeePayerBalance is one confirmed balance read of a fee-only shard.
type FeePayerBalance struct {
	Lamports, Slot int64
	At             time.Time
}

// errFeePayerReselection is Rust's fee_payer_reselection_required: the shard
// cannot fund this route now; the next preparation ranks shards again.
var errFeePayerReselection = errors.New("fee_payer_reselection_required")

// admitFeeOnlySpend is Rust's reserve_fee_only_route_payer_spend under the
// shard row lock: the balance read is current, the fee fits the per-
// transaction and rolling-window budgets, and the balance stays within the
// floor and ceiling after every fee it cannot yet reflect.
func admitFeeOnlySpend(ctx context.Context, tx pgx.Tx, cluster, payer, semantic string, opportunityID, submissionID, fee int64, b FeePayerBalance) error {
	var minimum, maximum, windowMax, txMax int64
	var window int32
	var checkedAt time.Time
	err := tx.QueryRow(ctx, `SELECT s.minimum_balance_lamports,s.maximum_balance_lamports,s.rolling_window_seconds,s.maximum_window_spend_lamports,s.maximum_transaction_fee_lamports,clock_timestamp()
  FROM loyal_yield.route_fee_payer_shards s
  WHERE s.cluster=$1 AND s.fee_payer=$2 AND s.enabled
   AND EXISTS(SELECT 1 FROM loyal_yield.route_fee_payer_shard_status v WHERE v.cluster=s.cluster AND v.fee_payer=s.fee_payer AND v.database_authority_separation_passes)
  FOR UPDATE OF s`, cluster, payer).Scan(&minimum, &maximum, &window, &windowMax, &txMax, &checkedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: payer is not an enabled durable shard", errFeePayerReselection)
	}
	if err != nil {
		return err
	}
	if b.At.Before(checkedAt.Add(-2*time.Second)) || b.At.After(checkedAt.Add(5*time.Second)) || fee > txMax {
		return fmt.Errorf("%w: balance read is stale or fee exceeds the per-transaction budget", errFeePayerReselection)
	}
	var unreflected, spent int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(r.compiled_fee_lamports),0)::BIGINT FROM loyal_yield.route_fee_payer_spend_reservations r JOIN loyal_yield.signed_route_submissions s ON s.id=r.signed_submission_id
  WHERE r.cluster=$1 AND r.fee_payer=$2 AND ((s.confirmed_slot IS NULL AND s.submission_state NOT IN ('reconciled','expired','failed')) OR s.confirmed_slot>$3)`, cluster, payer, b.Slot).Scan(&unreflected); err != nil {
		return err
	}
	if b.Lamports < minimum || b.Lamports > maximum || b.Lamports-unreflected-fee < minimum {
		return fmt.Errorf("%w: balance is outside the shard's floor and ceiling", errFeePayerReselection)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(compiled_fee_lamports),0)::BIGINT FROM loyal_yield.route_fee_payer_spend_reservations WHERE cluster=$1 AND fee_payer=$2 AND created_at>=clock_timestamp()-$3*interval '1 second'`, cluster, payer, window).Scan(&spent); err != nil {
		return err
	}
	if spent+fee > windowMax {
		return fmt.Errorf("%w: rolling spend budget is exhausted", errFeePayerReselection)
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.route_fee_payer_spend_reservations(cluster,fee_payer,semantic_key,opportunity_id,signed_submission_id,compiled_fee_lamports,observed_balance_lamports,observed_balance_slot,observed_balance_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		cluster, payer, semantic, opportunityID, submissionID, fee, b.Lamports, b.Slot, b.At)
	return err
}

// PersistFreshAdmission mirrors the registered execute-opportunity trigger:
// insert signed submission with decision=NULL, insert decision (links the
// opportunity and submission), then bind capacity/conflicts/ALT usage, and
// perform the final publication lifetime check before commit.
func (s *Store) PersistFreshAdmission(ctx context.Context, a fleet.ExecutionAdmission, wire WireIdentity, balance *FeePayerBalance) (int64, error) {
	sum := sha256.Sum256(wire.SignedTransaction)
	if hex.EncodeToString(sum[:]) != wire.SignedTransactionHash || wire.MessageHash != a.Preparation.Transaction.MessageSHA256 || wire.LastValidBlockHeight != a.LastValidBlockHeight {
		return 0, errors.New("signed admission identity differs from preparation")
	}
	decoded, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil {
		return 0, err
	}
	canonical, err := decoded.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, wire.SignedTransaction) {
		return 0, errors.New("signed admission wire is not canonical")
	}
	message, err := decoded.Message.MarshalBinary()
	if err != nil || !bytes.Equal(message, a.Preparation.Transaction.Message) || len(decoded.Message.AccountKeys) == 0 || len(decoded.Signatures) < 1 || len(decoded.Signatures) > 2 || decoded.Signatures[0].String() != wire.TransactionSignature || decoded.Message.RecentBlockhash.String() != wire.RecentBlockhash {
		return 0, errors.New("signed admission message/signature differs")
	}
	if err := decoded.VerifySignatures(); err != nil {
		return 0, err
	}
	payer := decoded.Message.AccountKeys[0].String()
	// One signature: the delegate pays. Two: the vault's fee-only payer pays,
	// with the Rust spend reservation 0025 requires for that payer kind.
	payerKind := "policy"
	if len(decoded.Signatures) == 2 {
		payerKind = "fee_only_shard"
		if a.FeePayer != payer || balance == nil || balance.Slot <= 0 {
			return 0, errors.New("fee-only payer differs from admission or lacks its observed balance")
		}
	}
	semantic := fmt.Sprintf("fleet-opportunity:%d", a.Lease.OpportunityID)
	epochs, err := marshalALTEpochs(a.SelectedALTs)
	if err != nil {
		return 0, err
	}
	expected, err := json.Marshal(struct {
		Kind    string                        `json:"kind"`
		Anchors fleet.ExecutionBalanceAnchors `json:"anchors"`
	}{"same_mint", a.Anchors})
	if err != nil {
		return 0, err
	}
	var submissionID int64
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		l, p := a.Lease, a.Preparation
		var actualPlan json.RawMessage
		var snapshot *int64
		err := tx.QueryRow(ctx, `SELECT o.execution_plan,o.source_snapshot_id
  FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
  WHERE o.id=$1 AND o.cluster=$2 AND o.opportunity_state='leased' AND o.lease_kind='execute'
   AND o.lease_owner=$3 AND o.fencing_token=$4 AND o.lease_expires_at>clock_timestamp()
   AND o.optimizer_epoch_id=$5 AND e.epoch_key=$6 AND o.vault_id=$7
   AND o.route_fingerprint=$8 AND o.requirements_fingerprint=$9
   AND o.target_reserve=$10 AND o.liquidity_mint=$11 AND o.amount_raw=$12
   AND o.source_reserve IS NOT DISTINCT FROM $13::text
   AND o.expires_at>=clock_timestamp()+interval '60 seconds' AND e.expires_at>=clock_timestamp()+interval '60 seconds'
  FOR UPDATE OF o FOR SHARE OF e`, l.OpportunityID, l.Cluster, l.Owner, l.FencingToken, l.OptimizerEpochID, l.OptimizerEpochKey, l.VaultID, p.RouteFingerprint, p.RequirementsFingerprint, l.TargetReserve, l.LiquidityMint, int64(l.LiquidityAmountRaw), nullString(l.SourceReserve)).Scan(&actualPlan, &snapshot)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(snapshot, l.SourceSnapshotID) || !sameJSON(actualPlan, p.ExecutionPlan) {
			return errors.New("fresh admission snapshot/plan differs from execute opportunity")
		}
		var capacityID int64
		if err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.target_capacity_reservations
   WHERE id=$1 AND opportunity_id=$2 AND cluster=$3 AND target_reserve=$4 AND liquidity_mint=$5
    AND reservation_state='active' AND decision_id IS NULL AND signed_submission_id IS NULL
    AND reservation_generation=$6 AND reservation_fencing_token=$7 AND principal_usd_micros=$8
   FOR UPDATE`, a.CapacityReservationID, l.OpportunityID, l.Cluster, l.TargetReserve, l.LiquidityMint, a.ReservationGeneration, a.CapacityFencingToken, l.PrincipalUSDMicros).Scan(&capacityID); err != nil {
			return fmt.Errorf("capacity admission changed: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT writable_account_key FROM loyal_yield.route_account_conflict_leases
   WHERE cluster=$1 AND opportunity_id=$2 AND lease_owner=$3 AND fencing_token=$4 AND submission_id IS NULL AND expires_at>clock_timestamp()
   ORDER BY writable_account_key FOR UPDATE`, l.Cluster, l.OpportunityID, l.Owner, l.FencingToken)
		if err != nil {
			return err
		}
		keys := []string{}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !sameStrings(keys, a.ConflictKeys) {
			return ErrConflictLeaseHeld
		}
		if err := lockAdmissionALTs(ctx, tx, a); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.signed_route_submissions
   (cluster,semantic_key,opportunity_id,signed_transaction,signed_transaction_hash,message_hash,transaction_signature,recent_blockhash,last_valid_block_height,
    source_snapshot_id,optimizer_epoch_id,alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer,fee_payer_kind,compiled_fee_lamports,
    writable_account_keys,conflict_account_keys,executor_owner,executor_fencing_token,movement_leg,leg_purpose,leg_generation,expected_effect,expected_balance_anchors)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$22,$16,$17,$18,$19,$20,'route','optimize_yield',1,$21,'{}') RETURNING id`,
			l.Cluster, semantic, l.OpportunityID, wire.SignedTransaction, wire.SignedTransactionHash, wire.MessageHash, wire.TransactionSignature, wire.RecentBlockhash, wire.LastValidBlockHeight,
			l.SourceSnapshotID, l.OptimizerEpochID, p.RequirementsFingerprint, a.AltSelectionFingerprint, epochs, payer, int64(p.Transaction.FeeLamports), p.Transaction.WritableAccounts, keys, l.Owner, l.FencingToken, expected, payerKind).Scan(&submissionID); err != nil {
			return err
		}
		if payerKind == "fee_only_shard" {
			if err := admitFeeOnlySpend(ctx, tx, l.Cluster, payer, semantic, l.OpportunityID, submissionID, int64(p.Transaction.FeeLamports), *balance); err != nil {
				return err
			}
		}
		var decisionID int64
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_decisions
   (vault_id,source_snapshot_id,status,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,
    amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,decision_reason,execution_plan,idempotency_key,preflight_chain_slot)
   SELECT vault_id,source_snapshot_id,'ready',source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,
    amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,'target_supply_apy_exceeds_source',execution_plan,$2,$3
   FROM loyal_yield.rebalance_opportunities WHERE id=$1 RETURNING id`, l.OpportunityID, semantic, p.Simulation.Slot).Scan(&decisionID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET decision_id=$2,signed_submission_id=$3,updated_at=clock_timestamp()
   WHERE id=$1 AND decision_id IS NULL AND signed_submission_id IS NULL AND reservation_state='active'`, capacityID, decisionID, submissionID)
		if err != nil || tag.RowsAffected() != 1 {
			return errors.New("capacity signed handoff fenced")
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases SET submission_id=$5,expires_at=GREATEST(expires_at,clock_timestamp()+interval '10 minutes'),updated_at=clock_timestamp()
   WHERE cluster=$1 AND opportunity_id=$2 AND lease_owner=$3 AND fencing_token=$4 AND writable_account_key=ANY($6) AND submission_id IS NULL AND expires_at>clock_timestamp()`, l.Cluster, l.OpportunityID, l.Owner, l.FencingToken, submissionID, keys)
		if err != nil || tag.RowsAffected() != int64(len(keys)) {
			return ErrConflictLeaseHeld
		}
		for _, table := range a.SelectedALTs {
			_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases
    (cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,binding_id,route_fingerprint,requirements_fingerprint,expires_at)
    VALUES($1,'prepared_transaction',$2,$3,$4,$5,$6,$7,clock_timestamp()+interval '5 minutes')`, l.Cluster, semantic, table.TableID, l.VaultID, table.BindingID, p.RouteFingerprint, p.RequirementsFingerprint)
			if err != nil {
				return err
			}
		}
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id
   JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
   WHERE s.id=$1 AND s.decision_id=$2 AND o.decision_id=$2 AND o.opportunity_state='decision_created' AND s.submission_state='signed'
    AND o.expires_at>=clock_timestamp()+interval '60 seconds' AND e.expires_at>=clock_timestamp()+interval '60 seconds')`, submissionID, decisionID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return ErrStaleOwner
		}
		return nil
	})
	return submissionID, err
}

// Lock the same parent identities as fresh admission, in the lookup writers'
// family, binding, table order, then compare the exact normalized member
// vector used by compilation. Mutators serialize on these rows; a binding
// identity cannot be adopted from an unlocked EXISTS result.
func lockAdmissionALTs(ctx context.Context, tx pgx.Tx, a fleet.ExecutionAdmission) error {
	tables, err := fleet.LockExecutionALTParents(ctx, tx, a.SelectedALTs)
	if err != nil {
		return err
	}
	for _, table := range tables {
		var kind string
		err := tx.QueryRow(ctx, `SELECT f.kind FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_families f ON f.id=t.family_id
   WHERE t.id=$1 AND t.table_address=$2 AND t.cluster=$3 AND t.mutation_epoch=$4 AND t.family_id=$5 AND t.generation=$6
    AND t.durable AND t.desired_state='active' AND t.status IN ('active','usable') AND t.deactivated_slot IS NULL
    AND f.cluster=t.cluster AND f.desired_state='active' AND f.active_generation=t.generation
    AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations op WHERE op.route_lookup_table_id=t.id AND op.operation_kind IN ('create','extend','rollover','deactivate','close')
     AND (op.operation_state IN ('signed','submitted','confirmed','finalized','reconciled','needs_reconcile') OR op.operation_state IN ('leased','retry_wait') AND op.transaction_signature IS NOT NULL))
   FOR SHARE OF t`, table.TableID, table.Address, a.Lease.Cluster, table.MutationEpoch, table.FamilyID, table.Generation).Scan(&kind)
		if err != nil {
			return fmt.Errorf("selected ALT changed before signed publication: %w", err)
		}
		if kind == "vault_shards" {
			if table.BindingID == nil {
				return errors.New("vault ALT lacks binding identity")
			}
			var valid bool
			err = tx.QueryRow(ctx, `SELECT route_lookup_table_id=$2 AND vault_id=$3 AND lifecycle_state='active' FROM loyal_yield.lookup_table_vault_bindings WHERE id=$1`, *table.BindingID, table.TableID, a.Lease.VaultID).Scan(&valid)
			if err != nil {
				return fmt.Errorf("vault ALT binding before publication: %w", err)
			}
			if !valid {
				return errors.New("vault ALT binding changed before publication")
			}
		} else if kind != "shared_market" || table.BindingID != nil {
			return errors.New("ALT family does not match route scope")
		}
		var members []string
		var usable bool
		if err := tx.QueryRow(ctx, `SELECT array_agg(address ORDER BY ordinal),COALESCE(bool_and(usable_after_slot<=$2 AND last_verified_slot IS NOT NULL),false) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, table.TableID, a.Evidence.Slot).Scan(&members, &usable); err != nil {
			return err
		}
		if !usable || !reflect.DeepEqual(members, table.Addresses) {
			return errors.New("ALT membership changed after simulation")
		}
	}
	return nil
}

func marshalALTEpochs(tables []fleet.ExecutionALT) ([]byte, error) {
	type epoch struct {
		TableID       int64  `json:"tableId"`
		TableAddress  string `json:"tableAddress"`
		MutationEpoch int64  `json:"mutationEpoch"`
		FamilyID      int64  `json:"familyId"`
		Generation    int64  `json:"generation"`
		BindingID     *int64 `json:"bindingId"`
	}
	out := struct {
		Tables []epoch `json:"tables"`
	}{Tables: make([]epoch, 0, len(tables))}
	for _, t := range tables {
		out.Tables = append(out.Tables, epoch{t.TableID, t.Address, t.MutationEpoch, t.FamilyID, t.Generation, t.BindingID})
	}
	return json.Marshal(out)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}
func sameJSON(a, b []byte) bool {
	var x, y any
	dx, dy := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	if dx.Decode(&x) != nil || dy.Decode(&y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
