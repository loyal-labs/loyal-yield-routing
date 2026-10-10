package backyard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
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
	t.Parallel()
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
				logs:   adaptorFailureLogs(voltr.ProgramID.String(), 6004),
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
					"Program " + voltr.ProgramID.String() + " invoke [2]",
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
					"Program " + voltr.ProgramID.String() + " invoke [1]",
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
					"Program " + voltr.ProgramID.String() + " invoke [1]",
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
					"Program " + voltr.ProgramID.String() + " invoke [1]",
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

	t.Run("the failure receipt is read from the chain and classified", func(t *testing.T) {
		evidence := `{"jsonrpc":"2.0","id":1,"result":` + transactionResult(t, 500, nil, map[string]any{"err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 9}}},
			"fee": 5000, "preBalances": []uint64{1}, "postBalances": []uint64{1}, "logMessages": adaptorFailureLogs(bridgeAdaptorProgram, 9)}) + `}`
		rpc := newFakeChain(t, nil)
		rpcOf(rpc).Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(evidence), nil
		})
		receipt, err := readFailureReceipt(context.Background(), rpc, testSignature)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Slot != 500 {
			t.Fatalf("failure receipt slot = %d, want 500", receipt.Slot)
		}
		classification := ClassifyConfirmedReportFailure(receipt.Meta.Err, receipt.Meta.LogMessages)
		if !classification.Retryable || classification.Reason != "adaptor_report_slot_refused" {
			t.Fatalf("classified receipt = %+v, want retryable adaptor_report_slot_refused", classification)
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
	t.Parallel()
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
		rpc := newFakeChain(t, nil)
		rpcOf(rpc).Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":50},"value":[` + testCase.value + `]}}`), nil
		})
		status, err := signatureStatus(context.Background(), rpc, testSignature)
		if err != nil {
			t.Fatal(err)
		}
		if status.Found != testCase.found || status.Settled != testCase.settled || status.ProcessedOnly != testCase.processedOnly ||
			status.Confirmed != testCase.confirmed || status.Finalized != testCase.finalized || status.Failed != testCase.failed {
			t.Fatalf("%s: observation = %+v", testCase.name, status)
		}
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
