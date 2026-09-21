package backyardrwa

import (
	"bytes"
	"context"
	"math"
	"time"
)

func validateLeverageSwap(ctx context.Context, rpc *RPCClient, r JupiterSwapRequest, e ExpectedEffects, slot int64) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if !r.PositionReturnReserved || r.Action != SwapDebtToCollateralStep || r.EntryReturnReserved || r.FullPayoffFunding || len(e.Accounts) != 2 {
		return KaminoPayoffBound{}, nil, budgetHold("leverage_swap_intent_mismatch")
	}
	if _, err := MeasureExecutableDebit(r, e); err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	var additional []string
	if route.Lane == autoAUTOPYUSD.Lane {
		additional = append(additional, route.Kamino.Market)
	}
	bound, accounts, err := observeKaminoPayoffWindowAccounts(ctx, rpc, route, slot, 3, additional...)
	if err != nil {
		return bound, nil, err
	}
	position, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw == 0 {
		return bound, nil, budgetHold("leverage_swap_position_changed")
	}
	for i, row := range []struct{ address, mint string }{{route.DebtCustody, route.Kamino.DebtMint}, {route.CollateralCustody, route.Kamino.CollateralMint}} {
		a := accountAt(accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		effect := e.Accounts[i]
		if err != nil || a.Executable || a.Lamports == 0 || effect.Address != row.address || effect.Mint != row.mint || effect.Owner != a.Owner || effect.Authority != bridgeVault || effect.BeforeRaw != cash.Raw {
			return bound, nil, budgetHold("leverage_swap_custody_changed")
		}
		if i == 0 {
			if cash.Raw != r.AmountRaw || effect.AfterRaw != 0 {
				return bound, nil, budgetHold("leverage_swap_requires_complete_debt_buffer")
			}
		} else if r.MinimumOutputRaw > math.MaxInt64 || cash.Raw > math.MaxInt64-r.MinimumOutputRaw || effect.MinimumAfterRaw == nil || *effect.MinimumAfterRaw != cash.Raw+r.MinimumOutputRaw || effect.AfterRaw != *effect.MinimumAfterRaw {
			return bound, nil, budgetHold("leverage_swap_output_mismatch")
		}
	}
	return bound, accounts, nil
}

func validateLeverageProjection(r JupiterSwapRequest, e ExpectedEffects, before []ConfirmedAccount, p phase3KaminoProjection) error {
	message, err := CompileJupiterMessage(r)
	if err != nil || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 {
		return budgetHold("leverage_projection_identity_mismatch")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return err
	}
	for _, address := range []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve, route.CollateralLiquiditySupply, route.DebtLiquiditySupply} {
		a, b := accountAt(before, address), accountAt(p.Accounts, address)
		if a.Owner != b.Owner || a.Lamports != b.Lamports || a.Executable != b.Executable || len(a.Data) == 0 || !bytes.Equal(a.Data, b.Data) {
			return budgetHold("leverage_projection_position_changed")
		}
	}
	for i, effect := range e.Accounts {
		a := accountAt(p.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Owner != effect.Owner || a.Executable || a.Lamports == 0 || cash.Raw > math.MaxInt64 || (i == 0 && cash.Raw != 0) || (i == 1 && cash.Raw < effect.AfterRaw) {
			return budgetHold("leverage_projection_custody_mismatch")
		}
	}
	_, err = decodeKaminoPayoffWindow(p.Accounts, route, p.Slot, 3)
	return err
}

func observePhase3LeverageSwapAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := o.Snapshot, e.Request
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || s.WithdrawalDemandRaw > 0 ||
		s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !positionReturnRoute(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 || debtCashRaw(s) <= 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || d.Action != SwapDebtToCollateralStep || d.Action != r.Action || d.AmountRaw != debtCashRaw(s) || r.AmountRaw != uint64(d.AmountRaw) {
		return phase3BridgeAdmission{}, budgetHold("complete_leverage_swap_return_unavailable")
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	bound, before, err := validateLeverageSwap(ctx, rpc, r, e.ExpectedEffects, max(s.Slot, current.ObservationSlot))
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	route, _ := runtimeRoute(s.RouteLane)
	position, err := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || bound.ObservedDebtRaw != uint64(s.PositionDebtRaw) || e.ExpectedEffects.Accounts[1].BeforeRaw != uint64(s.CollateralIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("leverage_swap_snapshot_changed")
	}
	message, err := CompileJupiterMessage(r)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	var addresses []string
	for _, a := range before {
		addresses = append(addresses, a.Address)
	}
	projection, err := rpc.simulatePhase3EntryProjection(ctx, message, addresses, bound.ObservedSlot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if err := validateLeverageProjection(r, e.ExpectedEffects, before, projection); err != nil {
		return phase3BridgeAdmission{}, err
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err != nil {
		return plan, err
	}
	plan.LeverageProjection, plan.FundingRelease, plan.BorrowRelease = &projection, plan.BorrowRelease, nil
	return plan, nil
}

func (db *Database) admitPhase3LeverageSwap(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, id string, o Observation, d Decision, e JupiterExecutionEvidence) error {
	plan, err := observePhase3LeverageSwapAdmission(ctx, rpc, client, m, o, d, e)
	if err != nil {
		return err
	}
	return db.persistPhase3ExitAdmission(ctx, rpc, id, o, d, plan)
}

func validateLeverageAdmissionPrestate(ctx context.Context, rpc *RPCClient, r JupiterSwapRequest, e ExpectedEffects, p *phase3BridgeAdmission, slot int64) (int64, error) {
	if p == nil || p.LeverageProjection == nil || p.Payoff == nil || p.Snapshot.RouteLane != r.RouteLane {
		return 0, budgetHold("leverage_projection_identity_mismatch")
	}
	bound, accounts, err := validateLeverageSwap(ctx, rpc, r, e, slot)
	if err != nil {
		return 0, err
	}
	route, _ := runtimeRoute(r.RouteLane)
	position, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw != uint64(p.Snapshot.PositionCollateralRaw) || bound.ObservedDebtRaw != uint64(p.Snapshot.PositionDebtRaw) {
		return 0, budgetHold("leverage_swap_snapshot_changed")
	}
	if bound.MaximumRateBPS > p.Payoff.MaximumRateBPS || bound.InterestBasis != p.Payoff.InterestBasis || bound.ChainUnix < p.Payoff.ChainUnix || bound.ChainUnix > p.Payoff.ChainUnix+kaminoPayoffWindowSeconds || bound.UpperDebtRaw > p.Payoff.UpperDebtRaw {
		return 0, budgetHold("leverage_return_interest_window_changed")
	}
	return bound.ObservedSlot, nil
}
