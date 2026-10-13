package backyard

// A Backyard transaction that lands and fails moved nothing: Solana rolls back
// every instruction effect of a failed transaction and debits only the fee
// payer's fee (a Backyard wire carries a recent blockhash, never a durable
// nonce, whose advance would survive the failure).
// The finalized receipt proves that for the exact persisted wire
// (validateFinalizedFailureReceipt), whatever program or code refused it, so a
// proven failure ends `failed` and the next tick plans from a fresh
// observation. The decoded error is only the row's recorded reason.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Adaptor error numbers are copied from
// crates/loyal-voltr-rwa-nav-adaptor/src/error.rs (AdaptorError); they name
// the reason a failed rwa bridge wire records.
const (
	// ReportSlot: the report's observed slot is older than the adaptor's
	// maximum report age at landing, so the report was refused unconsumed.
	adaptorErrorReportSlot uint32 = 9
	// TicketReplay: this slot's report was already consumed, so the refused
	// transaction cannot have armed a second report for the same slot.
	adaptorErrorTicketReplay uint32 = 18
)

// squadsSpendingLimitReason marks a Squads spending-limit refusal: a
// specific HOLD reason in simulation, and the recorded reason of a landed
// refusal.
const squadsSpendingLimitReason = "squads_spending_limit_exceeded"

// adaptorMaxReportAgeSlots is the deployed adaptor config's max report age.
// unclassifiedTransactionErrReason is the reason of a failed transaction no
// program error names, and the manual-recovery marker of a failure whose
// receipt does not prove the rollback.
const (
	adaptorMaxReportAgeSlots         = int64(32)
	unclassifiedTransactionErrReason = "confirmed_transaction_error"
)

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
// simply exhausted - return the empty result, an unattributed failure, never a
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

// confirmedFailureReason is the reason a failed transaction's row records,
// from its RPC error and logs: the rwa adaptor's report refusals and the
// Squads spending limit by name, any other custom error attributed to its
// program as program_error:<program>:<code>, and confirmed_transaction_error
// when no program error is attributable. It labels the failure; whether the
// row settles is the receipt proof's alone.
func confirmedFailureReason(rawErr json.RawMessage, logs []string) string {
	code, ok := decodeInstructionErrorCustom(rawErr)
	program := failingProgramFromLogs(logs)
	switch {
	case !ok || program == "":
		return unclassifiedTransactionErrReason
	case program == fsj27Program && code == adaptorErrorReportSlot:
		return "adaptor_report_slot_refused"
	case program == fsj27Program && code == adaptorErrorTicketReplay:
		return "adaptor_report_ticket_replayed"
	case program == squads.ProgramID.String() && code == squads.ErrSpendingLimitExceeded:
		return squadsSpendingLimitReason
	}
	return "program_error:" + program + ":" + strconv.FormatUint(uint64(code), 10)
}

// recoverConfirmedFailure settles one failed broadcast from its finalized
// receipt, read once. A receipt that proves the exact persisted wire rolled
// back terminates the row in `failed`, so the next tick plans again from
// fresh state and UnresolvedCapitalRecoverySQL never sees it; a receipt that
// does not latches manual recovery. A receipt that is not finalized yet
// leaves the row in its submission state, and the next tick lands it again.
func (d *Database) recoverConfirmedFailure(ctx context.Context, rpc *chain.Client, operation PersistedOperation) error {
	receipt, err := readFailureReceipt(ctx, rpc, operation.TransactionSignature)
	if err != nil {
		return err
	}
	settled, err := d.settleFinalizedFailure(ctx, operation, receipt, confirmedFailureReason(receipt.Meta.Err, receipt.Meta.LogMessages), mustKey(bridgeDelegate))
	if err != nil || settled {
		return err
	}
	return d.MarkManualRecovery(ctx, operation.ID, operation.Status, unclassifiedTransactionErrReason)
}
