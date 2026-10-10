package backyard

// Audit U4 (docs/plans/backyard-rwa-adaptor-strategy2-audit-2026-09-08.md): a
// report whose transaction lands after the adaptor's age limit fails on chain.
// Instruction effects roll back atomically; its network fee still gets paid.
// Exact finalized fee settlement allows the next observation without manual edits.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
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

	// adaptorReportSlotRefusedReason is the classification reason for the
	// ReportSlot refusal. Naming it once keeps the classifier and the two
	// settlement gates from drifting apart.
	adaptorReportSlotRefusedReason = "adaptor_report_slot_refused"

	// reportExpiredInSimulationReason: the adaptor refused an unsent wire in
	// simulation because its report was already past the age limit.
	reportExpiredInSimulationReason = "report_expired_in_simulation"
)

// squadsSpendingLimitReason marks a Squads spending-limit refusal: a
// specific, non-retryable HOLD reason - never a generic simulation failure and
// never manual recovery, because nothing moved and the limit self-heals at its
// next period boundary.
const squadsSpendingLimitReason = "squads_spending_limit_exceeded"

// adaptorMaxReportAgeSlots is the deployed adaptor config's max report age.
const (
	adaptorMaxReportAgeSlots         = int64(32)
	unclassifiedTransactionErrReason = "confirmed_transaction_error"
)

// ConfirmedFailureClassification is the recovery outcome of one failed
// on-chain transaction. Retryable failures terminate the row in `failed`, so
// LatestDecisionEpochSQL advances the durable epoch and the next tick takes a
// fresh observation; capital stops keep manual recovery.
type ConfirmedFailureClassification struct {
	Retryable bool
	Reason    string
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
			return ConfirmedFailureClassification{Retryable: true, Reason: adaptorReportSlotRefusedReason}
		case adaptorErrorTicketReplay:
			return ConfirmedFailureClassification{Retryable: true, Reason: "adaptor_report_ticket_replayed"}
		}
	}
	if squadsSpendingLimitExceeded(rawErr, logs) {
		// The policy refused capital movement, but its network fee still needs
		// exact finalized settlement before a report-only row can terminate.
		// Other action families retain their existing manual recovery hold.
		return ConfirmedFailureClassification{Retryable: true, Reason: squadsSpendingLimitReason}
	}
	return ConfirmedFailureClassification{Reason: "confirmed_transaction_error"}
}

// squadsSpendingLimitExceeded reports whether a failed transaction was
// refused by the pinned Squads program because the policy's embedded
// spending limit is exhausted for its current period. Attribution reuses the
// same log-stack walk as the adaptor errors: an unattributed 6073 stays a
// capital stop.
func squadsSpendingLimitExceeded(rawErr json.RawMessage, logs []string) bool {
	code, ok := decodeInstructionErrorCustom(rawErr)
	return ok && code == squads.ErrSpendingLimitExceeded && failingProgramFromLogs(logs) == squads.ProgramID.String()
}

// recoverConfirmedFailure settles one failed broadcast from its finalized
// receipt, read once. A classified refusal of a report-bearing wire
// terminates in `failed`, so the next tick plans again from fresh state;
// UnresolvedCapitalRecoverySQL never sees it. Every other failure latches
// manual recovery. A receipt that is not finalized yet leaves the row in its
// submission state, and the next tick lands it again.
func (d *Database) recoverConfirmedFailure(ctx context.Context, rpc *chain.Client, operation PersistedOperation) error {
	receipt, err := readFailureReceipt(ctx, rpc, operation.TransactionSignature)
	if err != nil {
		return err
	}
	classification := ClassifyConfirmedReportFailure(receipt.Meta.Err, receipt.Meta.LogMessages)
	if !classification.Retryable || !isSettleableReportFailure(operation.Decision.Action, classification.Reason) {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, unclassifiedTransactionErrReason)
	}
	return d.settleFinalizedReportFailure(ctx, operation, receipt, classification.Reason, false)
}

// isSettleableReportFailure scopes automatic finalized-fee settlement to the
// report-bearing bridge wires whose refusal receipt alone proves nothing was
// consumed and no capital moved: the report-only NAV refresh for every
// retryable adaptor refusal, and — restricted to the adaptor's report-age
// refusal — the VOLTR_RESTORE_IDLE wire that pulls staged custody back. A
// failed restore carries capital in its amount, so only the narrow ReportSlot
// classification (the report was refused before the Voltr CPI) admits it;
// every other action family and classification keeps its manual recovery hold.
func isSettleableReportFailure(action Action, reason string) bool {
	if action == ReportNAV {
		return true
	}
	return action == VoltrRestoreIdle && reason == adaptorReportSlotRefusedReason
}
