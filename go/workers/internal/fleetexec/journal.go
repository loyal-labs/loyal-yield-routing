package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Store is the typed journal over the legacy fleet route tables. It uses the
// same rows and leases the Rust fleet worker and route confirmer use, so old
// and new writers contend on identical custody records; there is no Go-only
// lock table.
type Store struct{ pool *pgxpool.Pool }

// NewStore adopts a caller-owned pool and verifies the legacy schema is
// actually present. Pool sizing and lifecycle belong to the composition root;
// this package never opens, migrates, or closes the database on its own.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	store := &Store{pool: pool}
	if err := store.RequireSchema(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// RequireSchema rejects a pool whose legacy fleet execution tables are absent.
func (s *Store) RequireSchema(ctx context.Context) error {
	return db.RequireTables(ctx, s.pool,
		"loyal_yield.signed_route_submissions",
		"loyal_yield.route_account_conflict_leases",
		"loyal_yield.target_capacity_reservations",
		"loyal_yield.target_capacity_frontiers",
		"loyal_yield.lookup_table_usage_leases",
	)
}

// Pool exposes the caller-owned pool so the composition root retains its
// lifecycle. The store never closes it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// PersistRouteInput is one durable broadcast intent for exact signed bytes.
type PersistRouteInput struct {
	Cluster              string
	SemanticKey          string
	OpportunityID        int64
	DecisionID           *int64
	Wire                 WireIdentity
	FeePayer             string
	CompiledFeeLamports  int64
	WritableAccountKeys  []string
	ConflictAccountKeys  []string
	ExecutorOwner        string
	ExecutorFencingToken int64
	MovementLeg          string
	LegPurpose           string
	LegGeneration        int64
	OptimizerEpochID     int64
	// AltRequirementsFingerprint and AltSelectionFingerprint are the exact
	// reusable-ALT constraints the route was compiled under.
	AltRequirementsFingerprint string
	AltSelectionFingerprint    string
	AltMutationEpochs          json.RawMessage
	// ExpectedEffect and ExpectedBalanceAnchors are the receipt contract the
	// reconciliation must verify; they are written with the wire and immutable.
	ExpectedEffect          json.RawMessage
	ExpectedBalanceAnchors  json.RawMessage
	ConflictLeaseExpiration time.Time
}

// PersistSignedRoute writes the exact signed wire, its hashes and its
// execution evidence in one transaction with the legacy route account
// conflict leases. A contended insert (same semantic key, opportunity fence,
// or signature) returns the existing live row only when it carries identical
// wire; anything else is refused so two writers cannot spend twice.
func (s *Store) PersistSignedRoute(ctx context.Context, input PersistRouteInput) (id int64, reused bool, err error) {
	if input.Cluster == "" || input.SemanticKey == "" || input.OpportunityID <= 0 {
		return 0, false, errors.New("signed route identity is incomplete")
	}
	if len(input.Wire.SignedTransaction) == 0 || len(input.Wire.SignedTransaction) > SolanaPacketLimit {
		return 0, false, errors.New("signed route packet is out of bounds")
	}
	if input.Wire.LastValidBlockHeight <= 0 || input.Wire.TransactionSignature == "" {
		return 0, false, errors.New("signed route evidence is incomplete")
	}
	if len(input.WritableAccountKeys) == 0 || len(input.ConflictAccountKeys) < 2 {
		return 0, false, errors.New("signed route writable or conflict evidence is incomplete")
	}
	if !containsString(input.WritableAccountKeys, input.FeePayer) {
		return 0, false, errors.New("fee payer is not a writable account of the signed route")
	}
	if input.DecisionID == nil {
		return 0, false, errors.New("signed route requires its rebalance decision identity")
	}
	if input.MovementLeg == "" || input.LegPurpose == "" || input.LegGeneration <= 0 {
		return 0, false, errors.New("signed route movement leg is incomplete")
	}
	if input.MovementLeg != LegRoute || !legalLegPurpose(input.LegPurpose) {
		return 0, false, errors.New("signed route movement leg is outside the schema vocabulary")
	}
	if len(input.ExpectedEffect) == 0 || !json.Valid(input.ExpectedEffect) {
		return 0, false, errors.New("signed route requires an expected effect receipt contract")
	}
	if len(input.AltMutationEpochs) == 0 || !json.Valid(input.AltMutationEpochs) {
		return 0, false, errors.New("signed route requires ALT mutation evidence")
	}
	if !input.ConflictLeaseExpiration.After(time.Now()) {
		return 0, false, errors.New("conflict lease expiration is not in the future")
	}

	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var family bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id WHERE o.id=$1 AND d.id=$2 AND o.cluster=$3 AND d.movement_route='same_mint' AND o.execution_plan->>'route_kind'='same_mint' AND o.execution_plan->>'source_kind' IN ('reserve_position','idle_vault_usdc'))`, input.OpportunityID, *input.DecisionID, input.Cluster).Scan(&family); err != nil {
			return err
		}
		if !family {
			return errors.New("signed adoption requires a supported same-mint family")
		}
		// A live submission for this opportunity is custody, not a free slot:
		// the exact live attempt decides the outcome. Identical signed bytes
		// are the same durable intent (a restart); different bytes would be a
		// second spend of the same target capacity and are refused. Only a
		// terminal prior attempt leaves the opportunity free for new wire.
		var liveID int64
		var liveHash string
		liveErr := tx.QueryRow(ctx, `
SELECT id, signed_transaction_hash FROM loyal_yield.signed_route_submissions
WHERE opportunity_id=$1 AND submission_state NOT IN ('reconciled','expired','failed')
ORDER BY id LIMIT 1`, input.OpportunityID).Scan(&liveID, &liveHash)
		switch {
		case liveErr == nil:
			if liveHash != input.Wire.SignedTransactionHash {
				return fmt.Errorf("%w: live submission %d owns this opportunity with different signed wire", ErrRouteContended, liveID)
			}
			id, reused = liveID, true
			return nil
		case !errors.Is(liveErr, pgx.ErrNoRows):
			return liveErr
		}

		// The insert can still collide on the semantic key or the exact
		// signature. A failed statement aborts a Postgres transaction, so the
		// contended read runs behind a savepoint that is rolled back first —
		// there is no read-after-failure and no invented status.
		if _, err := tx.Exec(ctx, "SAVEPOINT persist_signed_route"); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
INSERT INTO loyal_yield.signed_route_submissions
    (cluster, semantic_key, opportunity_id, decision_id, signed_transaction,
     signed_transaction_hash, message_hash, transaction_signature, recent_blockhash,
     last_valid_block_height, optimizer_epoch_id, alt_requirements_fingerprint,
     alt_selection_fingerprint, alt_mutation_epochs, fee_payer, compiled_fee_lamports,
     writable_account_keys, conflict_account_keys, executor_owner, executor_fencing_token,
     movement_leg, leg_purpose, leg_generation, expected_effect, expected_balance_anchors,
     submission_state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,'signed')
RETURNING id`, input.Cluster, input.SemanticKey, input.OpportunityID, input.DecisionID,
			input.Wire.SignedTransaction, input.Wire.SignedTransactionHash, input.Wire.MessageHash,
			input.Wire.TransactionSignature, input.Wire.RecentBlockhash, input.Wire.LastValidBlockHeight,
			input.OptimizerEpochID, input.AltRequirementsFingerprint, input.AltSelectionFingerprint,
			input.AltMutationEpochs, input.FeePayer, input.CompiledFeeLamports,
			input.WritableAccountKeys, input.ConflictAccountKeys, input.ExecutorOwner,
			input.ExecutorFencingToken, input.MovementLeg, input.LegPurpose, input.LegGeneration,
			input.ExpectedEffect, input.ExpectedBalanceAnchors).Scan(&id)
		if isUniqueViolation(err) {
			if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT persist_signed_route"); rollbackErr != nil {
				return rollbackErr
			}
			var existingID int64
			var existingHash, existingSemantic, existingCluster string
			var existingOpportunity int64
			scanErr := tx.QueryRow(ctx, `
SELECT id, signed_transaction_hash,semantic_key,cluster,opportunity_id FROM loyal_yield.signed_route_submissions
WHERE (semantic_key=$1 OR transaction_signature=$2)
  AND submission_state NOT IN ('reconciled','expired','failed')
ORDER BY id LIMIT 1`, input.SemanticKey, input.Wire.TransactionSignature).Scan(&existingID, &existingHash, &existingSemantic, &existingCluster, &existingOpportunity)
			if scanErr != nil {
				return fmt.Errorf("%w: %v", ErrRouteContended, err)
			}
			if existingHash != input.Wire.SignedTransactionHash || existingSemantic != input.SemanticKey || existingCluster != input.Cluster || existingOpportunity != input.OpportunityID {
				return fmt.Errorf("%w: live submission %d carries different signed wire", ErrRouteContended, existingID)
			}
			id, reused = existingID, true
			return nil
		}
		if err != nil {
			return err
		}

		// The schema's deferred guard requires every signed submission to be
		// capacity-linked before commit: attach the exact live reservation for
		// this opportunity and prove its decision identity matches. There is
		// no signed wire without admitted target capacity.
		var reservationDecision int64
		err = tx.QueryRow(ctx, `
UPDATE loyal_yield.target_capacity_reservations
SET signed_submission_id=$2, decision_id=COALESCE(decision_id,$3), updated_at=now()
WHERE opportunity_id=$1 AND signed_submission_id IS NULL
 AND reservation_state='active' AND (decision_id IS NULL OR decision_id=$3)
RETURNING decision_id`, input.OpportunityID, id, *input.DecisionID).Scan(&reservationDecision)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no unattached target-capacity reservation for opportunity %d", ErrRouteContended, input.OpportunityID)
		}
		if err != nil {
			return err
		}
		if reservationDecision != *input.DecisionID {
			return fmt.Errorf("target-capacity reservation decision %d diverges from signed route decision %d", reservationDecision, *input.DecisionID)
		}

		// Custody follows the submission: each conflict lease binds this
		// submission id. An existing lease is only ever taken over when it is
		// expired or its bound submission is terminal — never merely because
		// the opportunity matches.
		for _, key := range input.ConflictAccountKeys {
			var boundOpportunity int64
			err := tx.QueryRow(ctx, `
INSERT INTO loyal_yield.route_account_conflict_leases
    (cluster, writable_account_key, opportunity_id, lease_owner, fencing_token, expires_at, submission_id)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (cluster, writable_account_key) DO UPDATE
SET lease_owner=EXCLUDED.lease_owner,
    fencing_token=EXCLUDED.fencing_token,
    expires_at=EXCLUDED.expires_at,
    opportunity_id=EXCLUDED.opportunity_id,
    submission_id=EXCLUDED.submission_id,
    updated_at=now()
WHERE (route_account_conflict_leases.submission_id IS NULL
       AND route_account_conflict_leases.opportunity_id=EXCLUDED.opportunity_id
       AND route_account_conflict_leases.lease_owner=EXCLUDED.lease_owner
       AND route_account_conflict_leases.fencing_token=EXCLUDED.fencing_token
       AND route_account_conflict_leases.expires_at > clock_timestamp())
 OR (route_account_conflict_leases.expires_at < clock_timestamp()
       AND route_account_conflict_leases.submission_id IS NULL)
   OR EXISTS (
       SELECT 1 FROM loyal_yield.signed_route_submissions prior
       WHERE prior.id = route_account_conflict_leases.submission_id
         AND prior.submission_state IN ('reconciled','expired','failed')
   )
RETURNING opportunity_id`, input.Cluster, key, input.OpportunityID, input.ExecutorOwner,
				input.ExecutorFencingToken, input.ConflictLeaseExpiration, id).Scan(&boundOpportunity)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrConflictLeaseHeld, key)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return id, reused, nil
}

