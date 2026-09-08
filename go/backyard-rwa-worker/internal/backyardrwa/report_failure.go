package backyardrwa

// Audit U4 (docs/plans/backyard-rwa-adaptor-strategy2-audit-2026-09-08.md): a
// report whose transaction lands after the adaptor's report age limit fails on
// chain, and a failed Solana transaction moves no funds. Parking that row in
// manual recovery blocked every later execution until an operator edited it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Adaptor error numbers are copied from
// crates/loyal-voltr-rwa-nav-adaptor/src/error.rs (AdaptorError). A renumbering
// there is a protocol change and must be re-reviewed here, not guessed.
const (
	// ReportSlot: the report's observed slot is older than the adaptor's
	// maximum report age at landing, so the report was refused unconsumed.
	adaptorErrorReportSlot uint32 = 9
	// TicketReplay: this slot's report was already consumed, so the refused
	// transaction cannot have armed a second report for the same slot.
	adaptorErrorTicketReplay uint32 = 18
)

// adaptorMaxReportAgeSlots is the deployed adaptor config's max report age.
// reportFreshnessMarginSlots keeps the send fence inside that window: a wire
// refused at observed+28 can never land as an on-chain ReportSlot failure.
const (
	adaptorMaxReportAgeSlots      = int64(32)
	reportFreshnessMarginSlots    = int64(4)
	reportSendFreshnessLimitSlots = adaptorMaxReportAgeSlots - reportFreshnessMarginSlots
	// failureReceiptRetryWindow bounds how long a settled failure keeps its
	// ambiguous submission state while its receipt is unreadable. A lagging or
	// pruned RPC recovers in seconds; past this window the wire's blockhash is
	// long expired, so the row terminates durably in `failed` without claiming
	// that capital moved.
	failureReceiptRetryWindow        = 15 * time.Minute
	failureReceiptUnavailableReason  = "failure_receipt_unavailable"
	unclassifiedTransactionErrReason = "confirmed_transaction_error"
)

// ConfirmedFailureClassification is the recovery outcome of one failed
// on-chain transaction. Retryable failures terminate the row in `failed`, so
// LatestDecisionEpochSQL advances the durable epoch and the next tick takes a
// fresh observation; capital stops keep manual recovery.
type ConfirmedFailureClassification struct {
	Retryable bool
	Reason    string
	// ReceiptUnavailable marks a settled failure whose receipt could not be
	// read at all (lagging or pruned RPC, truncated logs). Nothing is proven
	// either way, so the row keeps its ambiguous submission state and the
	// receipt is fetched again on later ticks.
	ReceiptUnavailable bool
}

// ConfirmedFailureEvidence is the immutable failure receipt of one signature.
type ConfirmedFailureEvidence struct {
	Slot int64
	Err  json.RawMessage
	Logs []string
}

