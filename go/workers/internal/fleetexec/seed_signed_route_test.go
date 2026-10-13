package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Tests seed signed same-mint routes through the journal's adoption shape:
// the exact wire, its hashes and execution evidence with its conflict leases.

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

func legalLegPurpose(purpose string) bool {
	switch purpose {
	case PurposeOptimizeYield, PurposeRecoverSource, PurposeFallbackTarget:
		return true
	}
	return false
}
