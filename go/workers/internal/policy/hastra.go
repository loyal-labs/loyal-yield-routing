package policy

import (
	"context"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// HastraAmounts are a Hastra product's op amounts, raw: USDC into wYLDS
// (mint deposit), wYLDS into AUTO (stake deposit) and AUTO into wYLDS (stake
// redeem).
type HastraAmounts struct {
	MintDeposit, StakeDeposit, StakeRedeem uint64
}

// The hastra ops in policy order: a leg's value is its constraint index.
const (
	hastraStakeDeposit = iota
	hastraStakeRedeem
	hastraMintDeposit
	hastraLegs
)

// hastraConstraints are a vault's Hastra conversions between its own
// associated token accounts, in policy order: every account pinned (the
// vault, its three ATAs and every program account), the amount free by the
// owner's no-limits choice for conversions.
func hastraConstraints(vault solana.PublicKey) ([hastraLegs]squads.InstructionConstraintView, hastra.StakeAccounts, hastra.MintAccounts, error) {
	var out [hastraLegs]squads.InstructionConstraintView
	stake, err := hastra.OwnStake(vault)
	if err != nil {
		return out, hastra.StakeAccounts{}, hastra.MintAccounts{}, err
	}
	mint, err := hastra.OwnMint(vault)
	if err != nil {
		return out, hastra.StakeAccounts{}, hastra.MintAccounts{}, err
	}
	out[hastraStakeDeposit] = hastra.StakeDepositAllowed(hastra.PinStake(stake))
	out[hastraStakeRedeem] = hastra.StakeRedeemAllowed(hastra.PinStake(stake))
	out[hastraMintDeposit] = hastra.MintDepositAllowed(hastra.PinMint(mint))
	return out, stake, mint, nil
}

// Hastra is the product of one smart-account vault converting through Hastra
// between its own associated token accounts: wYLDS into AUTO, AUTO into
// wYLDS, and USDC into wYLDS. payer creates each op's destination account,
// idempotently, outside the policy. Each op's name says what the stake price
// quotes for it.
func Hastra(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, payer solana.PublicKey, amounts HastraAmounts) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
		if err != nil {
			return Product{}, err
		}
		constraints, stake, mint, err := hastraConstraints(vault)
		if err != nil {
			return Product{}, err
		}
		_, read, err := c.Accounts(ctx, []solana.PublicKey{hastra.StakePriceConfig}, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		price, err := hastra.DecodeStakePrice(read[0])
		if err != nil {
			return Product{}, err
		}
		quote := func(f func(uint64) (uint64, error), amount uint64) string {
			out, err := f(amount)
			if err != nil {
				return err.Error()
			}
			return fmt.Sprint(out)
		}
		create := func(mint solana.PublicKey) []solana.Instruction {
			return []solana.Instruction{spl.CreateIdempotentATA(payer, vault, mint, solana.TokenProgramID)}
		}
		ops := make([]Op, hastraLegs)
		ops[hastraStakeDeposit] = Op{Name: fmt.Sprintf("stake deposit %d wYLDS for %s AUTO", amounts.StakeDeposit, quote(price.AUTOForWYLDS, amounts.StakeDeposit)),
			Inner: hastra.StakeDeposit(stake, amounts.StakeDeposit), Before: create(hastra.AUTOMint)}
		ops[hastraStakeRedeem] = Op{Name: fmt.Sprintf("stake redeem %d AUTO for %s wYLDS", amounts.StakeRedeem, quote(price.WYLDSForAUTO, amounts.StakeRedeem)),
			Inner: hastra.StakeRedeem(stake, amounts.StakeRedeem), Before: create(hastra.WYLDSMint)}
		ops[hastraMintDeposit] = Op{Name: fmt.Sprintf("mint deposit %d USDC for as much wYLDS", amounts.MintDeposit),
			Inner: hastra.MintDeposit(mint, amounts.MintDeposit), Before: create(hastra.WYLDSMint)}
		for i := range ops {
			ops[i].Allowed = constraints[i]
		}
		return Product{Name: "hastra", VaultIndex: vaultIndex, Ops: ops}, nil
	}
}
