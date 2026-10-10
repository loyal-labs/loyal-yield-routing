// Package policy holds the products a smart account runs through one Squads
// policy, and the two acts on them: Apply installs a product's policy on a
// Settings, Check runs the product's instructions through the installed policy
// on the real chain.
//
// A product's policy is its ops' constraints in order, so an op's position is
// the constraint index it executes under. Each op's constraint comes from the
// same program builder as its instruction (kamino.DepositV2 and
// kamino.DepositV2Allowed read one account order), so the two cannot drift.
// The installed policy account is the only authorization: there is no manifest,
// and the policy to execute through is found on chain by equality.
package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// An Op is one instruction a smart account sends through its product's policy
// and the constraint that admits it.
type Op struct {
	Name    string
	Allowed squads.InstructionConstraintView
	Inner   solana.Instruction
	// Before are top-level instructions the op needs ahead of it in the same
	// transaction: KLend refreshes, or setup a payer funds outside the policy.
	Before []solana.Instruction
	// Done means the op's effect already exists on chain (an init whose
	// account is present), so Check does not send it.
	Done bool
}

// A Product is a smart account's ops in policy order.
type Product struct {
	Name       string
	VaultIndex uint8
	Ops        []Op
}

// Policy is the product's policy: its ops' constraints on its vault, with no
// spending limits (PolicyApply installs none).
func (p Product) Policy() squads.Policy {
	out := squads.Policy{VaultIndex: p.VaultIndex, Constraints: make([]squads.InstructionConstraintView, len(p.Ops))}
	for i, op := range p.Ops {
		out.Constraints[i] = op.Allowed
	}
	return out
}

// Build reads the chain state a product's ops need, at minSlot or later, and
// returns them, so Check can rebuild from the state each landed op left.
type Build func(ctx context.Context, minSlot uint64) (Product, error)

// The transaction budget every policy transaction asks for. v1 carries it in
// the message and an unset limit is zero, so each is explicit; the heap frame
// lets Squads parse a many-constraint policy.
const (
	computeUnits        = 1_400_000
	heapBytes           = 256 << 10
	loadedAccountsBytes = 64 << 20
	priorityFeeLamports = 10_000
)

// Apply installs build's product policy on settings, delegated to delegate,
// removing the replaced policies in the same settings transaction; when the
// policy is already installed it only removes them. signer is the Settings'
// one signer and pays. Without send it only simulates.
func Apply(ctx context.Context, c *chain.Client, out io.Writer, settings solana.PublicKey, build Build, signer solana.PrivateKey, delegate solana.PublicKey, replace []solana.PublicKey, send bool) error {
	product, err := build(ctx, 0)
	if err != nil {
		return err
	}
	want := product.Policy()
	constraints := want.Constraints
	installed, err := squads.Policies(ctx, c, settings, 0)
	if err != nil {
		return err
	}
	found, matches := squads.FindPolicy(installed, settings, delegate, want)
	if matches > 1 {
		return fmt.Errorf("%s is installed %d times on %s", product.Name, matches, settings)
	}
	if account := found.Account; matches == 1 {
		fmt.Fprintf(out, "%s is already installed on %s as %s\n", product.Name, settings, account)
		if slices.Contains(replace, account) {
			return fmt.Errorf("--replace names the installed %s policy %s", product.Name, account)
		}
		if len(replace) == 0 {
			return nil
		}
		constraints = nil
	}
	_, accounts, err := c.Accounts(ctx, []solana.PublicKey{settings}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return err
	}
	state, err := squads.DecodeSettings(accounts[0])
	if err != nil {
		return err
	}
	apply := squads.PolicyApply{Settings: settings, RentPayer: signer.PublicKey(), Signer: signer.PublicKey(), Delegate: delegate,
		Seed: squads.NextPolicySeed(state), VaultIndex: want.VaultIndex, Constraints: constraints, Replace: replace}
	ix, err := apply.Instruction()
	if err != nil {
		return err
	}
	policy, err := apply.Policy()
	if err != nil {
		return err
	}
	if constraints == nil {
		fmt.Fprintf(out, "apply %s: remove %v\n", product.Name, replace)
	} else {
		fmt.Fprintf(out, "apply %s: %d constraints at seed %d -> %s, remove %v\n", product.Name, len(constraints), apply.Seed, policy, replace)
	}
	_, err = run(ctx, c, out, signer, []solana.Instruction{generic(ix)}, send)
	return err
}

// Remove removes policies from settings in one settings transaction, signed
// and paid by signer, the Settings' one signer. Without send it only
// simulates.
func Remove(ctx context.Context, c *chain.Client, out io.Writer, settings solana.PublicKey, signer solana.PrivateKey, policies []solana.PublicKey, send bool) error {
	ix, err := squads.PolicyApply{Settings: settings, RentPayer: signer.PublicKey(), Signer: signer.PublicKey(), Replace: policies}.Instruction()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "remove %d policies from %s: %v\n", len(policies), settings, policies)
	_, err = run(ctx, c, out, signer, []solana.Instruction{generic(ix)}, send)
	return err
}

