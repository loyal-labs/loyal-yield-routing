package backyardrwa

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestConfirmedRPCReadsUseOneContextSlot(t *testing.T) {
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBody, _ := io.ReadAll(request.Body)
		if strings.Contains(string(requestBody), `"method":"getSlot"`) {
			return response(`{"jsonrpc":"2.0","id":1,"result":42}`), nil
		}
		if !strings.Contains(string(requestBody), `"commitment":"confirmed"`) ||
			!strings.Contains(string(requestBody), `"minContextSlot":42`) {
			t.Fatalf("incoherent account request: %s", requestBody)
		}
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":[{"owner":"Tokenkeg","lamports":1,"data":["AQ==","base64"],"executable":false}]}}`), nil
	})
	slot, err := client.ConfirmedSlot(context.Background())
	if err != nil || slot != 42 {
		t.Fatalf("slot=%d err=%v", slot, err)
	}
	observedSlot, accounts, err := client.GetMultipleAccounts(context.Background(), []string{"account"}, slot)
	if err != nil || observedSlot != 43 || len(accounts) != 1 || len(accounts[0].Data) != 1 {
		t.Fatalf("slot=%d accounts=%+v err=%v", observedSlot, accounts, err)
	}
}

func TestConfirmedRPCRejectsAbsentAccount(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":[null]}}`), nil
	})
	if _, _, err := client.GetMultipleAccounts(context.Background(), []string{"missing"}, 42); err == nil {
		t.Fatal("missing account accepted")
	}
}

func TestConcreteTransactionRPCUsesConfirmedAndNoRetries(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBody, _ := io.ReadAll(request.Body)
		body := string(requestBody)
		switch {
		case strings.Contains(body, `"method":"simulateTransaction"`):
			if !strings.Contains(body, `"sigVerify":true`) || !strings.Contains(body, `"replaceRecentBlockhash":false`) || !strings.Contains(body, `"commitment":"confirmed"`) {
				t.Fatalf("unsafe simulation request: %s", body)
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":44},"value":{"err":null,"logs":["ok"],"unitsConsumed":9}}}`), nil
		case strings.Contains(body, `"method":"sendTransaction"`):
			if !strings.Contains(body, `"maxRetries":0`) || !strings.Contains(body, `"skipPreflight":false`) || !strings.Contains(body, `"preflightCommitment":"confirmed"`) {
				t.Fatalf("unsafe send request: %s", body)
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":"signature"}`), nil
		case strings.Contains(body, `"method":"getSignatureStatuses"`):
			if !strings.Contains(body, `"searchTransactionHistory":true`) {
				t.Fatalf("incomplete status recovery request: %s", body)
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":45,"err":null,"confirmationStatus":"confirmed"}]}}`), nil
		default:
			t.Fatalf("unexpected RPC request: %s", body)
			return nil, nil
		}
	})
	simulation, err := client.SimulateSignedTransaction(context.Background(), []byte{1, 2})
	if err != nil || simulation.Slot != 44 {
		t.Fatalf("simulation=%+v err=%v", simulation, err)
	}
	if _, err := client.SendSignedTransactionOnce(context.Background(), []byte{1, 2}, "signature"); err != nil {
		t.Fatal(err)
	}
	status, err := client.SignatureStatus(context.Background(), "signature")
	if err != nil || !status.Found || !status.Confirmed || status.ConfirmationSlot != 45 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestSendRejectsDifferentSignature(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":"different"}`), nil
	})
	if _, err := client.SendSignedTransactionOnce(context.Background(), []byte{1}, "expected"); err == nil {
		t.Fatal("mismatched RPC signature accepted")
	}
}
