package backyardrwa

// Scoped operator seam for one class of already-manual failures: a
// VOLTR_RESTORE_IDLE wire whose finalization landed past the adaptor's report
// age and was refused with ReportSlot before the Voltr CPI. The automatic walk
// classifies that refusal correctly but the previous binary's action gate
// forced the row into manual recovery, which capital-stops the whole route
// through UnresolvedCapitalRecoverySQL. Reprocessing runs the SAME finalized
// fee settlement as the automatic path (settleFinalizedReportFailure): the
// receipt must prove the exact persisted wire rolled back, the measured fee is
// booked as spent, and the single guarded UPDATE moves manual_recovery ->
// failed. There is no SQL reset, no re-send, and no spent-fee discard; every
// other manual recovery row is untouched — the worker's
// UnresolvedCapitalRecoverySQL continues to block them for new capital.
//
// Operator output never carries raw database, RPC, or provider error text:
// every failure surfaces as a fixed stage code alongside the safe result.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ManualRestoreReprocessRequest pins the exact row: both fields are required
// and both must match the stored journal identity before anything is written.
type ManualRestoreReprocessRequest struct {
	OperationID string
	Signature   string
}

// ManualRestoreFailureResult is the fixed, sanitized outcome. It carries
// journal identity and settlement facts only — never wire bytes, RPC URLs,
// provider responses, or infra error text.
type ManualRestoreFailureResult struct {
	Schema                string `json:"schema"`
	OperationID           string `json:"operationId"`
	RouteKey              string `json:"routeKey"`
	Action                string `json:"action"`
	Status                string `json:"status"`
	RecoveryReason        string `json:"recoveryReason"`
	Signature             string `json:"signature"`
	Classification        string `json:"classification"`
	LandingSlot           int64  `json:"landingSlot"`
	Stage                 string `json:"stage,omitempty"`
	Executable            bool   `json:"executable"`
	Executed              bool   `json:"executed"`
	TerminalStatus        string `json:"terminalStatus,omitempty"`
	BookedFeeMicros       int64  `json:"bookedFeeMicros,omitempty"`
	LeaseReleaseConfirmed bool   `json:"leaseReleaseConfirmed,omitempty"`
}

// manualRestoreNoNonterminalSQL is the only route-activity gate for this fee
// settlement command: the route lease must cover a route with no nonterminal
// operation. Historical manual rows — including previously resolved strategy
// resets — are deliberately not consulted here; they stay untouched, and the
// worker's own UnresolvedCapitalRecoverySQL keeps blocking them for new
// capital decisions.
const manualRestoreNoNonterminalSQL = `SELECT EXISTS (
 SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status IN (` + nonterminalStatusSQL + `))`

// sanitizedStage returns the fixed error an operator sees for a failed stage.
// Underlying database, RPC, and provider errors are intentionally discarded:
// their text can carry credentials or provider response bodies.
func sanitizedStage(stage string) error {
	return errors.New("settle-manual-restore blocked at stage " + stage)
}

// loadManualRestoreOperation reads exactly one manual_recovery row by ID and
// restores its decision through the same persisted-decision validation every
// recovery path uses.
func (d *Database) loadManualRestoreOperation(ctx context.Context, operationID string) (PersistedOperation, string, error) {
	if d == nil || d.pool == nil || operationID == "" {
		return PersistedOperation{}, "", fmt.Errorf("database is not configured")
	}
	row := d.pool.QueryRow(ctx, `SELECT operation_id, route_key, cycle, action, status, idempotency_key, COALESCE(strategy_key, ''),
		expected_effects, COALESCE(signed_wire, ''::bytea), COALESCE(signed_wire_sha256, ''), COALESCE(transaction_signature, ''),
		COALESCE(recent_blockhash, ''), COALESCE(last_valid_block_height, 0),
		broadcast_intent_at IS NOT NULL, COALESCE(confirmed_slot, 0), COALESCE(recovery_reason, '')
		FROM loyal_yield.multiply_operations
		WHERE operation_id = $1 AND status = 'manual_recovery'`, operationID)
	var operation PersistedOperation
	var action, idempotencyKey, strategyKey, recoveryReason string
	if err := row.Scan(
		&operation.ID, &operation.RouteKey, &operation.Cycle, &action, &operation.Status,
		&idempotencyKey, &strategyKey, &operation.ExpectedEffects, &operation.SignedWire, &operation.SignedWireSHA256,
		&operation.TransactionSignature, &operation.RecentBlockhash,
		&operation.LastValidBlockHeight, &operation.BroadcastIntentRecorded,
		&operation.ConfirmedSlot, &recoveryReason,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PersistedOperation{}, "", fmt.Errorf("operation is not in manual_recovery")
		}
		return PersistedOperation{}, "", fmt.Errorf("load manual restore operation: %w", err)
	}
	decision, err := restorePersistedDecision(operation.ExpectedEffects, Action(action), idempotencyKey, strategyKey)
	if err != nil {
		return PersistedOperation{}, "", fmt.Errorf("loaded manual decision is invalid: %w", err)
	}
	operation.Decision = decision
	return operation, recoveryReason, nil
}