// decodeInstructionErrorCustom returns the innermost InstructionError custom
// code. CPI failures nest as another InstructionError frame inside the error
// position, so the walk recurses until a {"Custom": code} leaf or exhaustion.
func decodeInstructionErrorCustom(raw json.RawMessage) (uint32, bool) {
	var frame struct {
		InstructionError *json.RawMessage `json:"InstructionError"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.InstructionError == nil {
		return 0, false
	}
	var pair []json.RawMessage
	if err := json.Unmarshal(*frame.InstructionError, &pair); err != nil || len(pair) != 2 {
		return 0, false
	}
	var leaf struct {
		Custom *uint32 `json:"Custom"`
	}
	if err := json.Unmarshal(pair[1], &leaf); err == nil && leaf.Custom != nil {
		return *leaf.Custom, true
	}
	return decodeInstructionErrorCustom(pair[1])
}

func programIDFromFailedLogLine(line string) (string, bool) {
	prefix, ok := strings.CutPrefix(line, "Program ")
	if !ok {
		return "", false
	}
	id, rest, found := strings.Cut(prefix, " ")
	return id, found && strings.Contains(rest, "failed")
}

// programInvokeDepth parses "Program <id> invoke [depth]".
func programInvokeDepth(line string) (string, int, bool) {
	prefix, ok := strings.CutPrefix(line, "Program ")
	if !ok {
		return "", 0, false
	}
	id, rest, found := strings.Cut(prefix, " ")
	if !found {
		return "", 0, false
	}
	body, ok := strings.CutPrefix(rest, "invoke [")
	if !ok {
		return "", 0, false
	}
	end := strings.Index(body, "]")
	if end <= 0 {
		return "", 0, false
	}
	depth, err := strconv.Atoi(body[:end])
	if err != nil || depth <= 0 {
		return "", 0, false
	}
	return id, depth, true
}

// programSuccessLogLine parses "Program <id> success".
func programSuccessLogLine(line string) (string, bool) {
	prefix, ok := strings.CutPrefix(line, "Program ")
	if !ok {
		return "", false
	}
	id, rest, found := strings.Cut(prefix, " ")
	return id, found && strings.HasPrefix(rest, "success")
}

// failingProgramFromLogs attributes a custom instruction error to the program
// that produced it by rebuilding the CPI call stack from the log lines: every
// "Program <id> invoke [depth]" pushes a frame, "success" pops its frame, and
// an explicit "Program <id> failed:" line names the frame that failed.
//
// An AnchorError without such a line is attributed only when it appears inside
// the innermost frame that is still open at that point. Open frames alone
// prove nothing: logs that end without any error line - truncated, missing, or
// simply exhausted - return the empty result, which is a capital stop, never a
// guess about whichever frame happened to remain open.
func failingProgramFromLogs(logs []string) string {
	var stack []string
	anchorProgram := ""
	anchorAttributed := false
	for _, raw := range logs {
		line := strings.TrimSpace(raw)
		if id, depth, ok := programInvokeDepth(line); ok {
			for len(stack) >= depth {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, id)
			continue
		}
		if id, ok := programIDFromFailedLogLine(line); ok {
			return id
		}
		if strings.Contains(line, "AnchorError") {
			if len(stack) > 0 {
				anchorProgram, anchorAttributed = stack[len(stack)-1], true
			}
			continue
		}
		if id, ok := programSuccessLogLine(line); ok {
			for index := len(stack) - 1; index >= 0; index-- {
				if stack[index] == id {
					stack = append(stack[:index], stack[index+1:]...)
					break
				}
			}
		}
	}
	if !anchorAttributed {
		return ""
	}
	return anchorProgram
}

// ClassifyConfirmedReportFailure maps one failed transaction's RPC error and
// logs to its recovery outcome. Only a custom error from the pinned adaptor
// program that proves the report was refused unconsumed (ReportSlot,
// TicketReplay) is retryable. Everything else - including a Voltr MathOverflow
// or any token-program error - stays a capital stop, because the failure
// receipt alone cannot prove that no instruction side effect settled.
func ClassifyConfirmedReportFailure(rawErr json.RawMessage, logs []string) ConfirmedFailureClassification {
	if rawErr == nil || len(rawErr) == 0 || string(rawErr) == "null" {
		return ConfirmedFailureClassification{Reason: "confirmed_transaction_error"}
	}
	if code, ok := decodeInstructionErrorCustom(rawErr); ok && failingProgramFromLogs(logs) == bridgeAdaptorProgram {
		switch code {
		case adaptorErrorReportSlot:
			return ConfirmedFailureClassification{Retryable: true, Reason: "adaptor_report_slot_refused"}
		case adaptorErrorTicketReplay:
			return ConfirmedFailureClassification{Retryable: true, Reason: "adaptor_report_ticket_replayed"}
		}
	}
	return ConfirmedFailureClassification{Reason: "confirmed_transaction_error"}
}

// ReportExpiredAtLanding reports whether the landing slot is already past the
// adaptor's maximum report age. Any failure of a report-bearing wire at such a
// slot is independent of the decoded error: the report can no longer be armed,
// so the transaction cannot have moved capital.
func ReportExpiredAtLanding(observedSlot, landingSlot int64) bool {
	return observedSlot > 0 && landingSlot > observedSlot+adaptorMaxReportAgeSlots
}

// EvaluateReportSendFreshness is the pure send fence. A wire refused here can
// never land inside the adaptor window, so it must not be broadcast; a fresh
// observation replaces it on the next tick.
func EvaluateReportSendFreshness(observedSlot, confirmedSlot int64) (stale bool, reason string) {
	if observedSlot <= 0 {
		return false, ""
	}
	if confirmedSlot > observedSlot+reportSendFreshnessLimitSlots {
		return true, "report_stale"
	}
	return false, ""
}

// persistedReportObservedSlot reads the wire's own report slot back from the
// persisted build input. Only report-bearing bridge wires carry a report, so
// other actions return zero and are never fenced.
func (d *Database) persistedReportObservedSlot(ctx context.Context, operationID string) (int64, error) {
	if d == nil || d.pool == nil || operationID == "" {
		return 0, fmt.Errorf("report slot database is not configured")
	}
	// The build input's request bytes are stored as a JSONB string (the
	// canonical encoding), so the typed phase3BuildInput decode restores them;
	// selecting the nested `request` object directly would return that string
	// and silently read a zero report slot.
	var encoded []byte
	if err := d.pool.QueryRow(ctx, `SELECT expected_effects->'phase3'->'buildInput' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&encoded); err != nil {
		return 0, fmt.Errorf("read persisted build request: %w", err)
	}
	var input phase3BuildInput
	if len(encoded) == 0 || json.Unmarshal(encoded, &input) != nil {
		return 0, nil
	}
	decoded, _, _, err := input.decode()
	if err != nil {
		return 0, nil
	}
	bridge, ok := decoded.(BridgeBuildRequest)
	if !ok {
		// Only report-bearing bridge wires carry a report slot to fence on.
		return 0, nil
	}
	if bridge.Report.ObservedSlot > uint64(int64(^uint64(0)>>1)) {
		return 0, fmt.Errorf("persisted report slot exceeds signed range")
	}
	return int64(bridge.Report.ObservedSlot), nil
}

