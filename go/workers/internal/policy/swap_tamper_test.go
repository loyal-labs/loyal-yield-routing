package policy

import (
	"context"
	"encoding/binary"
	"net/http"
	"os"
	"slices"
	"strings"
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
	quotes, err := jupiter.NewClient(jupiter.KeyedBase, "", &http.Client{Timeout: 20 * time.Second})
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

		simulate := func(name string, route []rewrite, fixed func(metas []*solana.AccountMeta, data []byte)) {
			tamper(t, ctx, c, venue+" "+name, user, nil, accounts, returned, watch, route, fixed)
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

// rewrite swaps every route account (from 12 on) that is from for to, and
// makes it a signer when sign is set.
type rewrite struct {
	from, to solana.PublicKey
	sign     bool
}

func zeroQuote(_ []*solana.AccountMeta, data []byte) {
	binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], 0)
}

// tamper simulates before plus a rewritten copy of the swap paid by payer. The
// case named "as returned" must succeed; every other case must fail, and a
// route rewrite that finds nothing to rewrite fails the test.
func tamper(t *testing.T, ctx context.Context, c *chain.Client, name string, payer solana.PublicKey, before []solana.Instruction,
	accounts []*solana.AccountMeta, returned []byte, watch []solana.PublicKey, route []rewrite, fixed func(metas []*solana.AccountMeta, data []byte)) {
	t.Helper()
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
			t.Fatalf("%s: no route account is %s", name, r.from)
		}
	}
	if fixed != nil {
		fixed(metas, data)
	}
	time.Sleep(time.Second) // public RPC endpoints rate-limit bursts
	out, err := Simulate(ctx, c, payer, append(append([]solana.Instruction(nil), before...), solana.NewInstruction(jupiter.ProgramID, metas, data)), watch)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if strings.HasSuffix(name, "as returned") {
		if out.Err != nil {
			t.Fatalf("%s: the untampered swap failed: %v %v", name, out.Err, out.Logs)
		}
		t.Logf("%s: %+v", name, out.Watched)
		return
	}
	if out.Err == nil {
		t.Errorf("%s: succeeded; balances %+v", name, out.Watched)
		return
	}
	t.Logf("%s: %v", name, out.Err)
}

