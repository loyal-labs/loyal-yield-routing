package chain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type request struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func serve(t *testing.T, handle func(request) (int, any)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		status, result := handle(req)
		w.WriteHeader(status)
		if text, ok := result.(string); ok {
			_, _ = w.Write([]byte(text))
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL+"/?api-key=secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func result(value any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 0, "result": value}
}

func TestRateLimitIsTypedAndKeepsTheKeyOut(t *testing.T) {
	for name, body := range map[string]any{
		"plain body":     "Too Many Requests",
		"json-rpc error": map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32429, "message": "rate limited"}},
	} {
		client := serve(t, func(request) (int, any) { return http.StatusTooManyRequests, body })
		_, err := client.Slot(context.Background(), rpc.CommitmentConfirmed)
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("%s: want ErrRateLimited, got %v", name, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s: error leaks the endpoint key: %v", name, err)
		}
	}
}

func TestTransportFailuresKeepTheKeyOut(t *testing.T) {
	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hang) })
	timeout, err := New(server.URL+"/?api-key=secret", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"client timeout": func() error { _, err := timeout.Slot(context.Background(), rpc.CommitmentConfirmed); return err },
		"cancelled":      func() error { _, err := timeout.Slot(cancelled, rpc.CommitmentConfirmed); return err },
		"node unhealthy": func() error {
			client := serve(t, func(request) (int, any) {
				return http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32005, "message": "Node is unhealthy"}}
			})
			_, err := client.Slot(context.Background(), rpc.CommitmentConfirmed)
			return err
		},
		"node behind": func() error {
			client := serve(t, func(request) (int, any) {
				return http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32016, "message": "Minimum context slot has not been reached"}}
			})
			_, err := client.Slot(context.Background(), rpc.CommitmentConfirmed)
			if !errors.Is(err, ErrBehind) {
				t.Fatalf("want ErrBehind, got %v", err)
			}
			return err
		},
	} {
		err := call()
		if name != "cancelled" && !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: want ErrUnavailable, got %v", name, err)
		}
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%s: error is missing or leaks the endpoint: %v", name, err)
		}
	}
}

func TestAccountsPinsEveryBatchToTheFirstSlot(t *testing.T) {
	keys := make([]solana.PublicKey, 150)
	for i := range keys {
		keys[i] = solana.NewWallet().PublicKey()
	}
	var minSlots []uint64
	client := serve(t, func(req request) (int, any) {
		var batch []string
		_ = json.Unmarshal(req.Params[0], &batch)
		var opts struct {
			MinContextSlot uint64 `json:"minContextSlot"`
		}
		_ = json.Unmarshal(req.Params[1], &opts)
		minSlots = append(minSlots, opts.MinContextSlot)
		values := make([]any, len(batch))
		for i := range batch {
			if i%2 == 0 {
				values[i] = map[string]any{"lamports": 1, "owner": solana.SystemProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString([]byte{7}), "base64"}, "executable": false}
			}
		}
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 500 + len(minSlots)}, "value": values})
	})
	slot, accounts, err := client.Accounts(context.Background(), keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 501 || len(minSlots) != 2 || minSlots[0] != 0 || minSlots[1] != 501 {
		t.Fatalf("slot %d, minContextSlot per call %v", slot, minSlots)
	}
	if len(accounts) != 150 || accounts[0] == nil || accounts[0].Key != keys[0] || accounts[0].Data[0] != 7 || accounts[1] != nil || accounts[101] != nil || accounts[100] == nil {
		t.Fatal("accounts are not returned in key order with absent accounts as nil")
	}
}

func TestSendWireAsksTheNodeNotToRebroadcast(t *testing.T) {
	var opts map[string]any
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result(solana.Signature{1}.String())
	})
	if err := client.SendWire(context.Background(), []byte{1, 2, 3}, true); err != nil {
		t.Fatal(err)
	}
	if opts["maxRetries"] != float64(0) || opts["skipPreflight"] != true || opts["encoding"] != "base64" {
		t.Fatalf("send options %v", opts)
	}
}

func TestSignatureStateReadsCommitmentAndChainError(t *testing.T) {
	statuses := []any{
		map[string]any{"slot": 77, "confirmationStatus": "confirmed", "err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 6}}}},
		nil,
	}
	call := 0
	client := serve(t, func(request) (int, any) {
		status := statuses[call]
		call++
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 90}, "value": []any{status}})
	})
	failed, err := client.SignatureState(context.Background(), solana.Signature{1}.String())
	if err != nil || !failed.Found || failed.Slot != 77 || failed.Commitment != Confirmed || failed.Err != `{"InstructionError":[0,{"Custom":6}]}` || failed.ContextSlot != 90 {
		t.Fatalf("failed signature read as %+v, %v", failed, err)
	}
	absent, err := client.SignatureState(context.Background(), solana.Signature{1}.String())
	if err != nil || absent.Found || absent.ContextSlot != 90 {
		t.Fatalf("absent signature read as %+v, %v", absent, err)
	}
}