// TerminalTransition reports that the durable row already moved to a terminal
// status during this tick. It is a result, not an error: the caller must stop
// advancing the now-obsolete in-memory status instead of treating the row as
// still nonterminal.
type TerminalTransition struct {
	OperationID string
}

// RefuseStaleReportSend is the pre-broadcast fence for a persisted signed
// wire. It runs before broadcast intent is recorded, so a refused wire keeps
// its reservation and terminates in `failed` without ever being submitted.
func (d *Database) RefuseStaleReportSend(ctx context.Context, rpc *RPCClient, operationID string, from OperationStatus) (TerminalTransition, error) {
	if rpc == nil {
		return TerminalTransition{}, fmt.Errorf("RPC client is required")
	}
	observed, err := d.persistedReportObservedSlot(ctx, operationID)
	if err != nil || observed <= 0 {
		return TerminalTransition{}, err
	}
	confirmed, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return TerminalTransition{}, err
	}
	if stale, _ := EvaluateReportSendFreshness(observed, confirmed); !stale {
		return TerminalTransition{}, nil
	}
	if err := d.MarkReportStaleFailed(ctx, operationID, from); err != nil {
		return TerminalTransition{}, err
	}
	return TerminalTransition{OperationID: operationID}, nil
}

// MarkReportStaleFailed terminates a never-broadcast wire whose report can no
// longer land inside the adaptor's age window. Nothing was submitted, so the
// terminal failure releases the one-nonterminal slot and the reservation.
func (d *Database) MarkReportStaleFailed(ctx context.Context, operationID string, from OperationStatus) error {
	if from != Signed && from != Built && from != Simulated {
		return fmt.Errorf("report staleness fence requires a never-broadcast source")
	}
	return d.transition(ctx, operationID, from, Failed, `, recovery_reason = 'report_stale'`)
}

// MarkReportRetryFailed terminates a broadcast transaction whose failure
// receipt proves the report was refused unconsumed and no capital moved.
func (d *Database) MarkReportRetryFailed(ctx context.Context, operationID string, from OperationStatus, reason string) error {
	if reason == "" || (from != BroadcastIntent && from != Submitted) {
		return fmt.Errorf("report retry failure requires an ambiguous submission and explicit reason")
	}
	return d.transition(ctx, operationID, from, Failed, `, recovery_reason = $4`, reason)
}

