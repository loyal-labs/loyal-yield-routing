package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// Recovery validates the immutable signed bytes again before the first send;
// none of the DTO metadata alone grants a signing or broadcasting capability.
func verifyDurableWire(r SubmissionRecord) error {
	tx, err := sdk.TransactionFromBytes(r.SignedTransaction)
	if err != nil {
		return fmt.Errorf("durable signed wire: %w", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(raw, r.SignedTransaction) {
		return errors.New("durable signed wire is not canonical")
	}
	if len(tx.Message.AccountKeys) == 0 || tx.Message.AccountKeys[0].String() != r.FeePayer || len(tx.Signatures) != 1 || tx.Signatures[0].String() != r.Signature || tx.Message.RecentBlockhash.String() != r.RecentBlockhash {
		return errors.New("durable wire identity differs from journal")
	}
	if err := tx.VerifySignatures(); err != nil {
		return err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return err
	}
	hash := sha256.Sum256(message)
	if hex.EncodeToString(hash[:]) != r.MessageHash {
		return errors.New("durable signed message hash differs")
	}
	return nil
}

type preparedALTEpoch struct {
	TableID       int64 `json:"tableId"`
	MutationEpoch int64 `json:"mutationEpoch"`
}

func parsePreparedALTEpochs(raw []byte) ([]preparedALTEpoch, error) {
	var body struct {
		Tables []preparedALTEpoch `json:"tables"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if len(body.Tables) == 0 {
		return nil, errors.New("signed route has no prepared ALT proof")
	}
	seen := map[int64]bool{}
	for _, t := range body.Tables {
		if t.TableID <= 0 || t.MutationEpoch < 0 || seen[t.TableID] {
			return nil, errors.New("signed route has invalid ALT proof")
		}
		seen[t.TableID] = true
	}
	// A missing mutationEpoch would decode to zero. Check presence separately;
	// zero itself is a valid epoch in the legacy schema.
	var generic struct {
		Tables []map[string]json.RawMessage `json:"tables"`
	}
	_ = json.Unmarshal(raw, &generic)
	for _, t := range generic.Tables {
		if len(t["mutationEpoch"]) == 0 || bytes.Equal(t["mutationEpoch"], []byte("null")) {
			return nil, errors.New("ALT proof lacks mutation epoch")
		}
	}
	return body.Tables, nil
}
func checkPreparedALTUsage(ctx context.Context, tx pgx.Tx, raw []byte, semantic, cluster, requirements string) error {
	expected, err := parsePreparedALTEpochs(raw)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT t.id,t.mutation_epoch,t.desired_state='active' AND t.family_id IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations m WHERE m.route_lookup_table_id=t.id
 AND m.operation_kind IN ('create','extend','rollover','deactivate','close')
 AND (m.operation_state IN ('signed','submitted','confirmed','finalized','reconciled','needs_reconcile') OR m.operation_state IN ('leased','retry_wait') AND m.transaction_signature IS NOT NULL))
 FROM loyal_yield.lookup_table_usage_leases u JOIN loyal_yield.route_lookup_tables t ON t.id=u.route_lookup_table_id AND t.cluster=u.cluster
 WHERE u.cluster=$1 AND u.lease_kind='prepared_transaction' AND u.reference_key=$2 AND u.requirements_fingerprint=$3
 AND u.released_at IS NULL AND u.expires_at>clock_timestamp() ORDER BY t.id FOR SHARE OF t,u`, cluster, semantic, requirements)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := map[int64]int64{}
	for rows.Next() {
		var id, epoch int64
		var valid bool
		if err := rows.Scan(&id, &epoch, &valid); err != nil {
			return err
		}
		if !valid {
			return errors.New("prepared ALT mutated before send")
		}
		if _, dup := actual[id]; dup {
			return errors.New("duplicate prepared ALT usage")
		}
		actual[id] = epoch
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("prepared ALT usage expired or incomplete")
	}
	for _, e := range expected {
		epoch, ok := actual[e.TableID]
		if !ok || epoch != e.MutationEpoch {
			return errors.New("prepared ALT mutation fence changed")
		}
	}
	return nil
}

// ConfirmSameMint binds the exact signature and slot to its decision and hands
// off reconciliation atomically. Cross-mint needs per-leg finalized custody and
// cannot use this method, even if its signature happens to be confirmed.
func (s *Store) ConfirmSameMint(ctx context.Context, lease SubmissionLease, slot int64) error {
	if slot <= 0 || lease.Submission.MovementLeg != LegRoute {
		return errors.New("same-mint confirmation requires positive slot and route leg")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var decision int64
		var semantic string
		err := tx.QueryRow(ctx, `SELECT s.decision_id,s.semantic_key FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id
   WHERE s.id=$1 AND s.transaction_signature=$4 AND s.submission_state IN ('signed','submitted','confirmed','effect_ambiguous','expiry_check_pending')
   AND s.confirmation_lease_owner=$2 AND s.confirmation_fencing_token=$3 AND s.confirmation_lease_expires_at>clock_timestamp()
   AND (s.confirmed_slot IS NULL OR s.confirmed_slot=$5) AND d.movement_route<>'cross_mint_jupiter'
   AND d.status::text IN ('planned','simulating','ready','submitted','confirming','confirmed') AND (d.signature IS NULL OR d.signature=s.transaction_signature)
   FOR UPDATE OF s,d`, lease.Submission.ID, lease.Owner, lease.FencingToken, lease.Submission.Signature, slot).Scan(&decision, &semantic)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status=CASE WHEN status::text='confirmed' THEN status ELSE 'confirming'::loyal_yield.decision_status END,signature=COALESCE(signature,$2),updated_at=clock_timestamp() WHERE id=$1`, decision, lease.Submission.Signature); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciliation_pending',submitted_slot=COALESCE(submitted_slot,$4),submitted_at=COALESCE(submitted_at,last_broadcast_at,clock_timestamp()),confirmed_slot=COALESCE(confirmed_slot,$4),confirmed_at=COALESCE(confirmed_at,clock_timestamp()),last_status_checked_at=clock_timestamp(),confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp()`, lease.Submission.ID, lease.Owner, lease.FencingToken, slot)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		if _, err = tx.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE submission_id=$1 AND (writable_account_key LIKE 'fleet-shared-write-lane:%' OR writable_account_key LIKE 'policy-setup-funding:%')`, lease.Submission.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET released_at=COALESCE(released_at,clock_timestamp()),updated_at=clock_timestamp() WHERE lease_kind='prepared_transaction' AND reference_key=$1`, semantic)
		return err
	})
}
