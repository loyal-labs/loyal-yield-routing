package fleetexec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func adapterForResults(t *testing.T, results map[string]json.RawMessage) *RPCAdapter {
	t.Helper()
	return &RPCAdapter{url: "http://unused.invalid", deadline: time.Second, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		result, ok := results[request.Method]
		if !ok {
			t.Fatalf("unexpected RPC %s", request.Method)
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}}
}
func TestRPCSignatureCommitmentAndArrayContract(t *testing.T) {
	for _, tc := range []struct {
		name, value                 string
		found, confirmed, finalized bool
	}{
		{"absent", "null", false, false, false},
		{"processed", `{"slot":98,"confirmationStatus":"processed","err":null}`, true, false, false},
		{"processed_error", `{"slot":98,"confirmationStatus":"processed","err":{"InstructionError":[0,"InvalidArgument"]}}`, true, false, false},
		{"confirmed", `{"slot":98,"confirmationStatus":"confirmed","err":null}`, true, true, false},
		{"finalized", `{"slot":98,"confirmationStatus":"finalized","err":null}`, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := adapterForResults(t, map[string]json.RawMessage{"getSignatureStatuses": json.RawMessage(`{"context":{"slot":100},"value":[` + tc.value + `]}`), "getBlockHeight": json.RawMessage(`5000`)})
			status, err := adapter.SignatureStatus(context.Background(), "signature")
			if err != nil {
				t.Fatal(err)
			}
			if status.Found != tc.found || status.Confirmed != tc.confirmed || status.Finalized != tc.finalized {
				t.Fatalf("unexpected status %+v", status)
			}
		})
	}
}
func TestRPCFinalizedReceiptUsesBase64TransactionAndBindsKeys(t *testing.T) {
	fixture := mustSignedFixture(t)
	result, _ := json.Marshal(map[string]any{"slot": 123, "transaction": []string{fixture.SignedWireB64, "base64"}, "meta": map[string]any{"err": nil, "preTokenBalances": []any{}, "postTokenBalances": []any{}, "loadedAddresses": map[string]any{"writable": []string{}, "readonly": []string{}}}})
	adapter := adapterForResults(t, map[string]json.RawMessage{"getTransaction": result})
	receipt, err := adapter.FinalizedTransaction(context.Background(), fixture.SignatureB58)
	if err != nil {
		t.Fatal(err)
	}
	record := SubmissionRecord{Signature: fixture.SignatureB58, MessageHash: fixture.MessageSHA256, SignedTransaction: mustDecodeB64(t, fixture.SignedWireB64)}
	if err := VerifyReceiptIdentity(receipt, record, 123); err != nil {
		t.Fatal(err)
	}
	if _, err := receipt.WithAccountAddresses([]string{fixture.FeePayer, fixture.SecondaryAccount}); err != nil {
		t.Fatal(err)
	}
	if _, err := receipt.WithAccountAddresses([]string{fixture.FeePayer, fixture.FeePayer}); err == nil {
		t.Fatal("different receipt address accepted")
	}
}
func TestReceiptBalanceAmountsRejectMalformedAndDuplicateMetadata(t *testing.T) {
	balance := rpcTokenBalance{AccountIndex: 0, Mint: "mint"}
	balance.UITokenAmount.Amount = "12garbage"
	if _, err := receiptDeltas([]string{"account"}, []rpcTokenBalance{balance}, nil); err == nil {
		t.Fatal("partial amount parse accepted")
	}
	balance.UITokenAmount.Amount = "12"
	if _, err := receiptDeltas([]string{"account"}, []rpcTokenBalance{balance, balance}, nil); err == nil {
		t.Fatal("duplicate balance accepted")
	}
	balance.AccountIndex = -1
	if _, err := receiptDeltas([]string{"account"}, []rpcTokenBalance{balance}, nil); err == nil {
		t.Fatal("negative account index accepted")
	}
}
