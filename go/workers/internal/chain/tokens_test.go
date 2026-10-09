package chain

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestTokenAccountsRefusesAViewOlderThanAsked(t *testing.T) {
	owner, account := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	slot := 700
	client := serve(t, func(request) (int, any) {
		return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": slot}, "value": []any{map[string]any{"pubkey": account.String(), "account": map[string]any{
			"lamports": 2_039_280, "owner": solana.TokenProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString([]byte{4}), "base64"}, "executable": false}}}})
	})
	read, accounts, err := client.TokenAccounts(context.Background(), owner, solana.TokenProgramID, rpc.CommitmentConfirmed, 700)
	if err != nil || read != 700 || len(accounts) != 1 || accounts[0].Key != account || accounts[0].Owner != solana.TokenProgramID || accounts[0].Data[0] != 4 {
		t.Fatalf("token accounts %d %+v, %v", read, accounts, err)
	}
	slot = 699
	if _, _, err := client.TokenAccounts(context.Background(), owner, solana.TokenProgramID, rpc.CommitmentConfirmed, 700); err == nil {
		t.Fatal("a view older than the minimum slot was accepted")
	}
}
