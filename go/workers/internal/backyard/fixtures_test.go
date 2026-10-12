package backyard

import (
	"context"
	"math"

	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Fixture builders the tests keep after the worker stopped needing them.

// confirmedAccounts is readAccounts at confirmed.
func confirmedAccounts(ctx context.Context, c *chain.Client, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, error) {
	return readAccounts(ctx, c, rpc.CommitmentConfirmed, addresses, minSlot, optional...)
}

// depositProjectionAddresses is a lane's entry-projection capture set.
func depositProjectionAddresses(route RuntimeRoute) []string {
	addresses := []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply, route.DebtCustody, budgetClockAddress}
	if earnHeldLane(route.Lane) {
		addresses = append(addresses, route.Kamino.Market)
	}
	return addresses
}

// kaminoBorrowEffects is a lone borrow leg's effects: the reserve supply pays
// receive and the fee, the debt custody takes receive, the fee vault the fee.
func kaminoBorrowEffects(accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) (ExpectedEffects, error) {
	fee, err := kaminoBorrowFee(accounts, route, receive)
	if err != nil {
		return ExpectedEffects{}, err
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegBorrow, route)
	effects, err := exactKaminoTokenEffects(accounts, source, destination, receive)
	if err != nil {
		return effects, err
	}
	if effects.Accounts[0].AfterRaw < fee {
		return effects, budgetHold("borrow_supply_cannot_cover_fee")
	}
	a := accountAt(accounts, route.DebtFeeReceiver)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(route.Kamino.MarketAuthority)
	custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
	if err != nil || a.Owner != route.DebtTokenProgram || a.Lamports == 0 || a.Executable || custody.Raw > math.MaxUint64-fee {
		return effects, budgetHoldFrom(err, "invalid_borrow_fee_custody")
	}
	effects.Kind = "kamino-borrow"
	effects.Accounts[0].AfterRaw -= fee
	effects.Accounts = append(effects.Accounts, ExpectedAccountEffect{Address: a.Address, Owner: a.Owner, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: custody.Raw, AfterRaw: custody.Raw + fee})
	return effects, nil
}

// leverageExitSnapshot is a 1.75x position on lane with its payoff window.
func leverageExitSnapshot(lane string) Snapshot {
	s := leverageSnapshot(1.75)
	s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, 1.75
	s.PayoffDebtRaw = s.PositionDebtRaw + 1_000
	return s
}

func setDebtCash(s *Snapshot, raw int64) { setDebtCashRaw(s, raw) }

// liveIdleDebtSnapshot is the live 2026-09-28 AUTO state: a cancelled exit
// left 226.44 PYUSD in debt custody against 189.92 PYUSD debt at 55% LTV,
// with no withdrawal demand.
func liveIdleDebtSnapshot() Snapshot {
	s := base()
	s.RouteLane = autoAUTOPYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, 338_210_000, 189_920_000
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 345_000_000, 189_920_000
	s.PayoffDebtRaw, s.DebtIdleRaw, s.LTVBPS = 189_921_000, 226_440_000, 5500
	s.VoltrIdleRaw = 1_295_000_000
	return s
}

// livePartialSnapshot is the live 2026-09-29 AUTO state: $1,677 at ~1.5x
// with a $470 withdrawal queued and no Voltr idle.
func livePartialSnapshot() Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey, s.HasPosition = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.LeverageTargetLevel = 1.5
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 2_463_480_000, 2_514_967_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 837_770_000, 837_770_000, 837_900_000
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = 1_677_197_000, 1_677_197_000
	s.LTVBPS = 3331
	s.WithdrawalDemandRaw = 470_000_000
	return s
}

// exitAllTarget is the operator exit with equity 0: debt to 0, then every
// custody of the lane toward Voltr.
var exitAllTarget = LaneTarget{Rule: ruleOperatorExit, Level: 1, EquityZero: true, Outbound: true}
