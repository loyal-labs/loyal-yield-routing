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

const SwapSlippageBPS = 50

// swapMaxAccounts keeps the route small enough to run inside a Squads
// execution in one v1 transaction, which takes no lookup tables.
const swapMaxAccounts = 32

func SwapPolicy(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, from, to solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		product, _, _, err := swapPolicy(ctx, c, settings, vaultIndex, from, to, amount, minSlot)
		return product, err
	}
}

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
