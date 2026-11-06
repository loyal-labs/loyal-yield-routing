package backyard

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

type withdrawalCostCap struct {
	id      string
	lp      uint64
	maximum earnmax.USDCRaw
}

func allocateCappedWithdrawalComponent(total earnmax.USDCRaw, effectiveLP uint64, caps []withdrawalCostCap) ([]earnmax.USDCRaw, error) {
	if total < 0 || effectiveLP == 0 {
		return nil, fmt.Errorf("invalid finalized withdrawal allocation")
	}
	type remainder struct {
		index int
		value *big.Int
	}
	amounts := make([]earnmax.USDCRaw, len(caps))
	order := make([]remainder, 0, len(caps))
	left := total
	for i, c := range caps {
		if c.maximum < 0 {
			return nil, fmt.Errorf("invalid frozen withdrawal cap")
		}
		n := new(big.Int).Mul(big.NewInt(int64(total)), new(big.Int).SetUint64(c.lp))
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(n, new(big.Int).SetUint64(effectiveLP), r)
		if !q.IsInt64() || q.Int64() < 0 || earnmax.USDCRaw(q.Int64()) > c.maximum {
			return nil, fmt.Errorf("finalized withdrawal exceeds frozen participant cap")
		}
		amounts[i] = earnmax.USDCRaw(q.Int64())
		left -= amounts[i]
		order = append(order, remainder{i, r})
	}
	sort.Slice(order, func(i, j int) bool {
		if cmp := order[i].value.Cmp(order[j].value); cmp != 0 {
			return cmp > 0
		}
		a, b := caps[order[i].index].id, caps[order[j].index].id
		if a == "" || b == "" {
			return b == "" && a != ""
		}
		return a < b
	})
	for left > 0 {
		before := left
		for _, r := range order {
			if left == 0 {
				break
			}
			if amounts[r.index] < caps[r.index].maximum {
				amounts[r.index]++
				left--
			}
		}
		if before == left {
			break
		}
	}
	if left != 0 {
		return nil, fmt.Errorf("finalized withdrawal component cannot be conserved within frozen caps")
	}
	return amounts, nil
}

// allocateSettlement keeps every holder's share within its frozen
// reservation. A finalized loss above the reservation is incremental by
// construction and is booked whole to ExcessCost, a pool/operator expense.
func (p *withdrawalCostPreparation) allocateSettlement(total, incremental earnmax.USDCRaw) error {
	if err := p.validateAccounting(); err != nil {
		return err
	}
	if total < 0 || incremental < 0 || incremental > total || total-incremental > p.MaxCost-p.IncrementalCost {
		return fmt.Errorf("finalized withdrawal exceeds frozen component totals")
	}
	excess := max(total-p.MaxCost, 0)
	if incremental-excess > p.IncrementalCost {
		return fmt.Errorf("finalized withdrawal exceeds frozen component totals")
	}
	total, incremental = total-excess, incremental-excess
	incCaps := make([]withdrawalCostCap, 0, len(p.Participants)+1)
	coveredCaps := make([]withdrawalCostCap, 0, len(p.Participants)+1)
	remainingLP := p.EffectiveLP
	for _, r := range p.Participants {
		remainingLP -= r.EscrowLP
		incCaps = append(incCaps, withdrawalCostCap{r.ID, r.EscrowLP, r.IncrementalCost})
		coveredCaps = append(coveredCaps, withdrawalCostCap{r.ID, r.EscrowLP, r.MaxCost - r.IncrementalCost})
	}
	incCaps = append(incCaps, withdrawalCostCap{"", remainingLP, p.IncrementalOtherHolders})
	coveredCaps = append(coveredCaps, withdrawalCostCap{"", remainingLP, p.OtherHolders - p.IncrementalOtherHolders})
	inc, err := allocateCappedWithdrawalComponent(incremental, p.EffectiveLP, incCaps)
	if err != nil {
		return err
	}
	covered, err := allocateCappedWithdrawalComponent(total-incremental, p.EffectiveLP, coveredCaps)
	if err != nil {
		return err
	}
	for i := range p.Participants {
		p.Participants[i].IncrementalCost = inc[i]
		p.Participants[i].MaxCost = inc[i] + covered[i]
	}
	last := len(p.Participants)
	p.IncrementalOtherHolders = inc[last]
	p.OtherHolders = inc[last] + covered[last]
	p.MaxCost = total + excess
	p.IncrementalCost = incremental + excess
	p.ExcessCost = excess
	return nil
}
