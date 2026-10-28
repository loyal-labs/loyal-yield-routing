package squads

import (
	"encoding/base64"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

func TestDecodeSettingsWithNonzeroAllocationTail(t *testing.T) {
	// Public account 6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3 at slot 462478471.
	// The canonical Squads SDK consumes 168 of its 201 bytes.
	data, err := base64.StdEncoding.DecodeString("37OjvrHgQ60KHQIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/AQAAAJcaJGJKF/Wed7NyrW3KxJvGybD8BjcCj1JwIF/Ul3wIBwABAgEAAAAAAAAArW3KxJvGybD8BjcCj1JwIF/Ul3wIBwABQAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	account := &chain.Account{Owner: ProgramID, Data: data}
	got, err := DecodeSettings(account)
	if err != nil {
		t.Fatal(err)
	}
	wantSigner := solana.MustPublicKeyFromBase58("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ")
	if got.Threshold != 1 || len(got.Signers) != 1 || got.Signers[0].Key != wantSigner || got.Signers[0].Permissions != 7 || got.PolicySeed == nil || *got.PolicySeed != 258 {
		t.Fatalf("settings differ from canonical SDK: %+v", got)
	}
	for _, tc := range []struct {
		name    string
		account chain.Account
	}{
		{"wrong owner", chain.Account{Owner: solana.PublicKey{}, Data: data}},
		{"executable", chain.Account{Owner: ProgramID, Data: data, Executable: true}},
		{"missing reserved field", chain.Account{Owner: ProgramID, Data: data[:167]}},
		{"truncated signer", chain.Account{Owner: ProgramID, Data: data[:140]}},
		{"wrong discriminator", chain.Account{Owner: ProgramID, Data: append([]byte{0}, data[1:]...)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSettings(&tc.account); err == nil {
				t.Fatal("malformed settings accepted")
			}
		})
	}
}
