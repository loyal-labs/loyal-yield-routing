package earnmax

import (
	"errors"
	"math/big"
	"sort"
)

type WithdrawalCostParticipant struct {
	ConsentID  string
	EscrowedLP uint64
}

type WithdrawalCostShare struct {
	ConsentID string
	Cost      USDCRaw
}

type WithdrawalCostAllocation struct {
	Requests     []WithdrawalCostShare
	OtherHolders USDCRaw
}

// AllocateWithdrawalCost freezes economic attribution at preparation, including
// nonparticipants. Largest remainders then consent ID make integer sums exact.
// It neither reserves pool cash nor discounts costs already included in NAV.
func AllocateWithdrawalCost(total USDCRaw, effectiveLP uint64, participants []WithdrawalCostParticipant) (WithdrawalCostAllocation, error) {
	if total < 0 || effectiveLP == 0 {
		return WithdrawalCostAllocation{}, errors.New("invalid withdrawal allocation basis")
	}
	type share struct {
		id        string
		cost      USDCRaw
		remainder *big.Int
		other     bool
	}
	shares := make([]share, 0, len(participants)+1)
	seen := map[string]bool{}
	remaining := effectiveLP
	allocated := USDCRaw(0)
	add := func(id string, lp uint64, other bool) {
		n := new(big.Int).Mul(big.NewInt(int64(total)), new(big.Int).SetUint64(lp))
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(n, new(big.Int).SetUint64(effectiveLP), r)
		cost := USDCRaw(q.Int64())
		allocated += cost
		shares = append(shares, share{id, cost, r, other})
	}
	for _, p := range participants {
		if p.ConsentID == "" || seen[p.ConsentID] || p.EscrowedLP == 0 || p.EscrowedLP > remaining {
			return WithdrawalCostAllocation{}, errors.New("invalid or duplicate withdrawal participant")
		}
		seen[p.ConsentID] = true
		remaining -= p.EscrowedLP
		add(p.ConsentID, p.EscrowedLP, false)
	}
	if remaining > 0 {
		add("", remaining, true)
	}
	sort.Slice(shares, func(i, j int) bool {
		if cmp := shares[i].remainder.Cmp(shares[j].remainder); cmp != 0 {
			return cmp > 0
		}
		if shares[i].other != shares[j].other {
			return !shares[i].other
		}
		return shares[i].id < shares[j].id
	})
	for i := USDCRaw(0); i < total-allocated; i++ {
		shares[int(i)].cost++
	}
	result := WithdrawalCostAllocation{Requests: []WithdrawalCostShare{}}
	for _, s := range shares {
		if s.other {
			result.OtherHolders = s.cost
		} else {
			result.Requests = append(result.Requests, WithdrawalCostShare{s.id, s.cost})
		}
	}
	sort.Slice(result.Requests, func(i, j int) bool { return result.Requests[i].ConsentID < result.Requests[j].ConsentID })
	return result, nil
}
