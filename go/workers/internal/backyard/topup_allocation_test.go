package backyard

import (
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// The live state after the payoff and both earlier top-up legs: 1x AUTO,
// no debt, no working cash, $1,295 idle in Voltr, PYUSD borrowing blocked.
func liveTopupSnapshot() Snapshot {
	s := liveIdleDebtSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.DebtIdleRaw = 0, 0, 0, 0, 0
	s.BorrowUtilizationBlocked, s.TopupDepositRoomRaw = true, 5_000_000_000
	return s
}

func TestTopupAllocationSizingAndPriority(t *testing.T) {
	t.Parallel()
	s := liveTopupSnapshot()
	got := Decide(targeted(s))
	if got.Action != VoltrAllocateToSquads || got.Reason != topupAllocationReason || got.AmountRaw != s.VoltrIdleRaw || got.Validate() != nil {
		t.Fatalf("idle cash was not topped up: %+v", got)
	}
	// Size = min(idle - buffer, working tranche cap, deposit-limit room).
	room := s
	room.TopupDepositRoomRaw = 700_000_000
	if got := Decide(targeted(room)); got.AmountRaw != 700_000_000 {
		t.Fatalf("deposit-limit room ignored: %+v", got)
	}
	capped := s
	capped.VoltrIdleRaw, capped.TopupDepositRoomRaw = int64(strategyTwoBridgeLegCapRaw)+5, int64(strategyTwoBridgeLegCapRaw)+5
	if got := Decide(targeted(capped)); got.AmountRaw != int64(strategyTwoBridgeLegCapRaw) {
		t.Fatalf("working tranche cap ignored: %+v", got)
	}
	// Open borrowing does not jump ahead of idle cash: top up first, then the
	// borrow levers the whole collateral.
	open := s
	open.BorrowUtilizationBlocked = false
	if got := Decide(targeted(open)); got.Reason != topupAllocationReason {
		t.Fatalf("borrow preempted the top-up: %+v", got)
	}
	open.VoltrIdleRaw, open.LeverageTargetLevel = 0, 1.5
	armLeverageCapacityFixture(&open)
	if got := Decide(targeted(open)); got.Action != OpenRouteStep || got.Reason != leverageUpReason {
		t.Fatalf("no borrow after the top-up is done: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1 },
		"covered demand":   func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"hard ltv":         func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.DebtIdleRaw = 1, 1, 6000, 1 },
		"report due":       func(s *Snapshot) { s.PostMutationNAVRequired = true },
		"nonterminal":      func(s *Snapshot) { s.Nonterminal = Built },
		"no room":          func(s *Snapshot) { s.TopupDepositRoomRaw = 0 },
		"dust":             func(s *Snapshot) { s.VoltrIdleRaw = topupMinimumRaw - 1 },
		"unvalued debt":    func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw = 1, 0 },
		"flat":             func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = false, 0, 0 },
	} {
		c := s
		mutate(&c)
		if got := Decide(targeted(c)); got.Reason == topupAllocationReason {
			t.Fatalf("%s: top-up allocation selected: %+v", name, got)
		}
	}
}

// Live 2026-10-09 Earn Max: ~$2.1k AUTO collateral against ~$905 PYUSD debt
// (LTV ~43%) with ~$100,005 idle in Voltr held single_loop_position_ready.
func liveDebtTopupSnapshot() Snapshot {
	s := liveTopupSnapshot()
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 2_058_000_000, 2_100_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = 905_000_000, 905_000_000, 905_001_000, 4310
	s.VoltrIdleRaw, s.TopupDepositRoomRaw = 100_005_000_000, 500_000_000_000
	return s
}

