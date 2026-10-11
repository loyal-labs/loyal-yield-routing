package policy

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// A klend-loop installed on a test Settings proves what Backyard installs: for
// Backyard's own vault and the AUTO/PYUSD lane, its policy is the literal the
// Backyard runtime executes the AUTO lane through, constraint for constraint
// and in the same order. The wYLDS lane differs from it only in its debt mint.
func TestKLendLoopPolicyIsBackyardAUTOPolicy(t *testing.T) {
	key := solana.MustPublicKeyFromBase58
	lane, err := newLoopLane(key("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"), key("Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk"),
		key("G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX"), key("GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm"), solana.TokenProgramID,
		key("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"), solana.Token2022ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	literal, err := backyard.PolicyLiteral("AUTO/AUTO/PYUSD")
	if err != nil {
		t.Fatal(err)
	}
	constraints := lane.constraints()
	if loop := (squads.Policy{VaultIndex: 0, Constraints: constraints[:]}); !loop.Equal(literal) {
		t.Fatalf("the loop policy is not Backyard's AUTO policy:\nloop    %+v\nliteral %+v", loop, literal)
	}
}
