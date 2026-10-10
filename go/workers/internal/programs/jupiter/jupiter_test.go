package jupiter

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// The v2 swap constraint admits a real swap of the canary vault's USDC into
// its USDT, exactly as /swap-instructions returned it (instructionVersion=V2,
// useSharedAccounts), and nothing that sends the output elsewhere, takes a fee,
// pays from another mint or exceeds the slippage bound.
func TestSharedAccountsRouteV2AllowedAdmitsTheAPIsSwap(t *testing.T) {
	raw, err := os.ReadFile("testdata/swap_instructions_v2_usdc_usdt.json")
	if err != nil {
		t.Fatal(err)
	}
	var response SwapInstructions
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	ix, err := response.SwapInstruction.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeSharedAccountsRouteV2(ix); err != nil {
		t.Fatal(err)
	}
	returned, err := squads.InstructionOf(ix)
	if err != nil {
		t.Fatal(err)
	}
	vault := solana.MustPublicKeyFromBase58("F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e")
	usdc := solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	usdt := solana.MustPublicKeyFromBase58("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	allowed, err := OwnSwapAllowed(vault, usdc, solana.TokenProgramID, usdt, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	constraint := SharedAccountsRouteV2Allowed(allowed, 50)
	foreign := solana.MustPublicKeyFromBase58("6abgRZvHXZFjsr517KLdbqjQArCtzQXK6bSs9yZjMXqK") // another wallet's USDT account

	cases := []struct {
		name   string
		mutate func(*squads.Instruction)
		admit  bool
	}{
		{"as returned", func(*squads.Instruction) {}, true},
		{"destination swapped", func(ix *squads.Instruction) { ix.Accounts[5].PublicKey = foreign }, false},
		{"platform fee set", func(ix *squads.Instruction) { binary.LittleEndian.PutUint16(ix.Data[V2FeesOffset:], 10) }, false},
		{"positive slippage fee set", func(ix *squads.Instruction) { binary.LittleEndian.PutUint16(ix.Data[V2FeesOffset+2:], 10) }, false},
		{"source mint changed", func(ix *squads.Instruction) { ix.Accounts[6].PublicKey = usdt }, false},
		{"slippage above the bound", func(ix *squads.Instruction) { binary.LittleEndian.PutUint16(ix.Data[V2SlippageOffset:], 51) }, false},
	}
	for _, tc := range cases {
		ix := squads.Instruction{ProgramID: returned.ProgramID, Accounts: slices.Clone(returned.Accounts), Data: slices.Clone(returned.Data)}
		tc.mutate(&ix)
		if got := squads.Admits(constraint, ix, nil); got != tc.admit {
			t.Errorf("%s: admitted %v, want %v", tc.name, got, tc.admit)
		}
	}
}