// PYUSD is the one Backyard mint the swap API routes through the user's own
// token account instead of a program authority's: the program-side slot of a
// PYUSD input or output holds the user's PYUSD account, and the venue reads or
// pays it there. The same claim holds for it on mainnet: no rewrite of the
// route moves funds anywhere but the user's destination. The PYUSD holder is
// a Whirlpool PDA with two funded PYUSD accounts; signatures are not checked
// in simulation, so it stands in as the signing user.
//
// Cases, PYUSD into USDC (the user's PYUSD account is the program source):
//   - foreign output: the venue's output account (the authority's USDC ATA) is
//     another wallet's USDC account;
//   - foreign program-side account: the route's reads of the user's PYUSD
//     account, and with them the program source slot, are a foreign funded
//     PYUSD account;
//   - user's other account: the route's reads of the user's PYUSD account are
//     its other funded PYUSD account, and, when the route names the program
//     authority, also with every such slot the user, signing.
//
// USDC into PYUSD (the user's PYUSD account is the program destination):
//   - foreign output: the route's writes to the user's PYUSD account are a
//     foreign PYUSD account, with and without the program destination slot.
//
// It needs TEST_SOLANA_RPC_URL and calls the keyless Jupiter API.
func TestSharedRouteV2PYUSDTamperOnMainnet(t *testing.T) {
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
	quotes, err := jupiter.NewClient(jupiter.KeyedBase, "", &http.Client{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	key := solana.MustPublicKeyFromBase58
	usdc, pyusd := key("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), key("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo")
	payer := key("5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9")            // a wallet with SOL and USDC
	holder := key("EYMwW3Y7k37G9e3Hfks7KCFv8r5Sict7fBCYLz3vvusQ")           // owns two funded PYUSD accounts
	holderPYUSD := key("HupYrHwSbCU95VH2Q8SnpaWxwUhPPdqezAX4pvGssn3X")      // its larger one
	holderOtherPYUSD := key("5XcTbEiGhRTHJtXLiNjXHFaBriu3S5HfZ3BU5paFRLbW") // its smaller one
	foreignPYUSD := key("EeF6oBy6AQiBJoRx5xiRNxa6cmpQE3ayVagj28QFZuyg")     // another owner's funded PYUSD account
	fetch := func(from, to, user solana.PublicKey) ([]*solana.AccountMeta, []byte) {
		quote, err := quotes.Quote(ctx, jupiter.QuoteRequest{InputMint: from.String(), OutputMint: to.String(), Amount: 1_000_000,
			SlippageBPS: SwapSlippageBPS, MaxAccounts: swapMaxAccounts, InstructionVersion: "V2"})
		if err != nil {
			t.Fatal(err)
		}
		response, err := quotes.SwapInstructions(ctx, quote, user, true)
		if err != nil {
			t.Fatal(err)
		}
		ix, err := response.SwapInstruction.Decode()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := ix.Data()
		return ix.Accounts(), data
	}

	// PYUSD into USDC, from the holder's larger PYUSD account.
	holderATA, _ := spl.AssociatedTokenAddress(holder, pyusd, solana.Token2022ProgramID)
	holderUSDC, _ := spl.AssociatedTokenAddress(holder, usdc, solana.TokenProgramID)
	foreignUSDC, _ := spl.AssociatedTokenAddress(payer, usdc, solana.TokenProgramID) // foreign to the holder
	accounts, data := fetch(pyusd, usdc, holder)
	if accounts[2].PublicKey != holderATA || accounts[3].PublicKey != holderATA {
		t.Fatalf("the API no longer routes PYUSD through the user's own account: %s %s", accounts[2].PublicKey, accounts[3].PublicKey)
	}
	for _, meta := range accounts {
		if meta.PublicKey == holderATA {
			meta.PublicKey = holderPYUSD
		}
	}
	authority, programDestination := accounts[0].PublicKey, accounts[4].PublicKey
	before := []solana.Instruction{spl.CreateIdempotentATA(payer, holder, usdc, solana.TokenProgramID)}
	watch := []solana.PublicKey{holderPYUSD, holderOtherPYUSD, foreignPYUSD, holderUSDC, foreignUSDC, programDestination}
	run := func(name string, route []rewrite, fixed func([]*solana.AccountMeta, []byte)) {
		tamper(t, ctx, c, "PYUSD->USDC "+name, payer, before, accounts, data, watch, route, fixed)
	}
	run("as returned", nil, nil)
	run("foreign output", []rewrite{{programDestination, foreignUSDC, false}}, zeroQuote)
	run("foreign program-side account, route only", []rewrite{{holderPYUSD, foreignPYUSD, false}}, zeroQuote)
	run("foreign program-side account", []rewrite{{holderPYUSD, foreignPYUSD, false}}, func(metas []*solana.AccountMeta, data []byte) {
		zeroQuote(metas, data)
		metas[3].PublicKey = foreignPYUSD
	})
	run("user's other account as venue input", []rewrite{{holderPYUSD, holderOtherPYUSD, false}}, zeroQuote)
	if slices.ContainsFunc(accounts[12:], func(m *solana.AccountMeta) bool { return m.PublicKey == authority }) {
		run("user as venue authority, other account", []rewrite{{holderPYUSD, holderOtherPYUSD, false}, {authority, holder, true}}, zeroQuote)
	}

	// USDC into PYUSD, for the payer, which holds USDC.
	payerPYUSD, _ := spl.AssociatedTokenAddress(payer, pyusd, solana.Token2022ProgramID)
	payerUSDC, _ := spl.AssociatedTokenAddress(payer, usdc, solana.TokenProgramID)
	accounts, data = fetch(usdc, pyusd, payer)
	if accounts[4].PublicKey != payerPYUSD || accounts[5].PublicKey != payerPYUSD {
		t.Fatalf("the API no longer routes PYUSD out through the user's own account: %s %s", accounts[4].PublicKey, accounts[5].PublicKey)
	}
	before = []solana.Instruction{spl.CreateIdempotentATA(payer, payer, pyusd, solana.Token2022ProgramID)}
	watch = []solana.PublicKey{payerUSDC, payerPYUSD, foreignPYUSD}
	run = func(name string, route []rewrite, fixed func([]*solana.AccountMeta, []byte)) {
		tamper(t, ctx, c, "USDC->PYUSD "+name, payer, before, accounts, data, watch, route, fixed)
	}
	run("as returned", nil, nil)
	run("foreign output, route only", []rewrite{{payerPYUSD, foreignPYUSD, false}}, zeroQuote)
	run("foreign output", []rewrite{{payerPYUSD, foreignPYUSD, false}}, func(metas []*solana.AccountMeta, data []byte) {
		zeroQuote(metas, data)
		metas[4].PublicKey = foreignPYUSD
	})
}
