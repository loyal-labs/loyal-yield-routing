package earn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
)

// confirmedTransactionServer answers getTransaction with one fixed result.
func confirmedTransactionServer(t *testing.T, result any) *solanarpc.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID uint64 `json:"id"`
		}
		_ = json.Unmarshal(body, &request)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return solanarpc.New(server.URL, 5*time.Second)
}

func tokenRows(rows ...[2]any) []map[string]any {
	out := []map[string]any{}
	for _, row := range rows {
		out = append(out, map[string]any{"accountIndex": row[0], "mint": usdcMint.String(), "uiTokenAmount": map[string]any{"amount": row[1], "decimals": 6}})
	}
	return out
}

func TestClaimCustodyTransferNeedsOneExactCounterparty(t *testing.T) {
	payer, custody, wallet := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	transaction, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(tokenProgram, solana.AccountMetaSlice{
		{PublicKey: custody, IsWritable: true}, {PublicKey: wallet, IsWritable: true}}, []byte{3})}, solana.Hash{}, solana.TransactionPayer(payer))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	signature := solana.SignatureFromBytes(make([]byte, 64)).String()
	response := func(pre, post []map[string]any) map[string]any {
		return map[string]any{"slot": 77, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"},
			"meta": map[string]any{"err": nil, "preTokenBalances": pre, "postTokenBalances": post}}
	}
	// Accounts: 0 payer, 1 custody, 2 wallet.
	deposit := confirmedTransactionServer(t, response(tokenRows([2]any{2, "900"}), tokenRows([2]any{1, "300"}, [2]any{2, "600"})))
	transfer, err := readCustodyTransfer(context.Background(), deposit, signature, 77, custody)
	if err != nil {
		t.Fatal(err)
	}
	if transfer.source != wallet || transfer.destination != custody || transfer.sourcePre != 900 || transfer.sourcePost != 600 || transfer.destinationPre != 0 || transfer.destinationPost != 300 {
		t.Fatalf("deposit transfer = %+v", transfer)
	}
	if _, err := readCustodyTransfer(context.Background(), deposit, signature, 78, custody); err == nil {
		t.Fatal("a transfer at another slot than the account update was accepted")
	}
	// A claim debit may not hide an account creation behind a missing row.
	claim := confirmedTransactionServer(t, response(tokenRows([2]any{1, "300"}), tokenRows([2]any{1, "0"}, [2]any{2, "300"})))
	if _, err := readCustodyTransfer(context.Background(), claim, signature, 77, custody); err == nil {
		t.Fatal("a claim without the destination's pre balance was accepted")
	}
	ambiguous := confirmedTransactionServer(t, response(tokenRows([2]any{0, "300"}, [2]any{2, "300"}), tokenRows([2]any{0, "0"}, [2]any{1, "300"}, [2]any{2, "0"})))
	if _, err := readCustodyTransfer(context.Background(), ambiguous, signature, 77, custody); err == nil {
		t.Fatal("two exact counterparties were accepted")
	}
	pending := confirmedTransactionServer(t, nil)
	if _, err := readCustodyTransfer(context.Background(), pending, signature, 77, custody); !errors.Is(err, errProofPending) {
		t.Fatalf("an unavailable transaction = %v, want proof pending", err)
	}
}
