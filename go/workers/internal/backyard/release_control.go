package backyard

import (
	"slices"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

// releaseRequest is the release control a decision must be admitted under in
// the transaction that records it. Nil means the decision is never gated:
// emergency reductions, zero-cost cash moves, NAV reports and recovery.
type releaseRequest struct {
	action releasecontrol.Action
	owners []string
	amount *uint64
}

func releaseRequestOf(s Snapshot, d Decision, request any) *releaseRequest {
	if requiresNAVForAdd(d) {
		return &releaseRequest{action: releasecontrol.OptionalAddition}
	}
	if !ordinaryWithdrawalCostRequired(s, d) || !costBearingWithdrawalAction(d) {
		return nil
	}
	// Ordinary funding is gated where it charges approved costs; a move that
	// charges nobody is a cash move like the others above.
	protected, ok := request.(withdrawalCostRequest)
	if !ok || protected.cost == nil {
		return nil
	}
	r := &releaseRequest{action: releasecontrol.OrdinaryFunding}
	if s.WithdrawalDemandRaw >= 0 {
		amount := uint64(s.WithdrawalDemandRaw)
		r.amount = &amount
	}
	for _, p := range protected.cost.Participants {
		if p.Wait != "" {
			continue
		}
		owner := p.User
		if owner == "" {
			owner = p.Consent.Authority
		}
		if owner != "" && !slices.Contains(r.owners, owner) {
			r.owners = append(r.owners, owner)
		}
	}
	return r
}
