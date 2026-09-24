package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"time"
)

func observeRedepositPrestate(ctx context.Context, rpc *RPCClient, route RuntimeRoute, s Snapshot, slot int64) (KaminoPayoffBound, []ConfirmedAccount, error) {
	bound, accounts, err := observeKaminoPayoffWindow(ctx, rpc, route, slot, 3)
	if err != nil {
		return bound, nil, err
	}
	position, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || s.PositionCollateralRaw <= 0 || position.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || s.PositionDebtRaw <= 0 || !sameAccruingDebt(bound, s.PositionDebtRaw) {
		return bound, nil, budgetHold("redeposit_position_changed")
	}
	a := accountAt(accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
	if err != nil || a.Executable || a.Lamports == 0 || cash.Raw != 0 {
		return bound, nil, budgetHold("redeposit_debt_custody_changed")
	}
	return bound, accounts, nil
}

func validateRedepositProjection(r KaminoPrimeUSDCRequest, e ExpectedEffects, before []ConfirmedAccount, p phase3KaminoProjection) error {
	message, err := CompileKaminoMessage(r)
	if err != nil || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 || e.Deposit == nil {
		return budgetHold("redeposit_projection_identity_mismatch")
	}
	if _, err := MeasureExecutableDebit(r, e); err != nil {
		return err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return err
	}
	var moved uint64
	for i, effect := range e.Accounts {
		a := accountAt(p.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Owner != effect.Owner || a.Executable || a.Lamports == 0 {
			return budgetHold("redeposit_projection_custody_mismatch")
		}
		if i == 0 {
			if cash.Raw > effect.BeforeRaw {
				return budgetHold("redeposit_projection_debit_mismatch")
			}
			moved = effect.BeforeRaw - cash.Raw
			if moved < e.Deposit.MinimumDebitRaw || moved > e.Deposit.MaximumDebitRaw {
				return budgetHold("redeposit_projection_debit_mismatch")
			}
		} else if cash.Raw < effect.BeforeRaw || cash.Raw-effect.BeforeRaw != moved {
			return budgetHold("redeposit_projection_conservation_mismatch")
		}
	}
	old, err := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	position, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw <= old.collateralDepositedRaw {
		return budgetHold("redeposit_projection_position_mismatch")
	}
	reserve, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	wantDebt, err := old.debtAtReserveRate(reserve)
	if err != nil {
		return err
	}
	bound, err := decodeKaminoPayoffWindow(p.Accounts, route, p.Slot, 3)
	if err != nil || bound.ObservedDebtRaw != wantDebt {
		return budgetHold("redeposit_projection_debt_changed")
	}
	clockSlot := binary.LittleEndian.Uint64(accountAt(p.Accounts, budgetClockAddress).Data[:8])
	if _, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino); err != nil {
		return err
	}
	for _, address := range []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		if binary.LittleEndian.Uint64(accountAt(p.Accounts, address).Data[16:24]) != clockSlot {
			return budgetHold("redeposit_projection_refresh_mismatch")
		}
	}
	a := accountAt(p.Accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
	if err != nil || a.Executable || a.Lamports == 0 || cash.Raw != 0 {
		return budgetHold("redeposit_projection_debt_custody_changed")
	}
	return nil
}

func observePhase3RedepositAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := o.Snapshot, e.Request
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || s.WithdrawalDemandRaw > 0 ||
		s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !positionReturnRoute(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 || s.DebtIdleRaw != 0 || s.CollateralIdleRaw <= 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || d.Action != OpenRouteStep || d.Action != r.Action || d.Reason != "single_loop_redeposit" || d.AmountRaw != s.CollateralIdleRaw || r.AmountRaw != uint64(d.AmountRaw) || e.ExpectedEffects.Deposit == nil {
		return phase3BridgeAdmission{}, budgetHold("complete_redeposit_return_unavailable")
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	route, _ := runtimeRoute(s.RouteLane)
	bound, before, err := observeRedepositPrestate(ctx, rpc, route, s, max(s.Slot, current.ObservationSlot))
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if e.ExpectedEffects.Accounts[0].BeforeRaw != uint64(s.CollateralIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("redeposit_custody_snapshot_changed")
	}
	projection, err := rpc.simulateKaminoEntryProjection(ctx, r, bound.ObservedSlot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if err := validateRedepositProjection(r, e.ExpectedEffects, before, projection); err != nil {
		return phase3BridgeAdmission{}, err
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err != nil {
		return plan, err
	}
	plan.DepositProjection, plan.FundingRelease, plan.BorrowRelease = &projection, plan.BorrowRelease, nil
	return plan, nil
}

func validateRedepositAdmissionPrestate(ctx context.Context, rpc *RPCClient, r KaminoPrimeUSDCRequest, p *phase3BridgeAdmission, slot int64) (int64, error) {
	if p == nil || p.DepositProjection == nil || p.Payoff == nil || p.Snapshot.RouteLane != r.RouteLane {
		return 0, budgetHold("redeposit_projection_identity_mismatch")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return 0, err
	}
	bound, _, err := observeRedepositPrestate(ctx, rpc, route, p.Snapshot, slot)
	if err != nil {
		return 0, err
	}
	if bound.MaximumRateBPS > p.Payoff.MaximumRateBPS || bound.InterestBasis != p.Payoff.InterestBasis || bound.ChainUnix < p.Payoff.ChainUnix || bound.ChainUnix > p.Payoff.ChainUnix+kaminoPayoffWindowSeconds || bound.UpperDebtRaw > p.Payoff.UpperDebtRaw || bound.ObservedSlot > math.MaxInt64-budgetMaxObservationLagSlots {
		return 0, budgetHold("redeposit_return_interest_window_changed")
	}
	return bound.ObservedSlot, nil
}
