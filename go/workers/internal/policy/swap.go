package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// SwapSlippageBPS is the slippage a Swap quotes with and the most its policy
// admits.
const SwapSlippageBPS = 50

// swapMaxAccounts keeps the route small enough to run inside a Squads
// execution in one transaction.
const swapMaxAccounts = 32

// Swap is the product of one smart-account vault swapping amount of from into
// to through Jupiter's shared_accounts_route_v2, between its own associated
// token accounts. Its one op is the swap the live /swap-instructions V2 call
// returns for the vault; payer creates the vault's destination account,
// idempotently, outside the policy.
//
// The policy pins the vault, its two token accounts, both mints and their
// token programs, and Jupiter's program authorities with their token
// accounts; it bounds slippage and admits no fee. It sets no spending limit and
// no in_amount cap or quoted_out floor: the swap's output can only reach the
// vault's destination account (jupiter.SharedAccountsRouteV2Allowed).
func Swap(c *chain.Client, quotes *jupiter.Client, settings solana.PublicKey, vaultIndex uint8, from, to, payer solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		if from == to {
			return Product{}, errors.New("swap from and to are the same mint")
		}
		vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
		if err != nil {
			return Product{}, err
		}
		_, mints, err := c.Accounts(ctx, []solana.PublicKey{from, to}, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		for i, mint := range []solana.PublicKey{from, to} {
			if mints[i] == nil {
				return Product{}, fmt.Errorf("mint %s does not exist", mint)
			}
		}
		fromProgram, toProgram := mints[0].Owner, mints[1].Owner
		allowed, err := jupiter.OwnSwapAllowed(vault, from, fromProgram, to, toProgram)
		if err != nil {
			return Product{}, err
		}
		quote, err := quotes.Quote(ctx, jupiter.QuoteRequest{InputMint: from.String(), OutputMint: to.String(), Amount: amount,
			SlippageBPS: SwapSlippageBPS, MaxAccounts: swapMaxAccounts, InstructionVersion: "V2"})
		if err != nil {
			return Product{}, err
		}
		response, err := quotes.SwapInstructions(ctx, quote, vault, true)
		if err != nil {
			return Product{}, err
		}
		inner, err := response.SwapInstruction.Decode()
		if err != nil {
			return Product{}, err
		}
		_, args, err := jupiter.DecodeSharedAccountsRouteV2(inner)
		if err != nil {
			return Product{}, err
		}
		return Product{Name: fmt.Sprintf("swap:%s:%s", from, to), VaultIndex: vaultIndex, Ops: []Op{{
			Name:    fmt.Sprintf("swap %d %s -> %s (quoted %d, %d steps, authority %d)", args.InAmount, from, to, args.QuotedOutAmount, args.Steps, args.ID),
			Allowed: jupiter.SharedAccountsRouteV2Allowed(allowed, SwapSlippageBPS),
			Inner:   inner,
			Before:  []solana.Instruction{spl.CreateIdempotentATA(payer, vault, to, toProgram)},
		}}}, nil
	}
}