// RecordBroadcastIntent counts one send on the row before the bytes leave,
// exactly like the Rust confirmer's prepare_signed_route_broadcast_batch:
// broadcast_count increments, error_detail marks the intent and the decision
// moves to confirming. A signed row becomes submitted with its first send.
func (s *Store) RecordBroadcastIntent(ctx context.Context, lease SubmissionLease) error {
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var decision int64
		var epochs json.RawMessage
		var semantic, cluster, requirements string
		err := tx.QueryRow(ctx, `SELECT s.decision_id,s.alt_mutation_epochs,s.semantic_key,s.cluster,s.alt_requirements_fingerprint
   FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id
   WHERE s.id=$1 AND s.transaction_signature=$4 AND s.submission_state IN ('signed','submitted')
    AND s.confirmation_lease_owner=$2 AND s.confirmation_fencing_token=$3
    AND s.confirmation_lease_expires_at>clock_timestamp() AND d.movement_route <> 'cross_mint_jupiter'
    AND d.status::text IN ('planned','simulating','ready','submitted','confirming','confirmed')
    AND (d.signature IS NULL OR d.signature=s.transaction_signature) FOR UPDATE OF s,d`, lease.Submission.ID, lease.Owner, lease.FencingToken, lease.Submission.Signature).Scan(&decision, &epochs, &semantic, &cluster, &requirements)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		if err := checkPreparedALTUsage(ctx, tx, epochs, semantic, cluster, requirements); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status=CASE WHEN status::text='confirmed' THEN status ELSE 'confirming'::loyal_yield.decision_status END,signature=COALESCE(signature,$2),updated_at=clock_timestamp() WHERE id=$1`, decision, lease.Submission.Signature)
		if err != nil || tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='submitted',submitted_at=COALESCE(submitted_at,clock_timestamp()),broadcast_count=broadcast_count+1,last_broadcast_at=clock_timestamp(),last_status_checked_at=clock_timestamp(),error_detail='broadcast_intent_persisted',updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp()`, lease.Submission.ID, lease.Owner, lease.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// InflightCount is the number of this cluster's same-mint routes signed or
// sent and not yet terminal.
func (s *Store) InflightCount(ctx context.Context, cluster string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE cluster=$1 AND movement_leg='route' AND submission_state NOT IN ('reconciled','expired','failed')`, cluster).Scan(&n)
	return n, err
}

// RenewClaimLease extends this owner's confirmation lease without changing
// its fencing token, so RPC work still in flight does not run on a lapsed
// lease. Zero rows mean the lease was lost; the caller must re-claim.
func (s *Store) RenewClaimLease(ctx context.Context, lease SubmissionLease, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, errors.New("lease renewal requires a positive ttl")
	}
	var deadline time.Time
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
UPDATE loyal_yield.signed_route_submissions
SET confirmation_lease_expires_at = clock_timestamp() + $4::interval,
    updated_at = now()
WHERE id = $1
  AND confirmation_lease_owner = $2
  AND confirmation_fencing_token = $3
  AND confirmation_lease_expires_at > clock_timestamp()
RETURNING confirmation_lease_expires_at`,
			lease.Submission.ID, lease.Owner, lease.FencingToken, formatInterval(ttl)).Scan(&deadline)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		lease.ExpiresAt = deadline
		return renewConflictSet(ctx, tx, []SubmissionLease{lease})
	})
	return deadline, err
}

