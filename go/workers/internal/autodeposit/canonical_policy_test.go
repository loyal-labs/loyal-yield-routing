package autodeposit

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The Rust proxy recorded these creator and current-account verdicts in
// testdata/klend/golden.json (see fleet TestKLendGoldenParityWithRustProxy).
func TestCanonicalSubscriptionPolicyGoldenParityWithRustProxy(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/klend/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Operation, Error string
		Request, Output        json.RawMessage
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, c := range cases {
		if c.Operation != "buildCanonicalSubscriptionPolicy" {
			continue
		}
		seen++
		t.Run(c.Name, func(t *testing.T) {
			var request struct {
				Settings, RootAuthority, Payer, DelegatedSigner, Wallet, Vault, PolicyDataHex string
				PolicySeed, MaxAmountPerPeriod                                                uint64
			}
			if err := json.Unmarshal(c.Request, &request); err != nil {
				t.Fatal(err)
			}
			r := CanonicalSubscriptionPolicyRequest{request.Settings, request.RootAuthority, request.Payer, request.DelegatedSigner, request.PolicySeed, request.Wallet, request.Vault, request.MaxAmountPerPeriod}
			ix, err := BuildCanonicalSubscriptionPolicy(r)
			if err == nil && request.PolicyDataHex != "" {
				data, decodeErr := hex.DecodeString(request.PolicyDataHex)
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				err = VerifyCanonicalSubscriptionPolicyAccount(r, &chain.Account{Owner: squads.ProgramID, Data: data})
			}
			if c.Error != "" {
				if err == nil {
					t.Fatalf("Rust refused (%s); Go accepted", c.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rust accepted; Go refused: %v", err)
			}
			var rust struct {
				SchemaVersion int
				Operation     string
				Route         struct {
					Public    []json.RawMessage
					Protected []struct {
						Step, Program, DataHex string
						Accounts               []fleet.InstructionAccount
					}
				}
			}
			if err = json.Unmarshal(c.Output, &rust); err != nil {
				t.Fatal(err)
			}
			if rust.SchemaVersion != 1 || len(rust.Route.Public) != 0 || len(rust.Route.Protected) != 1 {
				t.Fatalf("unexpected Rust route shape: %s", c.Output)
			}
			want := rust.Route.Protected[0]
			if ix.Step != want.Step || ix.Program != want.Program || hex.EncodeToString(ix.Data) != want.DataHex || !reflect.DeepEqual(ix.Accounts, want.Accounts) {
				t.Fatalf("Go creator differs from Rust proxy\n go: %+v %x\nrust: %s", ix, ix.Data, c.Output)
			}
		})
	}
	if seen < 10 {
		t.Fatalf("golden record has only %d canonical policy cases", seen)
	}
}
