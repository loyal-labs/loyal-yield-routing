package policy

import (
	"context"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// SwapSlippageBPS is the slippage a Swap asks the quote for. The policy does
// not bound it.
const SwapSlippageBPS = 50

// swapMaxAccounts keeps the route small enough to run inside a Squads
// execution in one v1 transaction, which takes no lookup tables.
const swapMaxAccounts = 32

// SwapPolicy is the product of one smart-account vault swapping amount of
// from into to through Jupiter's shared_accounts_route_v2, between its own
// associated token accounts, with only its policy: its one op's constraint and
// no instruction. Apply needs nothing more, so it calls no swap API; Check
// takes Swap.
//
// The policy pins the vault, its two token accounts, both mints and their
// token programs, and Jupiter's program authorities with their token
// accounts, and admits no fee. Output can reach only the vault's destination
// account. The price is not bounded: in_amount, quoted_out_amount, slippage
// and the route accounts are free by the owner's no-limits choice for swaps,
// so a holder of the delegate key can sell the vault's whole source balance at
// any price through a pool it controls.
func SwapPolicy(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, from, to solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		product, _, _, err := swapPolicy(ctx, c, settings, vaultIndex, from, to, amount, minSlot)
		return product, err
	}
}

// swapPolicy is SwapPolicy's product, with the vault and the destination
// mint's token program it read.
func swapPolicy(ctx context.Context, c *chain.Client, settings solana.PublicKey, vaultIndex uint8, from, to solana.PublicKey, amount, minSlot uint64) (Product, solana.PublicKey, solana.PublicKey, error) {
	vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
	if err != nil {
		return Product{}, solana.PublicKey{}, solana.PublicKey{}, err
	}
	_, mints, err := c.Accounts(ctx, []solana.PublicKey{from, to}, rpc.CommitmentConfirmed, minSlot)
	if err != nil {
		return Product{}, solana.PublicKey{}, solana.PublicKey{}, err
	}
	for i, mint := range []solana.PublicKey{from, to} {
		if mints[i] == nil {
			return Product{}, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("mint %s does not exist", mint)
		}
	}
	allowed, err := jupiter.OwnSwapAllowed(vault, from, mints[0].Owner, to, mints[1].Owner)
	if err != nil {
		return Product{}, solana.PublicKey{}, solana.PublicKey{}, err
	}
	return Product{Name: fmt.Sprintf("swap:%s:%s", from, to), VaultIndex: vaultIndex, Ops: []Op{{
		Name:    fmt.Sprintf("swap %d %s -> %s", amount, from, to),
		Allowed: jupiter.SharedAccountsRouteV2Allowed(allowed),
	}}}, vault, mints[1].Owner, nil
}

// Swap is SwapPolicy with its op's instruction: the swap a live V2
// /swap-instructions call returns for the vault. payer creates the vault's
// destination account, idempotently, outside the policy; the API's own setup
// instruction would have the vault pay, so it is not sent.
func Swap(c *chain.Client, quotes *jupiter.Client, settings solana.PublicKey, vaultIndex uint8, from, to, payer solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		product, vault, toProgram, err := swapPolicy(ctx, c, settings, vaultIndex, from, to, amount, minSlot)
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
		op := &product.Ops[0]
		op.Inner = inner
		op.Before = []solana.Instruction{spl.CreateIdempotentATA(payer, vault, to, toProgram)}
		return product, nil
	}
}
