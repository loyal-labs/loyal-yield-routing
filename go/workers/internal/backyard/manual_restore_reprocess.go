package backyard

// Scoped operator seam for one class of already-manual failures: a landed,
// failed wire that a previous binary latched into manual recovery with the
// unclassified marker (confirmed_transaction_error) because its action or
// error code was not on a list, which capital-stops the whole route through
// UnresolvedCapitalRecoverySQL. Reprocessing runs the SAME finalized failure
// settlement as the automatic path (settleFinalizedFailure): the receipt must
// prove the exact persisted wire rolled back, and the single guarded UPDATE
// moves manual_recovery -> failed. There is no SQL reset and no re-send;
// every other manual recovery row is untouched — the worker's
// UnresolvedCapitalRecoverySQL continues to block them for new capital.
//
// Operator output never carries raw database, RPC, or provider error text:
// every failure surfaces as a fixed stage code alongside the safe result.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type ManualRestoreReprocessRequest struct {
	OperationID string
	Signature   string
}

// ManualRestoreFailureResult is the fixed, sanitized outcome. It carries
// journal identity and settlement facts only — never wire bytes, RPC URLs,
// provider responses, or infra error text.
type ManualRestoreFailureResult struct {
	Schema         string `json:"schema"`
	OperationID    string `json:"operationId"`
	RouteKey       string `json:"routeKey"`
	Action         string `json:"action"`
	Status         string `json:"status"`
	RecoveryReason string `json:"recoveryReason"`
	Signature      string `json:"signature"`
	Classification string `json:"classification"`
	LandingSlot    int64  `json:"landingSlot"`
	Stage          string `json:"stage,omitempty"`
	Executable     bool   `json:"executable"`
	Executed       bool   `json:"executed"`
	TerminalStatus string `json:"terminalStatus,omitempty"`
}

const manualRestoreNoNonterminalSQL = `SELECT EXISTS (
 SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status IN (` + nonterminalStatusSQL + `))`

func sanitizedStage(stage string) error {
	return errors.New("settle-manual-restore blocked at stage " + stage)
}

// loadManualRestoreOperation reads exactly one manual_recovery row by ID, with
// its recovery reason and message hash, and restores its decision through the
// same persisted-decision validation every recovery path uses.
func (d *Database) loadManualRestoreOperation(ctx context.Context, operationID string) (PersistedOperation, string, string, error) {
	if d == nil || d.pool == nil || operationID == "" {
		return PersistedOperation{}, "", "", fmt.Errorf("database is not configured")
	}
	row := d.pool.QueryRow(ctx, `SELECT operation_id, route_key, cycle, action, status, idempotency_key, COALESCE(strategy_key, ''),
		expected_effects, COALESCE(signed_wire, ''::bytea), COALESCE(signed_wire_sha256, ''), COALESCE(transaction_signature, ''),
		COALESCE(recent_blockhash, ''), COALESCE(last_valid_block_height, 0),
		broadcast_intent_at IS NOT NULL, COALESCE(confirmed_slot, 0), COALESCE(recovery_reason, ''), COALESCE(message_sha256, '')
		FROM loyal_yield.multiply_operations
		WHERE operation_id = $1 AND status = 'manual_recovery'`, operationID)
	var operation PersistedOperation
	var action, idempotencyKey, strategyKey, recoveryReason, messageHash string
	if err := row.Scan(
		&operation.ID, &operation.RouteKey, &operation.Cycle, &action, &operation.Status,
		&idempotencyKey, &strategyKey, &operation.ExpectedEffects, &operation.SignedWire, &operation.SignedWireSHA256,
		&operation.TransactionSignature, &operation.RecentBlockhash,
		&operation.LastValidBlockHeight, &operation.BroadcastIntentRecorded,
		&operation.ConfirmedSlot, &recoveryReason, &messageHash,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PersistedOperation{}, "", "", fmt.Errorf("operation is not in manual_recovery")
		}
		return PersistedOperation{}, "", "", fmt.Errorf("load manual restore operation: %w", err)
	}
	decision, err := restorePersistedDecision(operation.ExpectedEffects, Action(action), idempotencyKey, strategyKey)
	if err != nil {
		return PersistedOperation{}, "", "", fmt.Errorf("loaded manual decision is invalid: %w", err)
	}
	operation.Decision = decision
	return operation, recoveryReason, messageHash, nil
}