// broadcastIntentAt reads when the wire's submission was journaled. That
// timestamp is the durable clock for the receipt retry window; a missing one
// never expires the window.
func (d *Database) broadcastIntentAt(ctx context.Context, operationID string) (time.Time, error) {
	if d == nil || d.pool == nil || operationID == "" {
		return time.Time{}, fmt.Errorf("receipt retry database is not configured")
	}
	var sentAt time.Time
	if err := d.pool.QueryRow(ctx, `SELECT broadcast_intent_at FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&sentAt); err != nil {
		return time.Time{}, fmt.Errorf("read broadcast intent time: %w", err)
	}
	return sentAt, nil
}

// receiptRetryExpired is the pure liveness bound for unreadable failure
// receipts: after it, the row must terminate instead of observing forever.
func receiptRetryExpired(sentAt, now time.Time) bool {
	return !sentAt.IsZero() && now.Sub(sentAt) > failureReceiptRetryWindow
}

// classifyPersistedFailure decodes the failure receipt of a broadcast
// signature. A readable receipt yields either a retryable adaptor proof or an
// unclassified capital stop; an unreadable one yields the bounded
// receipt-unavailable outcome instead of an immediate manual recovery.
func (d *Database) classifyPersistedFailure(ctx context.Context, rpc *RPCClient, operation PersistedOperation) (ConfirmedFailureClassification, bool) {
	evidence, err := rpc.FailedTransactionEvidence(ctx, operation.TransactionSignature)
	if err != nil {
		return d.unreadableReceiptOutcome(ctx, operation)
	}
	if classification := ClassifyConfirmedReportFailure(evidence.Err, evidence.Logs); classification.Retryable {
		return classification, true
	}
	observed, err := d.persistedReportObservedSlot(ctx, operation.ID)
	if err == nil && ReportExpiredAtLanding(observed, evidence.Slot) {
		return ConfirmedFailureClassification{Retryable: true, Reason: "report_expired_at_landing"}, true
	}
	return ConfirmedFailureClassification{}, false
}

// unreadableReceiptOutcome keeps a settled-but-unreadable failure in its
// ambiguous submission state, so later ticks fetch the receipt again, and
// terminates it only once the retry window is exhausted.
func (d *Database) unreadableReceiptOutcome(ctx context.Context, operation PersistedOperation) (ConfirmedFailureClassification, bool) {
	classification := ConfirmedFailureClassification{Reason: failureReceiptUnavailableReason, ReceiptUnavailable: true}
	sentAt, err := d.broadcastIntentAt(ctx, operation.ID)
	if err != nil {
		return classification, false
	}
	return classification, receiptRetryExpired(sentAt, time.Now())
}

// recoverConfirmedFailure replaces the unconditional manual-recovery mapping
// for failed broadcasts. Retryable adaptor failures terminate in `failed` so
// the next tick can decide again; UnresolvedCapitalRecoverySQL never sees them.
// Manual recovery is reserved for a readable receipt that proves a
// non-retryable error, never for an RPC that simply could not answer.
func (d *Database) recoverConfirmedFailure(ctx context.Context, rpc *RPCClient, operation PersistedOperation) error {
	classification, terminal := d.classifyPersistedFailure(ctx, rpc, operation)
	if classification.ReceiptUnavailable {
		if !terminal {
			// Nothing is proven either way: keep the row in its ambiguous
			// submission state and retry the receipt on the next tick.
			return nil
		}
		// The wall-time window only triggers this finalized re-read; it never
		// terminates on its own. A merely confirmed failure can still be
		// forked away, and a confirmation that settles as a success must reach
		// the confirmation path instead of a terminal `failed`.
		status, err := rpc.FinalizedSignatureStatus(ctx, operation.TransactionSignature)
		if err != nil {
			return nil
		}
		if status.Failed {
			if !status.Finalized {
				return nil
			}
			return d.MarkReportRetryFailed(ctx, operation.ID, operation.Status, classification.Reason)
		}
		if status.Settled {
			return d.MarkConfirmed(ctx, operation.ID, operation.Status, status.ConfirmationSlot)
		}
		// Not finalized yet, or absent: keep observing.
		return nil
	}
	if !terminal {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, unclassifiedTransactionErrReason)
	}
	return d.MarkReportRetryFailed(ctx, operation.ID, operation.Status, classification.Reason)
}