// SubmissionLease is one claimed confirmation/reconciliation work item. The
// fencing token increments on every claim; advances carry it so a delayed
// worker can never overwrite newer progress.
type SubmissionLease struct {
	Submission   SubmissionRecord
	Owner        string
	FencingToken int64
	ExpiresAt    time.Time
}

// SubmissionRecord is the execution-owned projection of a signed route row.
type SubmissionRecord struct {
	ID                   int64
	Cluster              string
	SemanticKey          string
	OpportunityID        int64
	DecisionID           *int64
	Signature            string
	MessageHash          string
	RecentBlockhash      string
	LastValidBlockHeight int64
	FeePayer             string
	MovementLeg          string
	LegPurpose           string
	LegGeneration        int64
	// SignedTransaction is the exact immutable bytes this record persisted;
	// broadcast reuses them and nothing else ever reconstructs a wire.
	SignedTransaction         []byte
	State                     SubmissionState
	BroadcastCount            int
	SubmittedSlot             *int64
	ConfirmedSlot             *int64
	EffectCheckSlot           *int64
	ExpiryObservedBlockHeight *int64
	ExpectedEffect            json.RawMessage
	ExpectedBalanceAnchors    json.RawMessage
	ConflictAccountKeys       []string
}

// ClaimRecoveryWork claims up to limit same-mint submissions the family owns.
// It writes the Rust confirmation-lease columns, so a restarted Rust confirmer
// skips rows Go is landing and takes them back once the lease lapses.
func (s *Store) ClaimRecoveryWork(ctx context.Context, cluster, owner string, ttl time.Duration, limit int) ([]SubmissionLease, error) {
	if cluster == "" || owner == "" || ttl <= 0 || limit <= 0 {
		return nil, errors.New("incomplete claim request")
	}
	leases := []SubmissionLease{}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
WITH candidates AS (
  SELECT id
  FROM loyal_yield.signed_route_submissions
  WHERE cluster=$1
    AND movement_leg='route'
    AND EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.id=signed_route_submissions.opportunity_id
      WHERE d.id=signed_route_submissions.decision_id AND d.movement_route='same_mint'
       AND o.execution_plan->>'route_kind'='same_mint'
       AND o.execution_plan->>'source_kind' IN ('reserve_position','idle_vault_usdc'))
    AND submission_state IN ('signed','submitted','confirmed',
        'reconciliation_pending','expiry_check_pending')
    AND (confirmation_lease_owner IS NULL OR confirmation_lease_expires_at < clock_timestamp())
    AND confirmation_available_at <= clock_timestamp()
  ORDER BY CASE submission_state
      WHEN 'expiry_check_pending' THEN 1
      WHEN 'submitted' THEN 2
      WHEN 'confirmed' THEN 3
      WHEN 'reconciliation_pending' THEN 4
      WHEN 'signed' THEN 5
      ELSE 6
    END, confirmation_available_at, id
  LIMIT $2
  FOR UPDATE SKIP LOCKED
)
UPDATE loyal_yield.signed_route_submissions submission
SET confirmation_lease_owner=$3,
    confirmation_lease_expires_at=clock_timestamp()+$4::interval,
    confirmation_fencing_token=confirmation_fencing_token+1,
    confirmation_attempt_count=confirmation_attempt_count+1
