package policy

import (
	"context"
	"encoding/binary"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

// The safety claim of jupiter.SharedAccountsRouteV2Allowed, on mainnet: with
// every slot it pins left as the swap API returned it, no rewrite of the free
// route accounts (or of the pinned authority sets) moves funds anywhere but
// the user's destination. Each tamper simulates a real 1 USDC -> USDT route,
// quoted_out_amount zeroed (the policy leaves it free), with a whale as the
// user, and must fail; a failed transaction moves nothing.
//
// Cases, per venue:
//   - foreign output: the venue's output account (the program authority's
//     destination ATA) is another wallet's USDT account;
//   - user as venue authority: every route slot holding the program authority
//     is the user, signing, and the venue's input account is the user's other
//     funded USDC account, with the output kept or made foreign;
//   - mixed authority: the authority slot, the id byte, or both with the
//     route's authority slots, name authority j while the token accounts stay
//     authority i's (the policy pins all 16 authorities and their ATAs as sets).
//
// It needs TEST_SOLANA_RPC_URL and calls the keyless Jupiter API.
func TestSharedRouteV2TamperOnMainnet(t *testing.T) {
	endpoint := os.Getenv("TEST_SOLANA_RPC_URL")
	if endpoint == "" {
		t.Skip("TEST_SOLANA_RPC_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := chain.New(endpoint, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	quotes, err := jupiter.NewClient(jupiter.LiteBase, "", &http.Client{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	key := solana.MustPublicKeyFromBase58
	usdc, usdt := key("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), key("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	user := key("5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9")      // holds USDC in its ATA and in others
	userOther := key("7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU") // user's non-ATA USDC account
	foreign := key("6abgRZvHXZFjsr517KLdbqjQArCtzQXK6bSs9yZjMXqK")   // another wallet's USDT account
	userSource, _ := spl.AssociatedTokenAddress(user, usdc, solana.TokenProgramID)
	userDestination, _ := spl.AssociatedTokenAddress(user, usdt, solana.TokenProgramID)

	for _, venue := range []string{"Whirlpool", "Raydium CLMM", "Meteora DLMM"} {
		quote, err := quotes.Quote(ctx, jupiter.QuoteRequest{InputMint: usdc.String(), OutputMint: usdt.String(), Amount: 1_000_000,
			SlippageBPS: SwapSlippageBPS, MaxAccounts: swapMaxAccounts, InstructionVersion: "V2", Dexes: venue})
		if err != nil {
			t.Fatalf("%s quote: %v", venue, err)
		}
		response, err := quotes.SwapInstructions(ctx, quote, user, true)
		if err != nil {
			t.Fatalf("%s swap instructions: %v", venue, err)
		}
		ix, err := response.SwapInstruction.Decode()
		if err != nil {
			t.Fatal(err)
		}
		returned, _ := ix.Data()
		accounts := ix.Accounts()
		id := returned[jupiter.V2IDOffset]
		other := (id + 1) % jupiter.ProgramAuthorities
		authority, programSource, programDestination := accounts[0].PublicKey, accounts[3].PublicKey, accounts[4].PublicKey
		otherSource, _ := spl.AssociatedTokenAddress(jupiter.ProgramAuthority(other), usdc, solana.TokenProgramID)
		otherDestination, _ := spl.AssociatedTokenAddress(jupiter.ProgramAuthority(other), usdt, solana.TokenProgramID)
		watch := []solana.PublicKey{userSource, userDestination, userOther, foreign, programSource, programDestination, otherSource, otherDestination}

		// simulate runs a rewritten copy of the instruction; a route rewrite
		// that finds nothing to rewrite fails the test.
		type rewrite struct {
			from solana.PublicKey
			to   solana.PublicKey
			sign bool
		}
		simulate := func(name string, route []rewrite, fixed func(metas []*solana.AccountMeta, data []byte)) {
			metas := make([]*solana.AccountMeta, len(accounts))
			for i, meta := range accounts {
				copied := *meta
				metas[i] = &copied
			}
			data := append([]byte(nil), returned...)
			for _, r := range route {
				found := 0
				for i := 12; i < len(metas); i++ {
					if metas[i].PublicKey == r.from {
						metas[i].PublicKey, metas[i].IsSigner = r.to, metas[i].IsSigner || r.sign
						found++
					}
				}
				if found == 0 {
					t.Fatalf("%s %s: no route account is %s", venue, name, r.from)
				}
			}
			if fixed != nil {
				fixed(metas, data)
			}
			time.Sleep(time.Second) // public RPC endpoints rate-limit bursts
			out, err := Simulate(ctx, c, user, []solana.Instruction{solana.NewInstruction(jupiter.ProgramID, metas, data)}, watch)
			if err != nil {
				t.Fatalf("%s %s: %v", venue, name, err)
			}
			if name == "as returned" {
				if out.Err != nil {
					t.Fatalf("%s: the untampered swap failed: %v", venue, out.Err)
				}
				return
			}
			if out.Err == nil {
				t.Errorf("%s %s: succeeded; balances %+v", venue, name, out.Watched)
				return
			}
			t.Logf("%s %s: %v", venue, name, out.Err)
		}
		zeroQuote := func(_ []*solana.AccountMeta, data []byte) {
			binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], 0)
		}
		asUser := []rewrite{{authority, user, true}, {programSource, userOther, false}}

		simulate("as returned", nil, nil)
		simulate("foreign output", []rewrite{{programDestination, foreign, false}}, zeroQuote)
		simulate("user as venue authority, own output", asUser, zeroQuote)
		simulate("user as venue authority, foreign output", append(asUser, rewrite{programDestination, foreign, false}), zeroQuote)
		simulate("authority slot j, accounts of i", nil, func(metas []*solana.AccountMeta, data []byte) {
			zeroQuote(metas, data)
			metas[0].PublicKey = jupiter.ProgramAuthority(other)
		})
		simulate("authority slot and id j, accounts of i", nil, func(metas []*solana.AccountMeta, data []byte) {
			zeroQuote(metas, data)
			metas[0].PublicKey, data[jupiter.V2IDOffset] = jupiter.ProgramAuthority(other), other
		})
		simulate("authority j everywhere, accounts of i", []rewrite{{authority, jupiter.ProgramAuthority(other), false}}, func(metas []*solana.AccountMeta, data []byte) {
			zeroQuote(metas, data)
			metas[0].PublicKey, data[jupiter.V2IDOffset] = jupiter.ProgramAuthority(other), other
		})
	}
}
