package backyard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func adaptorFailureLogs(program string, code int) []string {
	return []string{
		"Program " + backyardDelegate + " invoke [1]",
		"Program log: Instruction: StageSquadsToVoltr",
		"Program " + program + " invoke [2]",
		"Program log: AnchorError occurred. Error Code: ReportSlot. Error Number: 0x" +
			strings.ToUpper(strings.TrimPrefix(jsonNumberHex(code), "0x")),
		"Program " + program + " failed: custom program error: 0x" + jsonNumberHex(code),
		"Program " + backyardDelegate + " failed: custom program error: 0x" + jsonNumberHex(code),
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

// A settled failure terminates in `failed`, which neither blocks capital nor
// keeps the durable decision epoch, so the next tick decides afresh.
func TestTerminalFailedRowNeverBlocksCapitalAndAdvancesTheEpoch(t *testing.T) {
	t.Parallel()
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
}

// Review fix: a transaction error is only a failure once it settles. A
// processed-only error can be forked away, so it must stay an observation and
// never drive a terminal journal transition.
func TestSignatureStatusFailureRequiresSettlement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  signatureCommitment
	}{
		{
			name:  "a processed-only failure is not a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"processed"}`,
			want:  signatureProcessed,
		},
		{
			name:  "a finalized failure is a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"}`,
			want:  signatureFailed,
		},
		{
			name:  "a confirmed failure is a failure",
			value: `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"confirmed"}`,
			want:  signatureFailed,
		},
		{
			name:  "a confirmed success is confirmed",
			value: `{"slot":45,"err":null,"confirmationStatus":"confirmed"}`,
			want:  signatureConfirmed,
		},
		{
			name:  "a processed success is only an observation",
			value: `{"slot":45,"err":null,"confirmationStatus":"processed"}`,
			want:  signatureProcessed,
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
		if status != testCase.want {
			t.Fatalf("%s: observation = %+v", testCase.name, status)
		}
	}
}
