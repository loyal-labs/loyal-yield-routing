package backyard

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func entrySwapAdmissionFixture(t *testing.T) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, _, _, manifest, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	o.Snapshot.HasPosition = false
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 0, 0
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0
	obligation := accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation)
	binary.LittleEndian.PutUint64(obligation.Data[128:136], 0)
	clear(obligation.Data[1296:1312])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	d := Decision{Action: SwapStableToCollateralStep, AmountRaw: 20_000, StrategyKey: o.Snapshot.RouteLane, Reason: "entry_swap", IdempotencyKey: "entry-swap-admission"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, testPolicies(t), d, 20_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved = true
	return o, d, e, manifest, rpc, client, accounts
}
