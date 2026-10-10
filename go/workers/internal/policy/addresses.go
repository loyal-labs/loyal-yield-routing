package policy

import (
	"context"
	"fmt"
	"io"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Addresses prints what a lane needs to know about a smart account before it
// writes a policy for it: the vault at vaultIndex, the vault's associated
// token account of each mint with its balance (absent when it does not
// exist), and every policy installed on settings with its delegate and the
// programs its constraints call.
func Addresses(ctx context.Context, c *chain.Client, out io.Writer, settings solana.PublicKey, vaultIndex uint8, mints []solana.PublicKey) error {
	vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "settings %s\nvault %d %s\n", settings, vaultIndex, vault)
	if len(mints) > 0 {
		slot, read, err := c.Accounts(ctx, mints, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return err
		}
		atas := make([]solana.PublicKey, len(mints))
		for i, mint := range mints {
			if read[i] == nil {
				return fmt.Errorf("mint %s does not exist", mint)
			}
			if atas[i], err = spl.AssociatedTokenAddress(vault, mint, read[i].Owner); err != nil {
				return err
			}
		}
		_, held, err := c.Accounts(ctx, atas, rpc.CommitmentConfirmed, slot)
		if err != nil {
			return err
		}
		for i, ata := range atas {
			if held[i] == nil {
				fmt.Fprintf(out, "ata %s %s absent\n", mints[i], ata)
				continue
			}
			account, err := spl.DecodeTokenAccount(held[i])
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "ata %s %s %d\n", mints[i], ata, account.Amount)
		}
	}
	installed, err := squads.Policies(ctx, c, settings, 0)
	if err != nil {
		return err
	}
	for _, policy := range installed {
		if policy.View == nil {
			fmt.Fprintf(out, "policy %s not canonical\n", policy.Account)
			continue
		}
		var programs []solana.PublicKey
		for _, constraint := range policy.View.Payload.Constraints {
			programs = append(programs, constraint.ProgramID)
		}
		fmt.Fprintf(out, "policy %s seed %d delegate %s vault %d, %d spending limits, programs %v\n", policy.Account, policy.View.PolicySeed,
			policy.View.DelegatedSigner, policy.View.Payload.VaultIndex, len(policy.View.Payload.SpendingLimits), programs)
	}
	return nil
}
