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

const (
	adaptorErrorReportSlot   uint32 = 9
	adaptorErrorTicketReplay uint32 = 18
)

const squadsSpendingLimitReason = "squads_spending_limit_exceeded"

const adaptorMaxReportAgeSlots = int64(32)

const (
	unclassifiedTransactionErrReason = "confirmed_transaction_error"
	unprovenRollbackRecoveryReason   = "confirmed_transaction_error"
)

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

func programSuccessLogLine(line string) (string, bool) {
	prefix, ok := strings.CutPrefix(line, "Program ")
	if !ok {
		return "", false
	}
	id, rest, found := strings.Cut(prefix, " ")
	return id, found && strings.HasPrefix(rest, "success")
}

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

func (d *Database) recoverConfirmedFailure(ctx context.Context, rpc *chain.Client, operation PersistedOperation) error {
	receipt, err := readFailureReceipt(ctx, rpc, operation.TransactionSignature)
	if err != nil {
		return err
	}
	settled, err := d.settleFinalizedFailure(ctx, operation, receipt, confirmedFailureReason(receipt.Meta.Err, receipt.Meta.LogMessages), mustKey(backyardDelegate))
	if err != nil || settled {
		return err
	}
	return d.MarkManualRecovery(ctx, operation.ID, operation.Status, unprovenRollbackRecoveryReason)
}
