package earn

import (
	"context"
	"slices"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type voltrFlowKind string

const (
	voltrDeposit voltrFlowKind = "deposit"
	voltrClaim   voltrFlowKind = "claim"
)

type voltrFlow struct {
	wallet solana.PublicKey
	kind   voltrFlowKind
	amount earnmax.USDCRaw
}

func voltrExecution(info *pb.SubscribeUpdateTransactionInfo, slot uint64) (chain.Execution, bool) {
	meta, message := info.GetMeta(), info.GetTransaction().GetMessage()
	var keys []solana.PublicKey
	for _, group := range [][][]byte{message.GetAccountKeys(), meta.GetLoadedWritableAddresses(), meta.GetLoadedReadonlyAddresses()} {
		for _, key := range group {
			keys = append(keys, solana.PublicKeyFromBytes(key))
		}
	}
	if !slices.Contains(keys, earnmax.Vault) {
		return chain.Execution{}, false
	}
	widen := func(indexes []byte) []uint16 {
		out := make([]uint16, len(indexes))
		for i, index := range indexes {
			out[i] = uint16(index)
		}
		return out
	}
	tx := &solana.Transaction{}
	for _, ix := range message.GetInstructions() {
		tx.Message.Instructions = append(tx.Message.Instructions, solana.CompiledInstruction{
			ProgramIDIndex: uint16(ix.GetProgramIdIndex()), Accounts: widen(ix.GetAccounts()), Data: ix.GetData()})
	}
	var inner []rpc.InnerInstruction
	for _, group := range meta.GetInnerInstructions() {
		calls := rpc.InnerInstruction{Index: uint16(group.GetIndex())}
		for _, ix := range group.GetInstructions() {
			calls.Instructions = append(calls.Instructions, rpc.CompiledInstruction{ProgramIDIndex: uint16(ix.GetProgramIdIndex()),
				Accounts: widen(ix.GetAccounts()), Data: ix.GetData(), StackHeight: uint16(ix.GetStackHeight())})
		}
		inner = append(inner, calls)
	}
	return chain.Execution{Transaction: tx, Inner: inner, Receipt: chain.Receipt{Slot: slot, Keys: keys, Logs: meta.GetLogMessages()}}, true
}

type earnMaxOwner struct {
	authority earnmax.Authority
	wallets   []solana.PublicKey
}

func earnMaxOwners(transaction *PolicyTransaction, keys []solana.PublicKey) []earnMaxOwner {
	var owners []earnMaxOwner
	for _, instruction := range transaction.Instructions {
		if instruction.ProgramID != squads.ProgramID {
			continue
		}
		var signers []solana.PublicKey
		for _, account := range instruction.Accounts {
			if account.IsSigner && !slices.Contains(signers, account.PublicKey) {
				signers = append(signers, account.PublicKey)
			}
		}
		for _, account := range instruction.Accounts {
			authority, err := earnmax.AuthorityOf(account.PublicKey)
			if err != nil || !slices.Contains(keys, authority.Key()) {
				continue
			}
			at := slices.IndexFunc(owners, func(o earnMaxOwner) bool { return o.authority == authority })
			if at < 0 {
				owners = append(owners, earnMaxOwner{authority: authority})
				at = len(owners) - 1
			}
			for _, signer := range signers {
				if !slices.Contains(owners[at].wallets, signer) {
					owners[at].wallets = append(owners[at].wallets, signer)
				}
			}
		}
	}
	return owners
}

func (a *Application) recordEarnMaxCashFlows(ctx context.Context, info *pb.SubscribeUpdateTransactionInfo, transaction *PolicyTransaction) error {
	execution, ok := voltrExecution(info, transaction.Slot)
	if !ok {
		return nil
	}
	signature := solana.SignatureFromBytes(info.GetSignature())
	skip := func(reason string) {
		a.logger.Info("skipped an Earn MAX cash flow", "event", "earn_max_cash_flow_skipped", "reason", reason, "signature", transaction.Signature)
	}
	var flows []voltrFlow
	add := func(wallet solana.PublicKey, kind voltrFlowKind, amount earnmax.USDCRaw) {
		at := slices.IndexFunc(flows, func(f voltrFlow) bool { return f.wallet == wallet && f.kind == kind })
		if at < 0 {
			flows = append(flows, voltrFlow{wallet: wallet, kind: kind, amount: amount})
			return
		}
		flows[at].amount += amount
	}
	for _, owner := range earnMaxOwners(transaction, execution.Keys) {
		entries, err := earnmax.TransactionEntries(execution, signature, owner.authority)
		if err != nil {
			skip(err.Error())
			continue
		}
		if len(entries) > 0 && len(owner.wallets) != 1 {
			skip("the Squads execution has no single signer")
			continue
		}
		if slices.ContainsFunc(entries, func(entry earnmax.Entry) bool {
			claim, ok := entry.(earnmax.Claim)
			return ok && claim.Paid == nil
		}) {
			skip("the claim has no single payout transfer")
			continue
		}
		for _, entry := range entries {
			switch e := entry.(type) {
			case earnmax.Deposit:
				add(owner.wallets[0], voltrDeposit, e.Amount)
			case earnmax.Claim:
				add(owner.wallets[0], voltrClaim, *e.Paid)
			}
		}
	}
	return a.store.RecordEarnMaxCashFlows(ctx, transaction, flows)
}

func (s *Store) RecordEarnMaxCashFlows(ctx context.Context, transaction *PolicyTransaction, flows []voltrFlow) error {
	if len(flows) == 0 {
		return nil
	}
	wallets, kinds, amounts := make([]string, len(flows)), make([]string, len(flows)), make([]int64, len(flows))
	for i, flow := range flows {
		wallets[i], kinds[i], amounts[i] = flow.wallet.String(), string(flow.kind), int64(flow.amount)
	}
	_, err := s.pool.Exec(ctx, `
        INSERT INTO loyal_yield.earn_max_cash_flows (signature, wallet, kind, amount_raw, slot, transaction_index)
        SELECT $1, flow.wallet, flow.kind, flow.amount_raw, $5, $6
        FROM unnest($2::text[], $3::text[], $4::bigint[]) AS flow (wallet, kind, amount_raw)
        ON CONFLICT DO NOTHING`,
		transaction.Signature, wallets, kinds, amounts, int64(transaction.Slot), int64(transaction.Index))
	return err
}
