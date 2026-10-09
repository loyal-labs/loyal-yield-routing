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
			},
		})
	})
	receipt, err := client.Receipt(context.Background(), solana.Signature{1}, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Pre[loaded].Amount != 10 || receipt.Post[loaded].Amount != 25 || receipt.Post[loaded].Mint != mint || receipt.Err != nil {
		t.Fatalf("token balance not resolved through the loaded address: %+v", receipt)
	}
}
