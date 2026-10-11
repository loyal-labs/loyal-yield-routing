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

// The wYLDS overflow lane's literal is the klend-loop on the AUTO collateral
// and wYLDS debt reserves (a classic SPL Token mint), so the canary's loop
// proves it too.
func TestKLendLoopPolicyIsBackyardAUTOWYLDSPolicy(t *testing.T) {
	key := solana.MustPublicKeyFromBase58
	lane, err := newLoopLane(key("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"), key("Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk"),
		key("G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX"), key("GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm"), solana.TokenProgramID,
		key("8fr7WGTVFszfyNWRMXj6fRjZZAnDwmXwEpCrtzmUkdih"), solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	literal, err := backyard.PolicyLiteral("AUTO/AUTO/wYLDS")
	if err != nil {
		t.Fatal(err)
	}
	constraints := lane.constraints()
	if loop := (squads.Policy{VaultIndex: 0, Constraints: constraints[:]}); !loop.Equal(literal) {
		t.Fatalf("the loop policy is not Backyard's AUTO/wYLDS policy:\nloop    %+v\nliteral %+v", loop, literal)
	}
	pyusd, err := backyard.PolicyLiteral("AUTO/AUTO/PYUSD")
	if err != nil || pyusd.Equal(literal) {
		t.Fatal("the two AUTO lanes' literals must differ (their debt custodies)", err)
	}
}

// A hastra product installed on a test Settings proves what Backyard
// installs: for Backyard's own vault its policy is the Hastra literal the
// runtime converts through, constraint for constraint and in the same order.
func TestHastraPolicyIsBackyardHastraPolicy(t *testing.T) {
	constraints, _, _, err := hastraConstraints(solana.MustPublicKeyFromBase58("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"))
	if err != nil {
		t.Fatal(err)
	}
	literal, err := backyard.PolicyLiteral("wYLDS->AUTO/AUTO->wYLDS/USDC->wYLDS hastra")
	if err != nil {
		t.Fatal(err)
	}
	if product := (squads.Policy{VaultIndex: 0, Constraints: constraints[:]}); !product.Equal(literal) {
		t.Fatalf("the hastra product's policy is not Backyard's Hastra literal:\nproduct %+v\nliteral %+v", product, literal)
	}
}
