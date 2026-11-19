package referral

import (
	"errors"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

// Rationals retain fractional micro-USDC across runs, including release of
// fractional cost basis on partial withdrawal or cancellation.
type capitalReplay struct {
	Free        uint64  `json:"free"`
	Escrow      uint64  `json:"escrow"`
	Active      big.Rat `json:"active"`
	Reserved    big.Rat `json:"reserved"`
	Earned      big.Rat `json:"earned"`
	At          int64   `json:"at"`
	LastEventAt int64   `json:"lastEventAt"`
}

func (r *capitalReplay) accrue(at int64) error {
	if at < r.At {
		return errors.New("nonmonotonic_event_time")
	}
	// A fixed 365-day year, simple annual rate of 3 per 1000, no compounding.
	elapsed := new(big.Rat).SetInt64(at - r.At)
	elapsed.Mul(elapsed, big.NewRat(3, 1000*365*24*60*60))
	r.Earned.Add(&r.Earned, elapsed.Mul(elapsed, &r.Active))
	r.At = at
	return nil
}

func costPortion(cost *big.Rat, shares, total uint64) *big.Rat {
	if shares == 0 {
		return new(big.Rat)
	}
	fraction := new(big.Rat).SetFrac(new(big.Int).SetUint64(shares), new(big.Int).SetUint64(total))
	return fraction.Mul(fraction, cost)
}

func (r *capitalReplay) apply(m earnmax.CapitalMovement, start time.Time) error {
	freeSeen, escrowSeen := capitalBalancesSeen(m)
	if (freeSeen && r.Free != m.FreeBefore) || (escrowSeen && r.Escrow != m.EscrowBefore) {
		return errors.New("opening_share_balance_unknown")
	}
	for _, e := range m.Events {
		if e.UnixTime < r.LastEventAt || (e.UnixTime < r.At && r.At > start.Unix()) {
			return errors.New("nonmonotonic_event_time")
		}
		r.LastEventAt = e.UnixTime
		eligible := !time.Unix(e.UnixTime, 0).Before(start)
		if eligible {
			if err := r.accrue(e.UnixTime); err != nil {
				return err
			}
		}
		switch e.Kind {
		case "deposit":
			if eligible {
				if r.Free > 0 && r.Active.Sign() == 0 {
					return errors.New("mixed_share_origin")
				}
				net, ok := new(big.Int).SetString(e.NetAssetNumerator, 10)
				if !ok || net.Sign() <= 0 || e.NetAssetDenominator == 0 {
					return errors.New("missing_deposit_net")
				}
				r.Active.Add(&r.Active, new(big.Rat).SetFrac(net, new(big.Int).SetUint64(e.NetAssetDenominator)))
			}
			if ^uint64(0)-r.Free < e.LP {
				return errors.New("invalid_share_balance")
			}
			r.Free += e.LP
		case "transfer_in":
			if r.Active.Sign() != 0 {
				return errors.New("mixed_share_origin")
			}
			if ^uint64(0)-r.Free < e.LP {
				return errors.New("invalid_share_balance")
			}
			r.Free += e.LP
		case "request", "transfer_out":
			if e.LP == 0 || e.LP > r.Free {
				return errors.New("invalid_share_balance")
			}
			portion := costPortion(&r.Active, e.LP, r.Free)
			r.Active.Sub(&r.Active, portion)
			r.Free -= e.LP
			if e.Kind == "request" {
				if r.Escrow != 0 {
					return errors.New("overlapping_withdrawal_requests")
				}
				r.Escrow = e.LP
				r.Reserved.Add(&r.Reserved, portion)
			}
		case "claim":
			if e.LP != r.Escrow {
				return errors.New("claim_escrow_mismatch")
			}
			r.Escrow = 0
			r.Reserved.SetInt64(0)
		case "cancel":
			if e.LP > r.Escrow || e.BurnedLP != r.Escrow-e.LP {
				return errors.New("cancel_escrow_mismatch")
			}
			if e.LP > 0 && r.Free > 0 && (r.Active.Sign() == 0) != (r.Reserved.Sign() == 0) {
				return errors.New("mixed_share_origin")
			}
			r.Active.Add(&r.Active, costPortion(&r.Reserved, e.LP, r.Escrow))
			r.Reserved.SetInt64(0)
			r.Escrow = 0
			if ^uint64(0)-r.Free < e.LP {
				return errors.New("invalid_share_balance")
			}
			r.Free += e.LP
		default:
			return errors.New("unknown_capital_event")
		}
	}
	if (freeSeen && r.Free != m.FreeAfter) || (escrowSeen && r.Escrow != m.EscrowAfter) {
		return errors.New("closing_share_balance_mismatch")
	}
	return nil
}

// Solana receipts omit token accounts that the transaction never referenced.
// In particular a deposit can top up free LP while a withdrawal stays escrowed,
// and a claim can burn escrow while untouched free LP remains in the wallet.
func capitalBalancesSeen(m earnmax.CapitalMovement) (bool, bool) {
	free := m.FreeBefore != 0 || m.FreeAfter != 0
	escrow := m.EscrowBefore != 0 || m.EscrowAfter != 0
	for _, e := range m.Events {
		switch e.Kind {
		case "deposit", "transfer_in", "transfer_out":
			free = true
		case "request":
			free, escrow = true, true
		case "claim":
			escrow = true
		case "cancel":
			escrow = true
			free = free || e.LP > 0
		}
	}
	return free, escrow
}