// Check sends each op of build's product (or only op number only, when it is
// not negative) through its installed policy on settings, signed and paid by
// delegate: simulated, and with send landed one by one, rebuilding the product
// from the chain each landed op left. It stops at the first failure.
func Check(ctx context.Context, c *chain.Client, out io.Writer, settings solana.PublicKey, build Build, delegate solana.PrivateKey, only int, send bool) error {
	product, err := build(ctx, 0)
	if err != nil {
		return err
	}
	installed, err := squads.Policies(ctx, c, settings, 0)
	if err != nil {
		return err
	}
	found, matches := squads.FindPolicy(installed, settings, delegate.PublicKey(), product.Policy())
	if matches != 1 {
		return fmt.Errorf("%s policy is installed %d times on %s", product.Name, matches, settings)
	}
	policy := found.Account
	var landed uint64
	for i := range product.Ops {
		if only >= 0 && i != only {
			continue
		}
		if landed > 0 {
			if product, err = build(ctx, landed); err != nil {
				return err
			}
		}
		op := product.Ops[i]
		if op.Done {
			fmt.Fprintf(out, "%d %s: done\n", i, op.Name)
			continue
		}
		inner, err := squadsInstruction(op.Inner)
		if err != nil {
			return err
		}
		execute, err := squads.ExecuteTransactionSyncV2(squads.ExecuteSync{Policy: policy, Signer: delegate.PublicKey(), AccountIndex: product.VaultIndex,
			ConstraintIndexes: []byte{byte(i)}, Inner: []squads.Instruction{inner}})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%d %s:\n", i, op.Name)
		if landed, err = run(ctx, c, out, delegate, append(append([]solana.Instruction(nil), op.Before...), generic(execute)), send); err != nil {
			return fmt.Errorf("%s: %w", op.Name, err)
		}
	}
	return nil
}

// run compiles one v1 transaction paid and signed by payer, simulates it with
// signature checks, and with send lands it at confirmed, prints its token
// deltas and returns its slot.
func run(ctx context.Context, c *chain.Client, out io.Writer, payer solana.PrivateKey, ixs []solana.Instruction, send bool) (uint64, error) {
	hash, lastValid, _, err := c.Blockhash(ctx, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return 0, err
	}
	tx, err := solana.NewTransaction(ixs, hash, solana.TransactionPayer(payer.PublicKey()), solana.TransactionV1Config(
		solana.TransactionConfig{}.WithComputeUnitLimit(computeUnits).WithHeapSize(heapBytes).
			WithLoadedAccountsDataSizeLimit(loadedAccountsBytes).WithPriorityFee(priorityFeeLamports)))
	if err != nil {
		return 0, err
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer.PublicKey() {
			return &payer
		}
		return nil
	}); err != nil {
		return 0, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return 0, err
	}
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{SigVerify: true, Commitment: rpc.CommitmentConfirmed})
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "  simulated: %d bytes, %d CU, slot %d\n", len(wire), simulated.Units, simulated.Slot)
	if !send {
		return 0, nil
	}
	signature := tx.Signatures[0]
	fmt.Fprintf(out, "  sending %s\n", signature)
	outcome, err := chain.Land(ctx, c, chain.Attempt{Wire: wire, Signature: signature.String(), LastValidBlockHeight: lastValid, Required: chain.Confirmed},
		2*time.Second, func(context.Context) error { return nil })
	if err != nil {
		return 0, err
	}
	switch outcome.Kind {
	case chain.Landed:
	case chain.Failed:
		return 0, fmt.Errorf("transaction %s failed on chain: %s", signature, outcome.Err)
	default:
		return 0, fmt.Errorf("transaction %s expired unlanded", signature)
	}
	receipt, err := c.Receipt(ctx, signature, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "  landed slot %d, fee %d lamports\n", receipt.Slot, receipt.Fee)
	for account, post := range receipt.Post {
		if pre := receipt.Pre[account]; pre.Amount != post.Amount {
			fmt.Fprintf(out, "  %s %s: %d -> %d\n", account, post.Mint, pre.Amount, post.Amount)
		}
	}
	return receipt.Slot, nil
}

func generic(ix squads.Instruction) solana.Instruction {
	metas := make(solana.AccountMetaSlice, len(ix.Accounts))
	for i := range ix.Accounts {
		meta := ix.Accounts[i]
		metas[i] = &meta
	}
	return solana.NewInstruction(ix.ProgramID, metas, ix.Data)
}

func squadsInstruction(ix solana.Instruction) (squads.Instruction, error) {
	data, err := ix.Data()
	if err != nil {
		return squads.Instruction{}, err
	}
	out := squads.Instruction{ProgramID: ix.ProgramID(), Data: data}
	for _, meta := range ix.Accounts() {
		if meta == nil {
			return squads.Instruction{}, errors.New("instruction has a nil account")
		}
		out.Accounts = append(out.Accounts, *meta)
	}
	return out, nil
}