// RunManualRestoreReprocess validates, and with execute writes, the exact
// already-manual restore failure. Validation is total before the write: the
// row identity, the stored unclassified marker, and the finalized refusal
// classification gate the dry-run; the execute path additionally acquires the
// route lease, re-checks route activity under it, and runs the same
// settlement the automatic walk uses. The lease is always released once
// acquired, on every post-acquire error path.
func RunManualRestoreReprocess(ctx context.Context, databaseURL, rpcURL string, request ManualRestoreReprocessRequest, execute bool) (ManualRestoreFailureResult, error) {
	result := ManualRestoreFailureResult{Schema: "backyard-manual-restore-reprocess/v1"}
	if databaseURL == "" || rpcURL == "" {
		return result, errors.New("settle-manual-restore: database and RPC URLs are required")
	}
	if request.OperationID == "" || request.Signature == "" {
		return result, errors.New("settle-manual-restore: --operation and --signature are required")
	}
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		result.Stage = "open_database"
		return result, sanitizedStage(result.Stage)
	}
	defer db.Close()
	rpc, err := NewRPCClient(rpcURL)
	if err != nil {
		result.Stage = "open_rpc"
		return result, sanitizedStage(result.Stage)
	}
	operation, recoveryReason, err := db.loadManualRestoreOperation(ctx, request.OperationID)
	if err != nil {
		result.Stage = "load_operation"
		return result, sanitizedStage(result.Stage)
	}
	result.OperationID = operation.ID
	result.RouteKey = operation.RouteKey
	result.Action = string(operation.Decision.Action)
	result.Status = string(operation.Status)
	result.RecoveryReason = recoveryReason
	result.Signature = operation.TransactionSignature
	if operation.Decision.Action != VoltrRestoreIdle || recoveryReason != unclassifiedTransactionErrReason || operation.TransactionSignature != request.Signature {
		result.Stage = "row_shape"
		return result, errors.New("settle-manual-restore: operation does not match the reprocessable restore failure shape")
	}
	// The same refusal classification the automatic walk would have used; the
	// settlement below re-derives it from the finalized receipt and requires
	// the exact match again.
	evidence, err := rpc.FailedTransactionEvidence(ctx, operation.TransactionSignature)
	if err != nil {
		result.Stage = "read_receipt"
		return result, sanitizedStage(result.Stage)
	}
	classification := ClassifyConfirmedReportFailure(evidence.Err, evidence.Logs)
	result.Classification = classification.Reason
	result.LandingSlot = evidence.Slot
	if !classification.Retryable || classification.Reason != adaptorReportSlotRefusedReason {
		result.Stage = "classification"
		return result, errors.New("settle-manual-restore: failure receipt does not classify as " + adaptorReportSlotRefusedReason)
	}
	result.Executable = true
	if !execute {
		return result, nil
	}
	nonce := make([]byte, 8)
	if _, err = rand.Read(nonce); err != nil {
		result.Stage = "lease_owner"
		return result, sanitizedStage(result.Stage)
	}
	if _, err = db.AcquireRouteLease(ctx, operation.RouteKey, "manual-restore-reprocess:"+hex.EncodeToString(nonce), 45*time.Second); err != nil {
		result.Stage = "route_lease"
		return result, sanitizedStage(result.Stage)
	}
	// Every path from here releases the lease exactly once before returning.
	settleErr := func() error {
		var blocked bool
		if err := db.pool.QueryRow(ctx, manualRestoreNoNonterminalSQL, operation.RouteKey).Scan(&blocked); err != nil {
			return fmt.Errorf("read active operations: %w", err)
		}
		if blocked {
			return errors.New("route still has a nonterminal operation")
		}
		return db.settleFinalizedReportFailure(ctx, rpc, operation, adaptorReportSlotRefusedReason, true)
	}()
	releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	released, releaseErr := db.ReleaseRouteLease(releaseCtx)
	cancel()
	result.LeaseReleaseConfirmed = releaseErr == nil && released
	if settleErr != nil {
		result.Stage = "settlement"
		var pgErr *pgconn.PgError
		if errors.As(settleErr, &pgErr) {
			result.Stage = "settlement_sql_" + pgErr.Code + "_" + pgErr.ConstraintName
		}
		// A BudgetHold names the exact existing safety guard that refused
		// settlement; surfacing its Reason alone keeps operator output fixed
		// while still identifying the guard. The row is unchanged and the fee
		// remains reserved; the sanitized stage code is all the operator
		// output carries.
		var hold *BudgetHold
		if errors.As(settleErr, &hold) {
			result.Stage = hold.Reason
		}
		return result, sanitizedStage(result.Stage)
	}
	if releaseErr != nil || !released {
		// The commit IS durable; only the lease release is unconfirmed.
		result.Executed = true
		result.Stage = "lease_release"
		return result, errors.New("settle-manual-restore: settled but lease release unconfirmed")
	}
	var terminalStatus, bookedFee string
	if err := db.pool.QueryRow(ctx, `SELECT status, COALESCE(reconciled_effects->>'bookedFeeMicros','') FROM loyal_yield.multiply_operations
		WHERE operation_id=$1 AND status='failed' AND action=$2 AND transaction_signature=$3 AND confirmation_status='finalized'`,
		operation.ID, string(VoltrRestoreIdle), operation.TransactionSignature).Scan(&terminalStatus, &bookedFee); err != nil {
		result.Stage = "verify_failed"
		return result, sanitizedStage(result.Stage)
	}
	result.Executed = true
	result.TerminalStatus = terminalStatus
	if fee, feeErr := strconv.ParseInt(bookedFee, 10, 64); feeErr == nil {
		result.BookedFeeMicros = fee
	}
	return result, nil
}
