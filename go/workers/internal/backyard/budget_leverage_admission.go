package backyard

import (
	"context"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func validateLeverageSwap(ctx context.Context, rpc *chain.Client, r JupiterSwapRequest, e ExpectedEffects, slot int64) (KaminoPayoffBound, []ConfirmedAccount, error) {
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
