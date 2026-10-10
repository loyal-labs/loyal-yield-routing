package backyard

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// The AUTO literal is the policy installed on Backyard's Settings, so the
// worker finds that account by equality and needs no binding.
func TestAutoPolicyIsTheInstalledAccount(t *testing.T) {
	installed := installedAutoPolicyAccount(t)
	view, err := squads.DecodeCanonicalPolicy(&chain.Account{Key: solana.MustPublicKeyFromBase58(installed.Address),
		Owner: squads.ProgramID, Lamports: installed.Lamports, Data: installed.Data})
	if err != nil || view == nil {
		t.Fatalf("installed AUTO policy does not decode: %v", err)
	}
	literal, err := autoPolicy(autoAUTOPYUSD)
	if err != nil {
		t.Fatal(err)
	}
	if !squads.ConstraintsEqual(view.Payload.Constraints, literal[:]) {
		t.Fatal("the AUTO literal is not the installed policy")
	}
}
