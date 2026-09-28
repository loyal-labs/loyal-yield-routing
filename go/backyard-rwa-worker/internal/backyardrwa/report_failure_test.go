package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func adaptorFailureLogs(program string, code int) []string {
	return []string{
		"Program " + bridgeDelegate + " invoke [1]",
		"Program log: Instruction: StageSquadsToVoltr",
		"Program " + program + " invoke [2]",
		"Program log: AnchorError occurred. Error Code: ReportSlot. Error Number: 0x" +
			strings.ToUpper(strings.TrimPrefix(jsonNumberHex(code), "0x")),
		"Program " + program + " failed: custom program error: 0x" + jsonNumberHex(code),
		"Program " + bridgeDelegate + " failed: custom program error: 0x" + jsonNumberHex(code),
	}
}

func jsonNumberHex(code int) string {
	encoded, err := json.Marshal(code)
	if err != nil {
		return "0"
	}
	var value int
	if json.Unmarshal(encoded, &value) != nil {
		return "0"
	}
	digits := "0123456789abcdef"
	if value == 0 {
		return "0"
	}
	out := ""
	for value > 0 {
		out = string(digits[value%16]) + out
		value /= 16
	}
	return out
}

// Audit U4 / contract P2.4: adaptor errors 9 (ReportSlot) and 18 (TicketReplay)
// are retryable terminal failures, never manual recovery, and a wire whose
// report can only land past observed_slot+28 is refused before broadcast.
func TestAdaptorErrors9And18AreRetryableAndLateSendRefused(t *testing.T) {
	t.Run("classified adaptor report failures are retryable", func(t *testing.T) {
		cases := []struct {
			name      string
			err       string
			logs      []string
			reason    string
			retryable bool
		}{
			{
				name:      "adaptor ReportSlot 9 is retryable",
				err:       `{"InstructionError":[2,{"Custom":9}]}`,
				logs:      adaptorFailureLogs(bridgeAdaptorProgram, 9),
				reason:    "adaptor_report_slot_refused",
				retryable: true,
			},
			{
				name:      "adaptor TicketReplay 18 inside a nested CPI frame is retryable",
				err:       `{"InstructionError":[1,{"InstructionError":[0,{"Custom":18}]}]}`,
				logs:      adaptorFailureLogs(bridgeAdaptorProgram, 18),
				reason:    "adaptor_report_ticket_replayed",
				retryable: true,
			},
			{
				name:   "Voltr 6004 MathOverflow stays a capital stop",
				err:    `{"InstructionError":[0,{"Custom":6004}]}`,
				logs:   adaptorFailureLogs(bridgeVoltrProgram, 6004),
				reason: "confirmed_transaction_error",
			},
			{
				name:   "custom code 9 from another program stays a capital stop",
				err:    `{"InstructionError":[0,{"Custom":9}]}`,
				logs:   adaptorFailureLogs(bridgeTokenProgram, 9),
				reason: "confirmed_transaction_error",
			},
			{
				name:   "custom code 9 without an attributable program stays a capital stop",
				err:    `{"InstructionError":[0,{"Custom":9}]}`,
				logs:   []string{"Program " + bridgeDelegate + " invoke [1]"},
				reason: "confirmed_transaction_error",
			},
			{
				name:   "token-program failure stays a capital stop",
				err:    `{"InstructionError":[1,{"Custom":1}]}`,
				logs:   adaptorFailureLogs(bridgeTokenProgram, 1),
				reason: "confirmed_transaction_error",
			},
			{
				name:   "a non-instruction failure stays a capital stop",
				err:    `"BlockhashNotFound"`,
				logs:   nil,
				reason: "confirmed_transaction_error",
			},
			{
				// Production never classifies a null error: the finalized
				// receipt reader rejects it and the row keeps its ambiguous
				// submission state. This only pins that a null error is never
				// called retryable.
				name:   "a null failure error is never retryable",
				err:    `null`,
				logs:   adaptorFailureLogs(bridgeAdaptorProgram, 9),
				reason: "confirmed_transaction_error",
			},
			{
				// The adaptor succeeded and the outer Voltr frame then failed
				// with an Anchor error whose explicit failed lines were
				// truncated. The error cannot be tied to the adaptor, so the
				// failure stays a capital stop instead of guessing the nearest
				// preceding invoke.
				name: "an adaptor success followed by an outer Anchor error is not retryable",
				err:  `{"InstructionError":[0,{"Custom":9}]}`,
				logs: []string{
					"Program " + bridgeDelegate + " invoke [1]",
					"Program " + bridgeAdaptorProgram + " invoke [2]",
					"Program " + bridgeAdaptorProgram + " success",
					"Program " + bridgeVoltrProgram + " invoke [2]",
					"Program log: AnchorError occurred. Error Code: ReportSlot. Error Number: 0x9",
				},
				reason: "confirmed_transaction_error",
			},
			{
				// Truncated logs that end on an open adaptor frame name no
				// failing frame at all: without an error line the adaptor
				// cannot be blamed, so this stays a capital stop.
				name: "truncated logs on an open adaptor frame are not retryable",
				err:  `{"InstructionError":[0,{"Custom":9}]}`,
				logs: []string{
					"Program " + bridgeVoltrProgram + " invoke [1]",
					"Program " + bridgeAdaptorProgram + " invoke [2]",
					"Log truncated",
				},
				reason: "confirmed_transaction_error",
			},
			{
				// An AnchorError logged inside that still-open adaptor frame is
				// the evidence the truncation left out.
				name: "an AnchorError inside the open adaptor frame is retryable",
				err:  `{"InstructionError":[0,{"Custom":9}]}`,
				logs: []string{
					"Program " + bridgeVoltrProgram + " invoke [1]",
					"Program " + bridgeAdaptorProgram + " invoke [2]",
					"Program log: AnchorError occurred. Error Code: ReportSlot. Error Number: 0x9",
					"Log truncated",
				},
				reason:    "adaptor_report_slot_refused",
				retryable: true,
			},
			{
				// An explicit failed line names the frame without needing an
				// AnchorError line at all.
				name: "an explicit adaptor failed line is retryable",
				err:  `{"InstructionError":[0,{"Custom":9}]}`,
				logs: []string{
					"Program " + bridgeVoltrProgram + " invoke [1]",
					"Program " + bridgeAdaptorProgram + " invoke [2]",
					"Program " + bridgeAdaptorProgram + " failed: custom program error: 0x9",
				},
				reason:    "adaptor_report_slot_refused",
				retryable: true,
			},
			{
				// A frame that already logged success cannot own a later
				// Anchor error either, even as the remaining sibling frame.
				name: "a completed adaptor frame is not attributed a later Anchor error",
				err:  `{"InstructionError":[0,{"Custom":18}]}`,
				logs: []string{
					"Program " + bridgeDelegate + " invoke [1]",
					"Program " + bridgeAdaptorProgram + " invoke [2]",
					"Program " + bridgeAdaptorProgram + " success",
					"Program log: AnchorError occurred. Error Code: TicketReplay. Error Number: 0x12",
				},
				reason: "confirmed_transaction_error",
			},
		}
		for _, testCase := range cases {
			classification := ClassifyConfirmedReportFailure(json.RawMessage(testCase.err), testCase.logs)
			if classification.Retryable != testCase.retryable || classification.Reason != testCase.reason {
				t.Fatalf("%s: classification = %+v, want retryable=%t reason=%q",
					testCase.name, classification, testCase.retryable, testCase.reason)
			}
			// A retryable failure terminates in `failed`, which neither blocks
			// execution nor occupies the nonterminal slot.
			if testCase.retryable {
				if !CanTransition(BroadcastIntent, Failed) || !CanTransition(Submitted, Failed) || !CanTransition(Signed, Failed) {
					t.Fatalf("%s: terminal failed is unreachable for a retryable adaptor failure", testCase.name)
				}
			}
		}
	})

	t.Run("a locally expired report is retryable regardless of the decoded error", func(t *testing.T) {
		if !ReportExpiredAtLanding(100, 133) {
			t.Fatal("a landing slot past observed+32 was not recognized as expiry")
		}
		if ReportExpiredAtLanding(100, 132) {
			t.Fatal("a landing slot at observed+32 was treated as expired")
		}
		if ReportExpiredAtLanding(0, 1_000) {
			t.Fatal("a wire without a report was treated as expirable")
		}
	})

	t.Run("the send fence refuses a wire past observed_slot+28", func(t *testing.T) {
		if reportFreshnessMarginSlots != 4 || adaptorMaxReportAgeSlots != 32 || reportSendFreshnessLimitSlots != 28 {
			t.Fatalf("fence constants drifted: margin=%d max=%d limit=%d",
				reportFreshnessMarginSlots, adaptorMaxReportAgeSlots, reportSendFreshnessLimitSlots)
		}
		if stale, reason := EvaluateReportSendFreshness(100, 128); stale || reason != "" {
			t.Fatalf("a wire at observed+28 was refused: %t %q", stale, reason)
		}
		stale, reason := EvaluateReportSendFreshness(100, 129)
		if !stale || reason != "report_stale" {
			t.Fatalf("a wire at observed+29 was not refused: %t %q", stale, reason)
		}
		if stale, _ = EvaluateReportSendFreshness(0, 1_000_000); stale {
			t.Fatal("a wire without a report was refused by the freshness fence")
		}
	})

	t.Run("the failure receipt is read from the chain and classified", func(t *testing.T) {
		evidence := `{"jsonrpc":"2.0","id":1,"result":{"slot":500,"meta":{"err":{"InstructionError":[0,{"Custom":9}]},"logMessages":` +
			mustJSONLogs(t, adaptorFailureLogs(bridgeAdaptorProgram, 9)) + `}}}`
		rpc, err := NewRPCClient("https://rpc.invalid")
		if err != nil {
			t.Fatal(err)
		}
		rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(evidence), nil
		})
		receipt, err := rpc.FailedTransactionEvidence(context.Background(), "5ignature")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Slot != 500 {
			t.Fatalf("failure receipt slot = %d, want 500", receipt.Slot)
		}
		classification := ClassifyConfirmedReportFailure(receipt.Err, receipt.Logs)
		if !classification.Retryable || classification.Reason != "adaptor_report_slot_refused" {
			t.Fatalf("classified receipt = %+v, want retryable adaptor_report_slot_refused", classification)
		}
		// The persisted classification runs without touching the journal when
		// the decoded error already proves the report was refused unconsumed.
		var database *Database
		classified, ok := database.classifyPersistedFailure(context.Background(), rpc, PersistedOperation{
			Operation:            Operation{Decision: Decision{Action: VoltrAllocateToSquads}},
			Status:               Submitted,
			TransactionSignature: "5ignature",
		})
		if !ok || !classified.Retryable || classified.Reason != "adaptor_report_slot_refused" {
			t.Fatalf("persisted classification = %+v ok=%t, want retryable adaptor_report_slot_refused", classified, ok)
		}
	})

	t.Run("the terminal retryable state never enters the capital recovery blocker", func(t *testing.T) {
		blocker := normalizeSQL(UnresolvedCapitalRecoverySQL)
		if !strings.Contains(blocker, "status = 'manual_recovery'") {
			t.Fatal("capital recovery blocker lost its manual recovery predicate")
		}
		for _, status := range []string{"'failed'", "status = 'failed'"} {
			if strings.Contains(blocker, status) {
				t.Fatalf("retryable terminal failures were made a capital execution blocker via %s", status)
			}
		}
		epoch := normalizeSQL(LatestDecisionEpochSQL)
		if !strings.Contains(epoch, "'reconciled','failed'") {
			t.Fatal("a terminal failed row does not advance the durable decision epoch, so the retry would dedupe")
		}
	})
}