func TestHistoryIsBoundedConfirmedAndCursored(t *testing.T) {
	address, before, landed, failed := solana.NewWallet().PublicKey(), solana.Signature{9}, solana.Signature{1}, solana.Signature{2}
	var opts map[string]any
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result([]any{
			map[string]any{"signature": landed.String(), "slot": 12, "err": nil},
			map[string]any{"signature": failed.String(), "slot": 11, "err": map[string]any{"InstructionError": []any{0, "Custom"}}},
		})
	})
	history, err := client.History(context.Background(), address, 2, before, rpc.CommitmentConfirmed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if opts["limit"] != float64(2) || opts["commitment"] != "confirmed" || opts["before"] != before.String() {
		t.Fatalf("history options %v", opts)
	}
	if len(history) != 2 || history[0] != (Signed{Signature: landed, Slot: 12}) || history[1] != (Signed{Signature: failed, Slot: 11, Failed: true}) {
		t.Fatalf("history %+v", history)
	}
	if _, err := client.History(context.Background(), address, 1, solana.Signature{}, rpc.CommitmentConfirmed, 0); err == nil {
		t.Fatal("a page longer than the limit was accepted")
	}
}

func TestReceiptResolvesLoadedAddresses(t *testing.T) {
	payer, loaded, mint, owner := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	tx, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(solana.SystemProgramID, solana.AccountMetaSlice{solana.Meta(payer).SIGNER().WRITE()}, nil)}, solana.Hash{1}, solana.TransactionPayer(payer))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	balance := func(amount string) []any {
		return []any{map[string]any{"accountIndex": 2, "mint": mint.String(), "owner": owner.String(), "programId": solana.TokenProgramID.String(), "uiTokenAmount": map[string]any{"amount": amount, "decimals": 6, "uiAmountString": "0"}}}
	}
	client := serve(t, func(request) (int, any) {
		return http.StatusOK, result(map[string]any{
			"slot":        900,
			"transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"},
			"meta": map[string]any{
				"err": nil, "fee": 5000, "logMessages": []string{},
				"preTokenBalances": balance("10"), "postTokenBalances": balance("25"),
				"loadedAddresses": map[string]any{"writable": []string{loaded.String()}, "readonly": []string{}},
				"returnData":      map[string]any{"programId": owner.String(), "data": []string{base64.StdEncoding.EncodeToString([]byte{4, 2}), "base64"}},
			},
		})
	})
	receipt, err := client.Receipt(context.Background(), solana.Signature{1}, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Pre[loaded].Amount != 10 || receipt.Post[loaded].Amount != 25 || receipt.Post[loaded].Mint != mint || receipt.Post[loaded].Decimals != 6 || receipt.Err != nil {
		t.Fatalf("token balance not resolved through the loaded address: %+v", receipt)
	}
	if receipt.ReturnProgram != owner || string(receipt.ReturnData) != string([]byte{4, 2}) {
		t.Fatalf("return data %s %v", receipt.ReturnProgram, receipt.ReturnData)
	}
}

func TestFeeIsReadNoOlderThanTheAskedSlot(t *testing.T) {
	var opts map[string]any
	fee := any(5000)
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 44}, "value": fee})
	})
	lamports, slot, err := client.Fee(context.Background(), []byte{1}, rpc.CommitmentConfirmed, 42)
	if err != nil || lamports != 5000 || slot != 44 || opts["minContextSlot"] != float64(42) || opts["commitment"] != "confirmed" {
		t.Fatalf("fee %d at %d, %v, options %v", lamports, slot, err, opts)
	}
	fee = nil
	if _, _, err := client.Fee(context.Background(), []byte{1}, rpc.CommitmentConfirmed, 42); err == nil {
		t.Fatal("a fee for an expired blockhash was accepted")
	}
}

