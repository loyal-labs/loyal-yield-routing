package autodeposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReconciliationRequest is one claimed high-water reconciliation ask for an
// Autodeposit target. AttemptCount is the claim's retry epoch: the consumer
// widens its backoff from it.
type ReconciliationRequest struct {
	TargetID      int64
	RequestedSlot int64
	AttemptCount  int
}

// EnqueueAutodepositReconciliationRequest records one bounded high-water
// reconciliation ask per target. A newer requested slot raises the high-water
// mark and pulls the next attempt forward; an older one is coalesced into the
// existing row, so a burst of wallet updates cannot multiply the work.
func (s *Store) EnqueueAutodepositReconciliationRequest(ctx context.Context, targetID, requestedSlot int64) (bool, error) {
	if requestedSlot < 0 {
		return false, fmt.Errorf("autodeposit reconciliation requested slot %d is negative", requestedSlot)
	}
	tag, err := s.pool.Exec(ctx, `
INSERT INTO loyal_yield.autodeposit_reconciliation_requests
    (target_id, requested_slot)
VALUES ($1, $2)
ON CONFLICT (target_id) DO UPDATE SET
    requested_slot = EXCLUDED.requested_slot,
    next_attempt_at = LEAST(
        loyal_yield.autodeposit_reconciliation_requests.next_attempt_at,
        NOW()
    ),
    updated_at = NOW()
WHERE EXCLUDED.requested_slot
    >= loyal_yield.autodeposit_reconciliation_requests.requested_slot`, targetID, requestedSlot)
	if err != nil {
		return false, fmt.Errorf("enqueue autodeposit reconciliation request: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimAutodepositReconciliationRequest takes exclusive, lease-bounded ownership
// of the oldest ready request, bumping its attempt count. SKIP LOCKED keeps
// concurrent consumers on different rows instead of deadlocking.
func (s *Store) ClaimAutodepositReconciliationRequest(ctx context.Context, claimOwner string, leaseSeconds int64) (*ReconciliationRequest, error) {
	if leaseSeconds <= 0 {
		return nil, errors.New("autodeposit reconciliation lease requires a positive duration")
	}
	rows, err := s.pool.Query(ctx, `
WITH candidate AS (
    SELECT target_id
    FROM loyal_yield.autodeposit_reconciliation_requests
    WHERE processed_slot < requested_slot
      AND next_attempt_at <= NOW()
      AND (claim_expires_at IS NULL OR claim_expires_at <= NOW())
    ORDER BY requested_slot, target_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE loyal_yield.autodeposit_reconciliation_requests request
SET claim_owner = $1,
    claim_expires_at = NOW() + make_interval(secs => $2::double precision),
    attempt_count = attempt_count + 1,
    updated_at = NOW()
FROM candidate
WHERE request.target_id = candidate.target_id
RETURNING request.target_id, request.requested_slot, request.attempt_count`, claimOwner, leaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("claim autodeposit reconciliation request: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var request ReconciliationRequest
	if err := rows.Scan(&request.TargetID, &request.RequestedSlot, &request.AttemptCount); err != nil {
		return nil, fmt.Errorf("scan autodeposit reconciliation request: %w", err)
	}
	return &request, rows.Err()
}

// CompleteAutodepositReconciliationRequest advances the processed high-water
// mark through the observed slot. Both marks move by GREATEST so a request that
// was re-raised while this claim ran keeps its newer ask. It reports whether
// more remains pending beyond what was just processed.
func (s *Store) CompleteAutodepositReconciliationRequest(ctx context.Context, targetID int64, claimOwner string, processedSlot int64) (bool, error) {
	if processedSlot < 0 {
		return false, fmt.Errorf("autodeposit reconciliation processed slot %d is negative", processedSlot)
	}
	var stillPending bool
	err := s.pool.QueryRow(ctx, `
UPDATE loyal_yield.autodeposit_reconciliation_requests
SET requested_slot = GREATEST(requested_slot, $3),
    processed_slot = GREATEST(processed_slot, $3),
    attempt_count = 0,
    claim_owner = NULL,
    claim_expires_at = NULL,
    last_error = NULL,
    updated_at = NOW()
WHERE target_id = $1
  AND claim_owner = $2
  AND claim_expires_at > NOW()
RETURNING processed_slot < requested_slot`, targetID, claimOwner, processedSlot).Scan(&stillPending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("autodeposit reconciliation request %d lost claim before completion", targetID)
	}
	if err != nil {
		return false, fmt.Errorf("complete autodeposit reconciliation request: %w", err)
	}
	return stillPending, nil
}

// RetryAutodepositReconciliationRequest releases a failed claim and schedules
// the next attempt after its backoff, keeping the error as durable evidence.
func (s *Store) RetryAutodepositReconciliationRequest(ctx context.Context, targetID int64, claimOwner, lastError string, retryAfterSeconds int64) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.autodeposit_reconciliation_requests
SET claim_owner = NULL,
    claim_expires_at = NULL,
    next_attempt_at = NOW() + make_interval(secs => $3::double precision),
    last_error = $4,
    updated_at = NOW()
WHERE target_id = $1
  AND claim_owner = $2
  AND claim_expires_at > NOW()`, targetID, claimOwner, retryAfterSeconds, lastError)
	if err != nil {
		return fmt.Errorf("retry autodeposit reconciliation request: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("autodeposit reconciliation request %d lost claim before retry", targetID)
	}
	return nil
}

// AwaitAutodepositSetupReconciliationRequest parks an incomplete user setup
// without burning retry budget: policy discovery can legitimately precede
// recurring-delegation creation, so the request sleeps instead of failing.
func (s *Store) AwaitAutodepositSetupReconciliationRequest(ctx context.Context, targetID int64, claimOwner string, retryAfterSeconds int64) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.autodeposit_reconciliation_requests
SET claim_owner = NULL,
    claim_expires_at = NULL,
    attempt_count = 0,
    next_attempt_at = NOW() + make_interval(secs => $3::double precision),
    last_error = NULL,
    updated_at = NOW()
WHERE target_id = $1
  AND claim_owner = $2
  AND claim_expires_at > NOW()`, targetID, claimOwner, retryAfterSeconds)
	if err != nil {
		return fmt.Errorf("park autodeposit setup reconciliation request: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("autodeposit reconciliation request %d lost claim before parking", targetID)
	}
	return nil
}

// ReconciliationRetryBackoffSeconds widens the consumer's base retry delay
// exponentially with the claimed attempt count, capped at five doublings and
// saturated instead of overflowing like the legacy shifting arithmetic.
func ReconciliationRetryBackoffSeconds(baseRetryAfterSeconds int64, attemptCount int) int64 {
	if baseRetryAfterSeconds <= 0 || attemptCount <= 1 {
		return baseRetryAfterSeconds
	}
	doublings := attemptCount - 1
	if doublings > 5 {
		doublings = 5
	}
	backoff := baseRetryAfterSeconds
	for i := 0; i < doublings; i++ {
		if backoff > (1<<62-1)>>1 {
			return int64(1) << 62
		}
		backoff *= 2
	}
	return backoff
}