FROM candidates
WHERE submission.id=candidates.id
RETURNING submission.id, submission.cluster, submission.semantic_key,
    submission.opportunity_id, submission.decision_id, submission.transaction_signature,
    submission.message_hash, submission.recent_blockhash, submission.last_valid_block_height,
    submission.fee_payer, submission.movement_leg, submission.leg_purpose,
    submission.leg_generation, submission.signed_transaction,
    submission.submission_state, submission.broadcast_count,
    submission.submitted_slot, submission.confirmed_slot, submission.effect_check_slot,
    submission.expiry_observed_block_height, submission.expected_effect,
    submission.expected_balance_anchors, submission.conflict_account_keys, submission.confirmation_fencing_token,
    submission.confirmation_lease_expires_at`, cluster, limit, owner, formatInterval(ttl))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			lease, scanErr := scanSubmissionLease(rows, owner)
			if scanErr != nil {
				return scanErr
			}
			leases = append(leases, lease)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return renewConflictSet(ctx, tx, leases)
	})
	if err != nil {
		return nil, err
	}
	return leases, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSubmissionLease(rows rowScanner, owner string) (SubmissionLease, error) {
	var lease SubmissionLease
	lease.Owner = owner
	record := &lease.Submission
	err := rows.Scan(&record.ID, &record.Cluster, &record.SemanticKey, &record.OpportunityID,
		&record.DecisionID, &record.Signature, &record.MessageHash, &record.RecentBlockhash,
		&record.LastValidBlockHeight, &record.FeePayer, &record.MovementLeg, &record.LegPurpose,
		&record.LegGeneration, &record.SignedTransaction, &record.State, &record.BroadcastCount, &record.SubmittedSlot,
		&record.ConfirmedSlot, &record.EffectCheckSlot, &record.ExpiryObservedBlockHeight,
		&record.ExpectedEffect, &record.ExpectedBalanceAnchors, &record.ConflictAccountKeys, &lease.FencingToken, &lease.ExpiresAt)
	return lease, err
}

// Advance is a fenced durable transition for one claimed submission.
type Advance struct {
	NextState                 SubmissionState
	ConfirmedSlot             *int64
	EffectCheckSlot           *int64
	ExpiryObservedBlockHeight *int64
	ErrorDetail               *string
}

// AdvanceSubmission applies one fenced transition. Zero affected rows mean the
// caller lost the lease or its fencing token is stale; the durable state is
// authoritative and the caller must re-claim.
func (s *Store) AdvanceSubmission(ctx context.Context, lease SubmissionLease, advance Advance) error {
	if !legalTransition(lease.Submission.State, advance.NextState) {
		return fmt.Errorf("%w: %s -> %s", ErrNotClaimable, lease.Submission.State, advance.NextState)
	}
	if lease.Submission.MovementLeg != LegRoute {
		return errors.New("generic advancement cannot own cross-mint custody")
	}
	if advance.NextState == StateReconciled {
		return errors.New("reconciliation requires fenced family post-state publication")
	}
	if advance.NextState == StateExpired && (advance.ExpiryObservedBlockHeight == nil || *advance.ExpiryObservedBlockHeight <= lease.Submission.LastValidBlockHeight) {
		return errors.New("expiry requires a finalized height beyond the signed blockhash")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions
SET submission_state=$3,
    confirmed_slot=COALESCE($4, confirmed_slot),
    effect_check_slot=COALESCE($5, effect_check_slot),
    expiry_observed_block_height=COALESCE($6, expiry_observed_block_height),
    error_detail=COALESCE($7, error_detail),
    last_status_checked_at=clock_timestamp(),
    confirmation_lease_owner=NULL,
    confirmation_lease_expires_at=NULL,
    updated_at=clock_timestamp()
WHERE id=$1
  AND confirmation_lease_owner=$2
  AND confirmation_fencing_token=$8
  AND confirmation_lease_expires_at > clock_timestamp()
  AND submission_state=$9
  AND movement_leg='route'
  AND ($3 <> 'expired' OR last_valid_block_height < $6)`,
		lease.Submission.ID, lease.Owner, string(advance.NextState),
		advance.ConfirmedSlot, advance.EffectCheckSlot, advance.ExpiryObservedBlockHeight,
		advance.ErrorDetail, lease.FencingToken, string(lease.Submission.State))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleOwner
	}
	return nil
}

