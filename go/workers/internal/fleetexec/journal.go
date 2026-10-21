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
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

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

func (s *Store) InflightCount(ctx context.Context, cluster string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE cluster=$1 AND submission_state<>ALL($2)`, cluster, fleet.SubmissionTerminalStates).Scan(&n)
	return n, err
}

func (s *Store) RenewClaimLease(ctx context.Context, lease SubmissionLease, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, errors.New("lease renewal requires a positive ttl")
	}
	var deadline time.Time
	err := s.pool.QueryRow(ctx, `
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
		return time.Time{}, ErrStaleOwner
	}
	return deadline, err
}

type SubmissionLease struct {
	Submission   SubmissionRecord
	Owner        string
	FencingToken int64
	ExpiresAt    time.Time
}

type SubmissionRecord struct {
	ID                        int64
	Cluster                   string
	SemanticKey               string
	OpportunityID             int64
	DecisionID                *int64
	Signature                 string
	MessageHash               string
	RecentBlockhash           string
	LastValidBlockHeight      int64
	FeePayer                  string
	MovementLeg               MovementLeg
	LegPurpose                LegPurpose
	LegGeneration             int64
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
	AttemptCount              int
}

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
       AND o.execution_plan->>'route_kind'='same_mint' AND o.execution_plan->>'source_kind' IN ('reserve_position','idle_vault_usdc'))
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
    submission.confirmation_lease_expires_at, submission.confirmation_attempt_count`, cluster, limit, owner, formatInterval(ttl))
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
		return rows.Err()
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
		&record.ExpectedEffect, &record.ExpectedBalanceAnchors, &record.ConflictAccountKeys, &lease.FencingToken, &lease.ExpiresAt,
		&record.AttemptCount)
	return lease, err
}

type Advance struct {
	NextState                 SubmissionState
	ConfirmedSlot             *int64
	ExpiryObservedBlockHeight *int64
	ErrorDetail               *string
}

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
    expiry_observed_block_height=COALESCE($5, expiry_observed_block_height),
    error_detail=COALESCE($6, error_detail),
    last_status_checked_at=clock_timestamp(),
    confirmation_lease_owner=NULL,
    confirmation_lease_expires_at=NULL,
    updated_at=clock_timestamp()
WHERE id=$1
  AND confirmation_lease_owner=$2
  AND confirmation_fencing_token=$7
  AND confirmation_lease_expires_at > clock_timestamp()
  AND submission_state=$8
  AND movement_leg='route'
  AND ($3 <> 'expired' OR last_valid_block_height < $5)`,
		lease.Submission.ID, lease.Owner, string(advance.NextState),
		advance.ConfirmedSlot, advance.ExpiryObservedBlockHeight,
		advance.ErrorDetail, lease.FencingToken, string(lease.Submission.State))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleOwner
	}
	return nil
}

func (s *Store) releaseNotYet(ctx context.Context, lease SubmissionLease, delay time.Duration) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET confirmation_available_at=clock_timestamp()+$4::interval,confirmation_attempt_count=confirmation_attempt_count-1,confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp()`, lease.Submission.ID, lease.Owner, lease.FencingToken, formatInterval(delay))
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}

const maxErrorDetail = 512

func (s *Store) DeferSubmission(ctx context.Context, lease SubmissionLease, delay time.Duration, detail string) error {
	if delay < 0 || strings.TrimSpace(detail) == "" {
		return errors.New("submission deferral requires a nonnegative delay and an error")
	}
	if len(detail) > maxErrorDetail {
		detail = strings.ToValidUTF8(detail[:maxErrorDetail], "")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions
SET confirmation_available_at=clock_timestamp()+$4::interval,
    confirmation_lease_owner=NULL,
    confirmation_lease_expires_at=NULL,
    last_status_checked_at=clock_timestamp(),
    error_detail=$5,
    updated_at=clock_timestamp()
WHERE id=$1
  AND submission_state IN ('signed','submitted','reconciliation_pending','expiry_check_pending','effect_ambiguous')
  AND confirmation_lease_owner=$2
  AND confirmation_fencing_token=$3
  AND confirmation_lease_expires_at>clock_timestamp()`,
		lease.Submission.ID, lease.Owner, lease.FencingToken, formatInterval(delay), detail)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleOwner
	}
	return nil
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func formatInterval(d time.Duration) string {
	return fmt.Sprintf("%.3f seconds", d.Seconds())
}
