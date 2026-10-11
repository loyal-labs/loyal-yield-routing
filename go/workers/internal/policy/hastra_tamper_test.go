package policy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// The safety claim of the hastra constraints, on mainnet: with the program's
// own accounts pinned, a conversion moves funds only between the signer's own
// token accounts and the program's pool. Each conversion is simulated as
// built for a real holder (it must succeed and move exactly what the stake
// price quotes), then with the signer's destination, and then its source,
// swapped for another owner's funded account of the same mint (each must
// fail, and a failed transaction moves nothing). Signatures are not checked
// in simulation, so the holder stands in as the signer.
//
// It also checks the two pools the package names as constants against the
// configs that name them on chain.
//
// It needs TEST_SOLANA_RPC_URL.
func TestHastraTamperOnMainnet(t *testing.T) {
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
	key := solana.MustPublicKeyFromBase58
	holder := key("HGpYjXi9nrFxJikYyFpvRfwk2XstrvhbUpYyd9guZvzs")       // holds wYLDS and AUTO in its ATAs, and SOL
	usdcHolder := key("5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9")   // holds USDC in its ATA, and SOL
	foreignWYLDS := key("5ZWxEY76gtz46trW7wqCPVqJoJVbQXd7WZ5Bzif9EArH") // another owner's funded wYLDS account
	foreignAUTO := key("FtBzhc7HSLCUV7D6fFLysBeTDE2bzHS6SGpgCeRFzTi9")  // another owner's funded AUTO account
	foreignUSDC := key("4Ur2yHk6wW7jXBuah6jvfMPGfFnV9jRCG6mBjTbpZKho")  // the holder's USDC account, foreign to usdcHolder

	_, configs, err := c.Accounts(ctx, []solana.PublicKey{hastra.StakeVaultAccountConfig, hastra.MintVaultAccountConfig, hastra.StakePriceConfig},
		rpc.CommitmentConfirmed, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, pool := range []solana.PublicKey{hastra.StakePool, hastra.MintPool} {
		if configs[i] == nil || len(configs[i].Data) < 40 || solana.PublicKeyFromBytes(configs[i].Data[8:40]) != pool {
			t.Fatalf("the vault token account config %d does not name the pool %s", i, pool)
		}
	}
	price, err := hastra.DecodeStakePrice(configs[2])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stake price %s / %d at %d (max staleness %d s)", price.Price, price.Scale, price.Timestamp, price.MaxStaleness)

	stake, err := hastra.OwnStake(holder)
	if err != nil {
		t.Fatal(err)
	}
	mint, err := hastra.OwnMint(usdcHolder)
	if err != nil {
		t.Fatal(err)
	}
	const amount = 100_000 // 0.1 wYLDS or AUTO; the foreign accounts hold more
	const usdcAmount = 1_000
	autoOut, err := price.AUTOForWYLDS(amount)
	if err != nil {
		t.Fatal(err)
	}
	wyldsOut, err := price.WYLDSForAUTO(amount)
	if err != nil {
		t.Fatal(err)
	}

	type tamperCase struct {
		name     string
		payer    solana.PublicKey
		before   []solana.Instruction
		ix       *solana.GenericInstruction
		watch    []solana.PublicKey
		delta    map[solana.PublicKey]int64 // as built: each watched account's exact change
		swapped  int                        // the slot a tampered case rewrites, -1 as built
		with     solana.PublicKey
		slotRole string
	}
	stakeWatch := []solana.PublicKey{stake.UserWYLDS, stake.UserAUTO, foreignWYLDS, foreignAUTO, hastra.StakePool}
	mintWatch := []solana.PublicKey{mint.UserUSDC, mint.UserWYLDS, foreignUSDC, foreignWYLDS, hastra.MintPool}
	createWYLDS := []solana.Instruction{spl.CreateIdempotentATA(usdcHolder, usdcHolder, hastra.WYLDSMint, solana.TokenProgramID)}
	deposit, redeem, mintDeposit := hastra.StakeDeposit(stake, amount), hastra.StakeRedeem(stake, amount), hastra.MintDeposit(mint, usdcAmount)
	cases := []tamperCase{
		{"stake deposit as built", holder, nil, deposit, stakeWatch,
			map[solana.PublicKey]int64{stake.UserWYLDS: -amount, stake.UserAUTO: int64(autoOut), hastra.StakePool: amount}, -1, solana.PublicKey{}, ""},
		{"stake deposit, foreign destination", holder, nil, deposit, stakeWatch, nil, 9, foreignAUTO, "destination"},
		{"stake deposit, foreign source", holder, nil, deposit, stakeWatch, nil, 8, foreignWYLDS, "source"},
		{"stake redeem as built", holder, nil, redeem, stakeWatch,
			map[solana.PublicKey]int64{stake.UserAUTO: -amount, stake.UserWYLDS: int64(wyldsOut), hastra.StakePool: -int64(wyldsOut)}, -1, solana.PublicKey{}, ""},
		{"stake redeem, foreign destination", holder, nil, redeem, stakeWatch, nil, 6, foreignWYLDS, "destination"},
		{"stake redeem, foreign source", holder, nil, redeem, stakeWatch, nil, 7, foreignAUTO, "source"},
		{"mint deposit as built", usdcHolder, createWYLDS, mintDeposit, mintWatch,
			map[solana.PublicKey]int64{mint.UserUSDC: -usdcAmount, mint.UserWYLDS: usdcAmount, hastra.MintPool: usdcAmount}, -1, solana.PublicKey{}, ""},
		{"mint deposit, foreign destination", usdcHolder, createWYLDS, mintDeposit, mintWatch, nil, 7, foreignWYLDS, "destination"},
		{"mint deposit, foreign source", usdcHolder, createWYLDS, mintDeposit, mintWatch, nil, 6, foreignUSDC, "source"},
	}
	for _, tc := range cases {
		metas := tc.ix.Accounts()
		copied := make([]*solana.AccountMeta, len(metas))
		for i, meta := range metas {
			m := *meta
			copied[i] = &m
		}
		if tc.swapped >= 0 {
			copied[tc.swapped].PublicKey = tc.with
		}
		data, _ := tc.ix.Data()
		time.Sleep(time.Second) // public RPC endpoints rate-limit bursts
		out, err := Simulate(ctx, c, tc.payer, append(append([]solana.Instruction(nil), tc.before...), solana.NewInstruction(tc.ix.ProgramID(), copied, data)), tc.watch)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.swapped < 0 {
			if out.Err != nil {
				t.Fatalf("%s failed: %v %v", tc.name, out.Err, out.Logs)
			}
			for _, w := range out.Watched {
				if got := int64(w.After) - int64(w.Before); got != tc.delta[w.Account] {
					t.Errorf("%s: %s moved %d, want %d", tc.name, w.Account, got, tc.delta[w.Account])
				}
			}
			t.Logf("%s: %+v", tc.name, out.Watched)
			continue
		}
		if out.Err == nil {
			t.Errorf("%s (slot %d %s): succeeded; balances %+v", tc.name, tc.swapped, tc.slotRole, out.Watched)
			continue
		}
		// Each program refuses a user account the signer does not own with
		// its InvalidTokenOwner: 6029 in vault-stake, 6020 in vault-mint.
		owner := map[solana.PublicKey]string{hastra.StakeProgramID: "Custom:6029", hastra.MintProgramID: "Custom:6020"}[tc.ix.ProgramID()]
		if !strings.Contains(fmt.Sprint(out.Err), owner) {
			t.Errorf("%s (slot %d): failed with %v, not InvalidTokenOwner (%s)", tc.name, tc.swapped, out.Err, lastLog(out.Logs))
			continue
		}
		t.Logf("%s (slot %d): InvalidTokenOwner %v", tc.name, tc.swapped, out.Err)
	}
}

// lastLog is the program log line that names the error, if any.
func lastLog(logs []string) string {
	for i := len(logs) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(logs[i]), "error") {
			return logs[i]
		}
	}
	return ""
}
