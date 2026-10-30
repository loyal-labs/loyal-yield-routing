package backyard

import (
	"context"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Last reconciled NAV report before the first foreign idle transfer (2026-10-30).
const foreignInflowAnchor = "4xAkkN1U82HKEaEFeS1N5YvtoyC831vdL67EudTybpEWeLmPN9pfZNgRKwGbj8DuoPtbrwAzb2jzw5nUPorbsWq8"
const foreignInflowAnchorSlot = 463246083

type foreignInflowChain interface {
	History(context.Context, solana.PublicKey, int, solana.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error)
	Execution(context.Context, solana.Signature, rpc.CommitmentType) (chain.Execution, error)
	Slot(context.Context, rpc.CommitmentType) (uint64, error)
}

type foreignInflowStore interface {
	foreignCheckpoint(context.Context, string, string) (foreignCheckpoint, bool, error)
	saveForeignEvidence(context.Context, string, string, foreignEvidence) error
}

func observeForeignInflows(ctx context.Context, db foreignInflowStore, c foreignInflowChain, v *Vault, s *Snapshot) error {
	if v.trustful() {
		return nil
	}
	if v.RouteKey != rwa.RouteKey || s.Slot < foreignInflowAnchorSlot {
		return fmt.Errorf("foreign-inflow history has no audited anchor")
	}
	finalized, err := c.Slot(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return err
	}
	for _, address := range []string{v.idleATA, v.strategyATA} {
		checkpoint, err := replayForeignInflows(ctx, db, c, v, address, uint64(s.Slot), finalized, "")
		if err != nil {
			var fault foreignAccountingFault
			if errors.As(err, &fault) {
				s.ForeignInflowFault = true
				return nil
			}
			return err
		}
		balance := s.VoltrIdleRaw
		if address == v.strategyATA {
			balance = s.VoltrStrategyIdleRaw
		}
		if balance < 0 || uint64(balance) != checkpoint.BalanceRaw {
			// Account reads and history may straddle a transaction in the same slot.
			return fmt.Errorf("foreign-inflow history and observation have not converged")
		}
		if address == v.idleATA {
			s.AcknowledgedForeignIdleRaw = int64(checkpoint.ForeignRaw)
		} else {
			s.ForeignCustodyRaw = int64(checkpoint.ForeignRaw)
		}
	}
	return nil
}

func replayForeignInflows(ctx context.Context, db foreignInflowStore, c foreignInflowChain, v *Vault, address string, through, finalized uint64, stopBefore string) (foreignCheckpoint, error) {
	checkpoint, exists, err := db.foreignCheckpoint(ctx, v.RouteKey, address)
	if err != nil {
		return checkpoint, err
	}
	if !exists {
		x, err := c.Execution(ctx, solana.MustSignatureFromBase58(foreignInflowAnchor), rpc.CommitmentFinalized)
		if err != nil {
			return checkpoint, err
		}
		if _, err := foreignAccountTransfers(v, address, x); err != nil {
			return checkpoint, err
		}
		if x.Slot != foreignInflowAnchorSlot || len(x.Transaction.Signatures) == 0 || x.Transaction.Signatures[0].String() != foreignInflowAnchor {
			return checkpoint, fmt.Errorf("foreign-inflow anchor identity mismatch")
		}
		checkpoint = foreignCheckpoint{Signature: foreignInflowAnchor, Slot: x.Slot, BalanceRaw: x.Post[solanaKey(address)].Amount}
		if address == v.strategyATA && checkpoint.BalanceRaw != 0 {
			return checkpoint, fmt.Errorf("foreign-inflow anchor custody is not empty")
		}
		if err := db.saveForeignEvidence(ctx, v.RouteKey, address, foreignEvidence{After: checkpoint, Execution: x}); err != nil {
			return checkpoint, err
		}
	}
	if checkpoint.Slot > through {
		return checkpoint, fmt.Errorf("foreign-inflow checkpoint is newer than observation")
	}
	var pending []chain.Signed
	var before solana.Signature
	found := false
	seen := make(map[solana.Signature]bool)
	for page := 0; page < 20 && !found; page++ {
		rows, err := c.History(ctx, solanaKey(address), 200, before, rpc.CommitmentConfirmed, through)
		if err != nil {
			return checkpoint, err
		}
		for _, row := range rows {
			if row.Signature.String() == checkpoint.Signature {
				found = true
				break
			}
			if seen[row.Signature] || row.Slot < checkpoint.Slot {
				return checkpoint, fmt.Errorf("foreign-inflow history lost its checkpoint")
			}
			seen[row.Signature] = true
			if row.Slot <= through {
				pending = append(pending, row)
			}
		}
		if len(rows) == 0 {
			break
		}
		before = rows[len(rows)-1].Signature
	}
	if !found {
		return checkpoint, fmt.Errorf("foreign-inflow history is incomplete or exceeds scan bound")
	}
	processed := 0
	for i := len(pending) - 1; i >= 0; i-- {
		row := pending[i]
		if row.Slot < checkpoint.Slot {
			return checkpoint, fmt.Errorf("foreign-inflow history is not ordered")
		}
		if row.Signature.String() == stopBefore {
			return checkpoint, nil
		}
		commitment := rpc.CommitmentConfirmed
		if row.Slot <= finalized {
			commitment = rpc.CommitmentFinalized
		}
		e := foreignEvidence{Before: checkpoint, After: checkpoint}
		if row.Failed {
			e.After.Signature, e.After.Slot = row.Signature.String(), row.Slot
		} else {
			x, err := c.Execution(ctx, row.Signature, commitment)
			if err != nil {
				return checkpoint, err
			}
			if x.Slot != row.Slot {
				return checkpoint, fmt.Errorf("foreign-inflow transaction slot changed")
			}
			e, err = advanceForeignCheckpoint(v, address, row.Signature.String(), checkpoint, x)
			if err != nil {
				return checkpoint, err
			}
		}
		if commitment == rpc.CommitmentFinalized {
			if err := db.saveForeignEvidence(ctx, v.RouteKey, address, e); err != nil {
				return checkpoint, err
			}
		}
		checkpoint = e.After
		processed++
		if processed == 64 && i > 0 {
			return checkpoint, fmt.Errorf("foreign-inflow history catch-up in progress")
		}
	}
	if stopBefore != "" {
		return checkpoint, fmt.Errorf("foreign sweep transaction missing from history")
	}
	return checkpoint, nil
}