func RunManualRestoreReprocess(ctx context.Context, databaseURL, rpcURL string, request ManualRestoreReprocessRequest, execute bool) (ManualRestoreFailureResult, error) {
	result := ManualRestoreFailureResult{Schema: "backyard-manual-restore-reprocess/v1"}
	if databaseURL == "" || rpcURL == "" {
		return result, errors.New("settle-manual-restore: database and RPC URLs are required")
	}
	if request.OperationID == "" || request.Signature == "" {
		return result, errors.New("settle-manual-restore: --operation and --signature are required")
	}
	open := openOperatorDatabase
	if execute {
		open = openOperatorWriter
	}
	db, err := open(ctx, databaseURL)
	if err != nil {
		result.Stage = "open_database"
		if errors.Is(err, engine.ErrFamilyHeld) {
			result.Stage = "writer_lock"
		}
		return result, sanitizedStage(result.Stage)
	}
	defer db.Close()
	rpc, err := chain.New(rpcURL, 15*time.Second)
	if err != nil {
		result.Stage = "open_rpc"
		return result, sanitizedStage(result.Stage)
	}
	operation, recoveryReason, messageHash, err := db.loadManualRestoreOperation(ctx, request.OperationID)
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
	if recoveryReason != unclassifiedTransactionErrReason || operation.TransactionSignature != request.Signature {
		result.Stage = "row_shape"
		return result, errors.New("settle-manual-restore: operation does not match the reprocessable landed failure shape")
	}
	receipt, err := readFailureReceipt(ctx, rpc, operation.TransactionSignature)
	if err != nil {
		result.Stage = "read_receipt"
		return result, sanitizedStage(result.Stage)
	}
	reason := confirmedFailureReason(receipt.Meta.Err, receipt.Meta.LogMessages)
	result.Classification = reason
	result.LandingSlot = receipt.Slot
	if validateFinalizedFailureReceipt(receipt, operation.SignedWire, operation.TransactionSignature, operation.SignedWireSHA256, messageHash, mustKey(bridgeDelegate)) != nil {
		result.Stage = "unproven_finalized_failure_receipt"
		return result, sanitizedStage(result.Stage)
	}
	result.Executable = true
	if !execute {
		return result, nil
	}
	settleErr := func() error {
		var blocked bool
		if err := db.pool.QueryRow(ctx, manualRestoreNoNonterminalSQL, operation.RouteKey).Scan(&blocked); err != nil {
			return fmt.Errorf("read active operations: %w", err)
		}
		if blocked {
			return errors.New("route still has a nonterminal operation")
		}
		settled, err := db.settleFinalizedFailure(ctx, operation, receipt, reason, mustKey(bridgeDelegate))
		if err == nil && !settled {
			err = budgetHold("unproven_finalized_failure_receipt")
		}
		return err
	}()
	if settleErr != nil {
		result.Stage = "settlement"
		var pgErr *pgconn.PgError
		if errors.As(settleErr, &pgErr) {
			result.Stage = "settlement_sql_" + pgErr.Code + "_" + pgErr.ConstraintName
		}
		var hold *BudgetHold
		if errors.As(settleErr, &hold) {
			result.Stage = holdCause(hold)
		}
		return result, sanitizedStage(result.Stage)
	}
	var terminalStatus string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations
		WHERE operation_id=$1 AND status='failed' AND transaction_signature=$2 AND confirmation_status='finalized'`,
		operation.ID, operation.TransactionSignature).Scan(&terminalStatus); err != nil {
		result.Stage = "verify_failed"
		return result, sanitizedStage(result.Stage)
	}
	result.Executed = true
	result.TerminalStatus = terminalStatus
	return result, nil
}
