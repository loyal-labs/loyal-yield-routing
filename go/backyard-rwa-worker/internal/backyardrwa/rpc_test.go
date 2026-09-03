package backyardrwa

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	} else if errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatal("missing required account was misclassified as transient")
	}
}

func TestConfirmedRPCClassifiesExhaustedReadFailureAsTransient(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.retryBackoff = 0
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"minimum context slot has not been reached"}}`), nil
	})
	_, err := client.ConfirmedSlot(context.Background())
	if !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("exhausted confirmed read was not classified as transient: %v", err)
	}
}

func TestReadOnlyRPCRetriesButSendRemainsSingleAttempt(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.retryBackoff = 0
	readAttempts := 0
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		readAttempts++
		if readAttempts < readOnlyRPCAttempts {
			return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"minimum context slot has not been reached"}}`), nil
		}
		return response(`{"jsonrpc":"2.0","id":1,"result":42}`), nil
	})
	if slot, err := client.ConfirmedSlot(context.Background()); err != nil || slot != 42 || readAttempts != readOnlyRPCAttempts {
		t.Fatalf("slot=%d attempts=%d err=%v", slot, readAttempts, err)
	}

	sendAttempts := 0
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		sendAttempts++
		return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"temporarily unavailable"}}`), nil
	})
	if _, err := client.SendSignedTransactionOnce(context.Background(), []byte{1}, "expected"); err == nil {
		t.Fatal("failed send unexpectedly succeeded")
	}
	if sendAttempts != 1 {
		t.Fatalf("sendTransaction was attempted %d times", sendAttempts)
	}
}

func TestReadOnlyRPCRetryPublishesOnlyACompleteFreshDecode(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.retryBackoff = 0
	attempts := 0
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"optional":"poison","required":"invalid"}}`), nil
		}
		return response(`{"jsonrpc":"2.0","id":1,"result":{"required":42}}`), nil
	})
	output := struct {
		Optional *string `json:"optional"`
		Required int     `json:"required"`
	}{}
	if err := client.call(context.Background(), "getReadOnlyFixture", nil, &output); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || output.Optional != nil || output.Required != 42 {
		t.Fatalf("attempts=%d output=%+v", attempts, output)
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

func TestSimulationFailureIncludesSanitizedProgramResult(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	client.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":44},"value":{"err":{"InstructionError":[3,{"Custom":6001}]},"logs":["Program log: policy rejected","Program failed: custom program error: 0x1771"],"unitsConsumed":9}}}`), nil
	})
	_, err := client.SimulateSignedTransaction(context.Background(), []byte{1, 2})
	if err == nil || !strings.Contains(err.Error(), `slot=44 err={"InstructionError":[3,{"Custom":6001}]} last_log="Program failed: custom program error: 0x1771"`) {
		t.Fatalf("simulation failure lost its safe program diagnostics: %v", err)
	}
}

func TestConfirmedTransactionParsesStaticAndLoadedTokenBalances(t *testing.T) {
	client, _ := NewRPCClient("https://rpc.invalid")
	static, loaded := testPublicKey(1), testPublicKey(2)
	mint, authority := testPublicKey(3), testPublicKey(4)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBody, _ := io.ReadAll(request.Body)
		body := string(requestBody)
		if !strings.Contains(body, `"method":"getTransaction"`) || !strings.Contains(body, `"commitment":"confirmed"`) ||
			!strings.Contains(body, `"encoding":"json"`) || !strings.Contains(body, `"maxSupportedTransactionVersion":0`) {
			t.Fatalf("unsafe transaction receipt request: %s", body)
		}
		returnData := base64.StdEncoding.EncodeToString(make([]byte, 8))
		return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"slot":91,"meta":{"err":null,"preTokenBalances":[{"accountIndex":0,"mint":"%s","owner":"%s","programId":"%s","uiTokenAmount":{"amount":"10"}},{"accountIndex":1,"mint":"%s","owner":"%s","programId":"%s","uiTokenAmount":{"amount":"1"}}],"postTokenBalances":[{"accountIndex":0,"mint":"%s","owner":"%s","programId":"%s","uiTokenAmount":{"amount":"7"}},{"accountIndex":1,"mint":"%s","owner":"%s","programId":"%s","uiTokenAmount":{"amount":"4"}}],"loadedAddresses":{"writable":["%s"],"readonly":[]},"returnData":{"programId":"%s","data":["%s","base64"]},"logMessages":["Program return: %s %s"]},"transaction":{"message":{"accountKeys":["%s"]}}}}`,
			mint, authority, classicTokenProgram, mint, authority, token2022Program,
			mint, authority, classicTokenProgram, mint, authority, token2022Program,
			loaded, bridgeAdaptorProgram, returnData, bridgeAdaptorProgram, returnData, static)), nil
	})
	receipt, err := client.ConfirmedTransaction(context.Background(), "signature")
	if err != nil || receipt.Slot != 91 || receipt.Signature != "signature" ||
		len(receipt.PreTokenBalances) != 2 || receipt.PreTokenBalances[0].Address != static ||
		receipt.PreTokenBalances[1].Address != loaded || receipt.PostTokenBalances[1].Raw != 4 ||
		receipt.ReturnData == nil || receipt.ReturnData.ProgramID != bridgeAdaptorProgram || len(receipt.Logs) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}
