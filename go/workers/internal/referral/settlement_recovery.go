package referral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type expiryProof struct {
	FinalizedHeight     uint64 `json:"finalized_height"`
	FinalizedSlot       uint64 `json:"finalized_slot"`
	OldestAvailableSlot uint64 `json:"oldest_available_slot"`
	HistoryThroughSlot  uint64 `json:"history_through_slot"`
	StartBefore         string `json:"start_before,omitempty"`
	CheckedTransactions int    `json:"checked_transactions"`
}

// Every possible execution of the exported message touches the writable source
// ATA. Its original harvest guarantees there is a history entry at or before the
// already-finalized quote. Demand positive coverage on both sides of the window.
func scanExpiredSettlement(ctx context.Context, c *chain.Client, p Settlement, cursor solana.Signature) (*chain.Execution, expiryProof, error) {
	var proof expiryProof
	height, tip, err := c.FinalizedBlockHeight(ctx)
	if err != nil {
		return nil, proof, err
	}
	if height <= p.LastValidHeight || tip <= p.QuoteSlot {
		return nil, proof, errors.New("settlement hash has not reached finalized expiry")
	}
	proof.FinalizedHeight, proof.FinalizedSlot = height, tip
	proof.OldestAvailableSlot, err = c.FirstAvailableBlock(ctx)
	if err != nil {
		return nil, proof, err
	}
	if proof.OldestAvailableSlot > p.QuoteSlot {
		return nil, proof, errors.New("settlement history predates the archive")
	}
	source, err := solana.PublicKeyFromBase58(p.TreasuryATA)
	if err != nil {
		return nil, proof, err
	}
	tx, err := solana.TransactionFromBytes(p.UnsignedWire)
	if err != nil {
		return nil, proof, err
	}
	valid, at, err := c.BlockhashValid(ctx, tx.Message.RecentBlockhash, rpc.CommitmentFinalized, tip)
	if err != nil {
		return nil, proof, err
	}
	if valid || at < tip {
		return nil, proof, errors.New("archive has not confirmed expiry of this exact blockhash")
	}
	want, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, proof, err
	}
	if !cursor.IsZero() {
		anchor, err := c.Execution(ctx, cursor, rpc.CommitmentFinalized)
		if err != nil {
			return nil, proof, err
		}
		if anchor.Transaction == nil || len(anchor.Transaction.Signatures) == 0 || anchor.Transaction.Signatures[0] != cursor || !slices.Contains(anchor.Keys, source) || anchor.Slot > tip {
			return nil, proof, errors.New("cursor must be a finalized transaction of the source treasury")
		}
		anchorHeight, err := c.FinalizedBlockHeightAt(ctx, anchor.Slot)
		if err != nil {
			return nil, proof, err
		}
		if anchorHeight <= p.LastValidHeight {
			return nil, proof, errors.New("recovery cursor precedes finalized blockhash expiry")
		}
		proof.StartBefore = cursor.String()
	}
	var match *chain.Execution
	seen := map[solana.Signature]bool{}
	previous := uint64(math.MaxUint64)
	const pageSize = 64
	for range 8 {
		rows, err := c.History(ctx, source, pageSize, cursor, rpc.CommitmentFinalized, tip)
		if err != nil {
			return nil, proof, err
		}
		for _, r := range rows {
			if r.Slot > previous || seen[r.Signature] {
				return nil, proof, errors.New("inconsistent finalized treasury history")
			}
			previous = r.Slot
			seen[r.Signature] = true
			if r.Slot <= p.QuoteSlot {
				proof.HistoryThroughSlot = r.Slot
				return match, proof, nil
			}
			if r.Slot > tip {
				continue
			}
			ex, err := c.Execution(ctx, r.Signature, rpc.CommitmentFinalized)
			if err != nil {
				return nil, proof, err
			}
			if ex.Transaction == nil || len(ex.Transaction.Signatures) == 0 || ex.Transaction.Signatures[0] != r.Signature || ex.Slot != r.Slot || (ex.Err != nil) != r.Failed {
				return nil, proof, errors.New("incomplete finalized treasury receipt")
			}
			proof.CheckedTransactions++
			got, err := ex.Transaction.Message.MarshalBinary()
			if err != nil {
				return nil, proof, err
			}
			if !bytes.Equal(want, got) {
				continue
			}
			sig, err := verifySettlementWire(p.UnsignedWire, ex.Wire)
			if err != nil {
				return nil, proof, err
			}
			if match != nil || (p.Signature != nil && *p.Signature != sig.String()) {
				return nil, proof, errors.New("ambiguous signature for the settlement message")
			}
			match = &ex
		}
		if len(rows) < pageSize {
			break
		}
		cursor = rows[len(rows)-1].Signature
	}
	return nil, proof, errors.New("treasury history did not reach the finalized quote within the recovery scan")
}

// RecoverSettlement includes abandoned unsigned drafts. A null signature lookup
// or an expired clock alone cannot release a reservation.
func (s *Store) RecoverSettlement(ctx context.Context, c *chain.Client, id string, afterExpiry solana.Signature) (Settlement, error) {
	p, _, err := settlementByID(ctx, s.yield, id)
	if err != nil {
		return p, err
	}
	if p.State != "signed" && p.State != "prepared" {
		return p, nil
	}
	if err = mainnetSettlement(ctx, c); err != nil {
		return p, err
	}
	ex, expiry, err := scanExpiredSettlement(ctx, c, p, afterExpiry)
	if err != nil {
		return p, err
	}
	at, err := c.BlockTime(ctx, expiry.FinalizedSlot)
	if err != nil {
		return p, err
	}
	state, slot := "expired", expiry.FinalizedSlot
	proof, err := json.Marshal(expiry)
	if err != nil {
		return p, err
	}
	if ex != nil {
		sig := ex.Transaction.Signatures[0].String()
		p.Signature = &sig
		state, proof, err = settlementReceipt(p, ex.Wire, *ex)
		if err != nil {
			return p, err
		}
		slot, at = ex.Slot, *ex.BlockTime
	}
	err = db.WithTx(ctx, s.yield, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockSettlement(ctx, tx, p.Referee, p.TreasuryATA); err != nil {
			return err
		}
		current, wire, err := settlementByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.State != "prepared" && current.State != "signed" {
			return nil
		}
		if ex != nil {
			if current.State == "prepared" {
				if _, err = tx.Exec(ctx, "UPDATE loyal_yield.referral_earn_max_settlements SET state='signed',signature=$2,signed_wire=$3 WHERE settlement_id=$1", id, *p.Signature, ex.Wire); err != nil {
					return err
				}
			} else if !bytes.Equal(wire, ex.Wire) {
				return errors.New("finalized wire conflicts with registered signature")
			}
		}
		_, err = tx.Exec(ctx, "UPDATE loyal_yield.referral_earn_max_settlements SET state=$2,finalized_slot=$3,finalized_at=$4,receipt=$5 WHERE settlement_id=$1 AND state IN ('prepared','signed')", id, state, slot, at, proof)
		return err
	})
	if err != nil {
		return p, err
	}
	p, _, err = settlementByID(ctx, s.yield, id)
	return p, err
}