func TestSimulateReturnsTheAskedAccountsAsSimulated(t *testing.T) {
	present, absent := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	var opts map[string]any
	failure := any(nil)
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 50}, "value": map[string]any{"err": failure, "unitsConsumed": 9, "accounts": []any{
			map[string]any{"lamports": 3, "owner": solana.TokenProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString([]byte{8}), "base64"}, "executable": false}, nil}}})
	})
	minimum := uint64(48)
	capture := &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: []solana.PublicKey{present, absent}}
	simulated, err := client.Simulate(context.Background(), []byte{1}, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimum, Accounts: capture})
	accounts := simulated.Accounts
	captured, _ := opts["accounts"].(map[string]any)
	if err != nil || simulated.Slot != 50 || simulated.Units != 9 || accounts[0].Key != present || accounts[0].Data[0] != 8 || accounts[1] != nil ||
		captured["encoding"] != "base64" || opts["minContextSlot"] != float64(48) {
		t.Fatalf("capture %+v %+v %v, options %v", simulated, accounts, err, opts)
	}
	failure = "BlockhashNotFound"
	var simulationErr *SimulationError
	if _, err := client.Simulate(context.Background(), []byte{1}, rpc.SimulateTransactionOpts{Accounts: capture}); !errors.As(err, &simulationErr) {
		t.Fatalf("a failed simulation is not a SimulationError: %v", err)
	}
}

func TestProgramAccountsAreFilteredAndReadNoOlderThanTheAskedSlot(t *testing.T) {
	program, owned := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	var opts map[string]any
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 91}, "value": []any{map[string]any{"pubkey": owned.String(), "account": map[string]any{
			"lamports": 2, "owner": program.String(), "data": []string{base64.StdEncoding.EncodeToString([]byte{1, 2}), "base64"}, "executable": false}}}})
	})
	slot, accounts, err := client.ProgramAccounts(context.Background(), program, []rpc.RPCFilter{{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: owned[:]}}}, rpc.CommitmentConfirmed, 90)
	filters, _ := opts["filters"].([]any)
	if err != nil || slot != 91 || len(accounts) != 1 || accounts[0].Key != owned || accounts[0].Owner != program ||
		opts["withContext"] != true || opts["minContextSlot"] != float64(90) || len(filters) != 1 {
		t.Fatalf("program accounts at %d: %+v %v, options %v", slot, accounts, err, opts)
	}
}

func TestFinalizedBlockHeightAtReadsTheBlockWithoutTransactions(t *testing.T) {
	var opts map[string]any
	block := any(map[string]any{"blockhash": solana.Hash{1}.String(), "previousBlockhash": solana.Hash{2}.String(), "parentSlot": 9, "blockHeight": 77})
	client := serve(t, func(req request) (int, any) {
		_ = json.Unmarshal(req.Params[1], &opts)
		return http.StatusOK, result(block)
	})
	height, err := client.FinalizedBlockHeightAt(context.Background(), 10)
	if err != nil || height != 77 || opts["transactionDetails"] != "none" || opts["commitment"] != "finalized" || opts["rewards"] != false {
		t.Fatalf("height %d, %v, options %v", height, err, opts)
	}
	block = nil
	if _, err := client.FinalizedBlockHeightAt(context.Background(), 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a slot without a finalized block: %v", err)
	}
}

func TestSlotSamplesSumOnlyUsableSamples(t *testing.T) {
	client := serve(t, func(request) (int, any) {
		return http.StatusOK, result([]any{
			map[string]any{"slot": 3, "numSlots": 150, "numTransactions": 1, "samplePeriodSecs": 60},
			map[string]any{"slot": 2, "numSlots": 0, "numTransactions": 1, "samplePeriodSecs": 60},
			map[string]any{"slot": 1, "numSlots": 72, "numTransactions": 1, "samplePeriodSecs": 0},
		})
	})
	slots, seconds, err := client.SlotSamples(context.Background(), 5)
	if err != nil || slots != 150 || seconds != 60 {
		t.Fatalf("samples %d slots over %d s, %v", slots, seconds, err)
	}
}

// A pooled endpoint's lagging node answers -32016 until it reaches the asked
// slot: the read gets the caught-up answer, a broadcast is never sent twice,
// and a node that stays behind is still unavailable.
func TestLaggingNodeIsAskedAgainForReadsOnly(t *testing.T) {
	behind := map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32016, "message": "Minimum context slot has not been reached"}}
	calls, lag := 0, 2
	client := serve(t, func(req request) (int, any) {
		calls++
		if req.Method == "sendTransaction" || calls <= lag {
			return http.StatusOK, behind
		}
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": 44}, "value": 5000})
	})
	lamports, _, err := client.Fee(context.Background(), []byte{1}, rpc.CommitmentConfirmed, 42)
	if err != nil || lamports != 5000 || calls != lag+1 {
		t.Fatalf("fee %d after %d calls: %v", lamports, calls, err)
	}
	calls = 0
	if err := client.SendWire(context.Background(), []byte{1, 2, 3}, true); !errors.Is(err, ErrBehind) || calls != 1 {
		t.Fatalf("broadcast sent %d times: %v", calls, err)
	}
	calls, lag = 0, 1_000
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := client.Fee(ctx, []byte{1}, rpc.CommitmentConfirmed, 42); !errors.Is(err, ErrBehind) {
		t.Fatalf("a node that stays behind answered: %v", err)
	}
}