// ReleaseTelemetryReflectedCapacity moves awaiting_telemetry reservations to
// released only when the target frontier observation is strictly newer than
// the movement slot. Equal slots stay reserved: without a transaction index
// the ordering is ambiguous.
func (s *Store) ReleaseTelemetryReflectedCapacity(ctx context.Context, cluster string) (int64, error) {
	if cluster == "" {
		return 0, errors.New("cluster is required")
	}
	command, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.target_capacity_reservations reservation
SET reservation_state='released',
    released_at=now(),
    release_reason='target_telemetry_reflected_movement',
    state_version=reservation.state_version+1,
    updated_at=now()
FROM loyal_yield.target_capacity_frontiers frontier
WHERE reservation.cluster=frontier.cluster
  AND reservation.target_reserve=frontier.target_reserve
  AND reservation.liquidity_mint=frontier.liquidity_mint
  AND reservation.cluster=$1
  AND reservation.reservation_state='awaiting_telemetry'
  AND reservation.movement_slot IS NOT NULL
  AND frontier.observed_slot>reservation.movement_slot`, cluster)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

// ReleasePreparedTransactionALTLeases releases the reusable-lookup-table usage
// leases this submission held, exactly as the legacy terminal trigger does for
// its own writers.
func (s *Store) ReleasePreparedTransactionALTLeases(ctx context.Context, semanticKey string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.lookup_table_usage_leases
SET released_at=COALESCE(released_at, now()), updated_at=now()
WHERE lease_kind='prepared_transaction' AND reference_key=$1 AND released_at IS NULL`, semanticKey)
	return err
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// isUniqueViolation reports a Postgres unique constraint conflict, the same
// contended-admission signal the legacy fence error codes classify.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func formatInterval(d time.Duration) string {
	return fmt.Sprintf("%.3f seconds", d.Seconds())
}

