package backyard

import (
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

func TestOrdinaryFundingAllowlistCountsOnlyFundedReceipts(t *testing.T) {
	var s Snapshot
	s.Target.Rule = ruleWithdrawal
	s.WithdrawalDemandRaw = 40
	d := Decision{Action: SwapCollateralToStableStep}
	cost := &withdrawalCostPreparation{Participants: []withdrawalCostParticipant{
		{Class: withdrawalOwnerVaultAdmin, User: "canary"},
		{Class: withdrawalOwnerUnsupported, User: "stranger", Wait: string(withdrawalOwnerUnsupported)},
	}}
	r := releaseRequestOf(looping, s, d, withdrawalCostRequest{request: StepRequest{}, cost: cost})
	if r == nil || r.action != releasecontrol.OrdinaryFunding || !slices.Equal(r.owners, []string{"canary"}) {
		t.Fatalf("a waiting receipt would refuse the canary's own funding: %+v", r)
	}
	if r := releaseRequestOf(looping, s, d, StepRequest{}); r != nil {
		t.Fatalf("a move charging nobody was gated as ordinary funding: %+v", r)
	}
}

func TestPublicFundingKeepsOperatorReleaseControl(t *testing.T) {
	s := Snapshot{Target: LaneTarget{Rule: ruleWithdrawal}, WithdrawalDemandRaw: 40}
	r := releaseRequestOf(rwa, s, Decision{Action: SwapCollateralToStableStep}, StepRequest{})
	if r == nil || r.action != releasecontrol.OrdinaryFunding || r.amount == nil || *r.amount != 40 {
		t.Fatalf("public funding bypasses the operator hold: %+v", r)
	}
}