// The top-up runs beside a debt-bearing AUTO position: allocate, swap, then
// a collateral-only deposit into the same obligation, then hold. No leg
// borrows or repays; re-levering stays with the leverage rules.
func TestTopupBesideDebtAllocatesSwapsRedepositsThenHolds(t *testing.T) {
	t.Parallel()
	s := liveDebtTopupSnapshot()
	steps := []struct {
		mutate func(*Snapshot)
		action Action
		reason string
		amount int64
	}{
		{func(*Snapshot) {}, VoltrAllocateToSquads, topupAllocationReason, 100_005_000_000},
		{func(s *Snapshot) { s.VoltrIdleRaw, s.SquadsIdleRaw = 5_000_000, 100_005_000_000 }, SwapStableToCollateralStep, topupSwapReason, 100_005_000_000},
		{func(s *Snapshot) {
			s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 97_900_000_000, 97_900_000_000, 99_900_000_000
		}, OpenRouteStep, topupDepositReason, 97_900_000_000},
		{func(s *Snapshot) {
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
			s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 99_958_000_000, 102_000_000_000, 88
		}, Hold, holdAtTarget, 0},
	}
	for i, step := range steps {
		step.mutate(&s)
		got := Decide(targeted(s))
		if got.Action != step.action || got.Reason != step.reason || got.AmountRaw != step.amount || got.Validate() != nil {
			t.Fatalf("step %d: %+v", i, got)
		}
	}
	// Sizing is the debt-free tranche rule: deposit-limit room still binds.
	room := liveDebtTopupSnapshot()
	room.TopupDepositRoomRaw = 700_000_000
	if got := Decide(targeted(room)); got.Reason != topupAllocationReason || got.AmountRaw != 700_000_000 {
		t.Fatalf("deposit-limit room ignored beside debt: %+v", got)
	}
	// Idle collateral beside debt joins the top-up: Squads cash swaps first,
	// then one deposit takes both.
	loop := liveDebtTopupSnapshot()
	loop.SquadsIdleRaw, loop.CollateralIdleRaw, loop.PrimeIdleRaw = 1_000_000, 35_000_000, 35_000_000
	if got := Decide(targeted(loop)); got.Action != SwapStableToCollateralStep || got.Reason != topupSwapReason || got.AmountRaw != 1_000_000 {
		t.Fatalf("collateral beside top-up cash: %+v", got)
	}
	for name, c := range map[string]struct {
		mutate func(*Snapshot)
		reason string
	}{
		// Idle PYUSD beside debt is a step's conversion excess: it converts
		// to collateral before any top-up.
		"debt idle": {func(s *Snapshot) { s.DebtIdleRaw = s.PositionDebtRaw }, stepResidueReason},
		// Ethena is no leverage lane: debt keeps the installed hold.
		"non-leverage lane": {func(s *Snapshot) { s.RouteLane, s.StrategyKey = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane }, holdAtTarget},
	} {
		d := liveDebtTopupSnapshot()
		c.mutate(&d)
		if got := Decide(targeted(d)); got.Reason != c.reason {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// OnRe debt is Squads USDC: cash there beside debt converts to
	// collateral as a step's excess, never as a Voltr top-up.
	onre := liveDebtTopupSnapshot()
	onre.RouteLane, onre.StrategyKey, onre.SquadsIdleRaw = onreONycUSDC, onreONycUSDC, 1_000_000
	if got := Decide(targeted(onre)); got.Action != SwapDebtToCollateralStep || got.Reason != stepResidueReason || got.AmountRaw != 1_000_000 {
		t.Fatalf("OnRe cash beside debt: %+v", got)
	}
}

// autoDebtTopupFixture is the owned AUTO batch (pilot, PYUSD debt, released
// collateral) observed by the production observer, reshaped to the top-up
// state: no withdrawal or stage in flight, empty Squads, collateral and debt
// custody, and $50 idle in Voltr.
func autoDebtTopupFixture(t *testing.T) (Observation, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := partialWithdrawalRestoreFixture(t, autoAUTOPYUSD.Lane, int64(autoFixtureDebtRaw))
	for _, address := range []string{bridgeStrategyATA, bridgeSquadsATA, autoAUTOPYUSD.CollateralCustody, autoAUTOPYUSD.DebtCustody} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[64:72], 0)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], 50_000_000)
	s := &o.Snapshot
	s.LeverageTargetLevel, s.WithdrawalDemandRaw = 0, 0
	s.VoltrStrategyIdleRaw, s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = 0, 0, false, false
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.DebtIdleRaw, s.VoltrIdleRaw = 0, 0, 0, 0, 50_000_000
	if !s.HasPosition || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 {
		t.Fatalf("fixture lost the debt-bearing AUTO position: %+v", *s)
	}
	return o, m, rpc, client, accounts
}
