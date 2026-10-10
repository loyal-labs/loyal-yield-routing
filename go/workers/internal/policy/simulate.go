package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Simulation is one transaction as the cluster ran it in simulation. Err is
// the transaction's error, nil when it succeeded.
type Simulation struct {
	Slot, Units uint64
	Err         any
	Logs        []string
	Watched     []Watched
}

// Watched is one watched token account's balance before and after a
// simulation, in raw units; an absent account holds zero.
type Watched struct {
	Account       solana.PublicKey
	Before, After uint64
}

// Simulate runs ixs on the real chain as one transaction paid by payer, with
// no signatures checked and the latest blockhash, and reports what it did to
// the watched token accounts. It answers "can this instruction move funds
// anywhere its policy leaves free?": take a real instruction, put a foreign
// account in a free slot, simulate it, read the deltas. Nothing is signed or
// sent.
func Simulate(ctx context.Context, c *chain.Client, payer solana.PublicKey, ixs []solana.Instruction, watch []solana.PublicKey) (Simulation, error) {
	if len(watch) == 0 {
		return Simulation{}, errors.New("simulate watches no accounts")
	}
	tx, err := transaction(ixs, solana.Hash{}, payer)
	if err != nil {
		return Simulation{}, err
	}
	tx.Signatures = make([]solana.Signature, tx.Message.Header.NumRequiredSignatures)
	wire, err := tx.MarshalBinary()
	if err != nil {
		return Simulation{}, err
	}
	slot, before, err := c.Accounts(ctx, watch, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return Simulation{}, err
	}
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{ReplaceRecentBlockhash: true, Commitment: rpc.CommitmentConfirmed,
		MinContextSlot: &slot, Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: watch}})
	var failed *chain.SimulationError
	if errors.As(err, &failed) {
		out := Simulation{Slot: failed.Slot, Err: failed.Err, Logs: failed.Logs}
		for i, key := range watch {
			amount, err := tokenAmount(before[i])
			if err != nil {
				return Simulation{}, err
			}
			out.Watched = append(out.Watched, Watched{Account: key, Before: amount, After: amount})
		}
		return out, nil
	}
	if err != nil {
		return Simulation{}, err
	}
	out := Simulation{Slot: simulated.Slot, Units: simulated.Units, Logs: simulated.Logs}
	for i, key := range watch {
		pre, err := tokenAmount(before[i])
		if err != nil {
			return Simulation{}, err
		}
		post, err := tokenAmount(simulated.Accounts[i])
		if err != nil {
			return Simulation{}, err
		}
		out.Watched = append(out.Watched, Watched{Account: key, Before: pre, After: post})
	}
	return out, nil
}

func tokenAmount(account *chain.Account) (uint64, error) {
	if account == nil {
		return 0, nil
	}
	decoded, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return 0, fmt.Errorf("watched account %s: %w", account.Key, err)
	}
	return decoded.Amount, nil
}