// Confirmation ownership retains the same account-conflict rows as Rust.
// A missing/replaced retained row aborts the entire claim, not just renewal.
func renewConflictSet(ctx context.Context, tx pgx.Tx, leases []SubmissionLease) error {
	if len(leases) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(leases))
	expected := int64(0)
	deadline := leases[0].ExpiresAt
	for _, lease := range leases {
		ids = append(ids, lease.Submission.ID)
		if lease.ExpiresAt.After(deadline) {
			deadline = lease.ExpiresAt
		}
		for _, key := range lease.Submission.ConflictAccountKeys {
			released := (lease.Submission.State == StateReconciliationPending || lease.Submission.State == StateEffectAmbiguous) && (strings.HasPrefix(key, "fleet-shared-write-lane:") || lease.Submission.State == StateReconciliationPending && strings.HasPrefix(key, "policy-setup-funding:"))
			if !released {
				expected++
			}
		}
	}
	command, err := tx.Exec(ctx, `WITH locked AS (
 SELECT cluster,writable_account_key FROM loyal_yield.route_account_conflict_leases
 WHERE submission_id=ANY($1::bigint[]) ORDER BY cluster,writable_account_key FOR UPDATE
 ) UPDATE loyal_yield.route_account_conflict_leases c
 SET expires_at=GREATEST(c.expires_at,$2::timestamptz+interval '2 minutes'),updated_at=clock_timestamp()
 FROM locked WHERE c.cluster=locked.cluster AND c.writable_account_key=locked.writable_account_key`, ids, deadline)
	if err != nil {
		return err
	}
	if command.RowsAffected() != expected {
		return fmt.Errorf("%w: retained conflict set differs from signed submission", ErrConflictLeaseHeld)
	}
	return nil
}