// Review fix: a transaction error is only a failure once it settles. A
// processed-only error can be forked away, so it must stay an observation and
// never drive a terminal journal transition.
func TestSignatureStatusFailureRequiresSettlement(t *testing.T) {
	cases := []struct {
		name                          string
		value                         string
		found, settled, processedOnly bool
		confirmed, finalized, failed  bool
	}{
		{
			name:  "a processed-only failure is not a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"processed"}`,
			found: true, processedOnly: true,
		},
		{
			name:  "a finalized failure is a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"}`,
			found: true, settled: true, finalized: true, failed: true,
		},
		{
			name:  "a confirmed failure is a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"confirmed"}`,
			found: true, settled: true, failed: true,
		},
		{
			name:  "a confirmed success is confirmed",
			value: `{"slot":45,"err":null,"confirmationStatus":"confirmed"}`,
			found: true, settled: true, confirmed: true,
		},
		{
			name:  "a processed success is only an observation",
			value: `{"slot":45,"err":null,"confirmationStatus":"processed"}`,
			found: true, processedOnly: true,
		},
	}
	for _, testCase := range cases {
		rpc, err := NewRPCClient("https://rpc.invalid")
		if err != nil {
			t.Fatal(err)
		}
		rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[` + testCase.value + `]}}`), nil
		})
		status, err := rpc.SignatureStatus(context.Background(), "5ignature")
		if err != nil {
			t.Fatal(err)
		}
		if status.Found != testCase.found || status.Settled != testCase.settled || status.ProcessedOnly != testCase.processedOnly ||
			status.Confirmed != testCase.confirmed || status.Finalized != testCase.finalized || status.Failed != testCase.failed {
			t.Fatalf("%s: observation = %+v", testCase.name, status)
		}
	}
}

// Review fix: an unreadable receipt is not evidence. The row keeps its
// ambiguous submission state so later ticks can read the receipt again, and
// only the bounded receipt retry window terminates it - never manual recovery.
func TestUnreadableFailureReceiptKeepsObservingUntilBounded(t *testing.T) {
	now := time.Now()
	if receiptRetryExpired(time.Time{}, now) {
		t.Fatal("a missing broadcast timestamp expired the receipt retry window")
	}
	if receiptRetryExpired(now.Add(-failureReceiptRetryWindow), now) {
		t.Fatal("the receipt retry window expired at its boundary")
	}
	if !receiptRetryExpired(now.Add(-failureReceiptRetryWindow-time.Second), now) {
		t.Fatal("an exhausted receipt retry window never expired")
	}

	rpc, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failure receipt pruned")
	})
	var database *Database
	operation := PersistedOperation{
		Operation: Operation{ID: "unreadable"}, Status: Submitted, TransactionSignature: "5ignature",
	}
	classification, terminal := database.classifyPersistedFailure(context.Background(), rpc, operation)
	if terminal || !classification.ReceiptUnavailable || classification.Retryable ||
		classification.Reason != failureReceiptUnavailableReason {
		t.Fatalf("unreadable receipt classification = %+v terminal=%t", classification, terminal)
	}
	if err := database.recoverConfirmedFailure(context.Background(), rpc, operation); err != nil {
		t.Fatalf("an unreadable receipt entered manual recovery: %v", err)
	}
}

// Review fix: the R03 batch reader must apply the same settled invariant as
// the singular reader - a processed-only error is an observation, not a
// failure.
func TestBatchSignatureStatusesRequireSettlement(t *testing.T) {
	rpc, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[` +
			`{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"processed"},` +
			`{"slot":46,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"},` +
			`null]}}`), nil
	})
	statuses, err := rpc.SignatureStatuses(context.Background(), []string{"processed-only", "finalized", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if !statuses[0].Found || statuses[0].Failed || statuses[0].Settled || statuses[0].Confirmed || !statuses[0].ProcessedOnly {
		t.Fatalf("a processed-only error was treated as a failure: %+v", statuses[0])
	}
	if !statuses[1].Found || !statuses[1].Failed || !statuses[1].Settled || statuses[1].ProcessedOnly {
		t.Fatalf("a settled error lost its failure: %+v", statuses[1])
	}
	if statuses[2].Found {
		t.Fatalf("an absent signature was reported found: %+v", statuses[2])
	}
}

func mustJSONLogs(t *testing.T, logs []string) string {
	t.Helper()
	encoded, err := json.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
