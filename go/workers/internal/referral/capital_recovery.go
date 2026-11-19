package referral

import (
	"encoding/json"
	"math/big"
	"time"
)

// Persist both sides of the historical gap so another accounting reader can
// still prove ownership inside the earlier, unaffected interval.
type capitalReset struct {
	slot        uint64
	at          time.Time
	replay      []byte
	reason      string
	priorSlot   *uint64
	priorAt     *time.Time
	priorReplay []byte
}

func resettableOrigin(reason string) bool {
	switch reason {
	case "mixed_share_origin", "opening_share_balance_unknown", "historical_deposit_owner_unproved", "unverified_transaction",
		"history_not_retained", "history_page_limit", "transaction_batch_limit", "same_slot_order_unproved":
		return true
	}
	return false
}

// When every receipt is present, traverse its authenticated balance deltas
// backwards from the actual finalized free and escrow accounts. An untouched
// account's absence in a transaction is never treated as a zero balance.
func recoverCapital(facts []capitalFact, failed int, free, held, slot uint64, at, start time.Time, earned *big.Rat) (*capitalReset, *capitalReplay) {
	try := func(zeroSlot uint64, zeroAt time.Time, following []capitalFact) (*capitalReset, *capitalReplay) {
		if zeroAt.After(at) {
			return nil, nil
		}
		r := &capitalReplay{At: max(zeroAt.Unix(), start.Unix()), LastEventAt: zeroAt.Unix()}
		r.Earned.Set(earned)
		anchor, err := json.Marshal(r)
		if err != nil {
			return nil, nil
		}
		for _, f := range following {
			if f.slot <= zeroSlot || r.apply(f.movement, start) != nil {
				return nil, nil
			}
		}
		if r.Free != free || r.Escrow != held || r.accrue(at.Unix()) != nil {
			return nil, nil
		}
		return &capitalReset{slot: zeroSlot, at: zeroAt, replay: anchor}, r
	}
	if failed >= 0 {
		f, h := free, held
		var earliest *capitalReset
		var result *capitalReplay
		for i := len(facts) - 1; i >= failed; i-- {
			m := facts[i].movement
			touchedFree, touchedEscrow := capitalBalancesSeen(m)
			if (touchedFree && f != m.FreeAfter) || (touchedEscrow && h != m.EscrowAfter) {
				break
			}
			if f == 0 && h == 0 {
				reset, r := try(facts[i].slot, facts[i].at, facts[i+1:])
				if reset != nil {
					earliest, result = reset, r
				}
			}
			if touchedFree {
				f = m.FreeBefore
			}
			if touchedEscrow {
				h = m.EscrowBefore
			}
		}
		if earliest != nil {
			return earliest, result
		}
	}
	if free == 0 && held == 0 {
		return try(slot, at, nil)
	}
	return nil, nil
}
