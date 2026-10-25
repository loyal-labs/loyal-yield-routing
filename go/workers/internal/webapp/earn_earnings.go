package webapp

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

var earnEarningsYieldGrants = []Grant{
	TableGrant("loyal_yield.earn_earnings_snapshots", "SELECT"),
	TableGrant("loyal_yield.earn_earnings_snapshots", "INSERT"),
	TableGrant("loyal_yield.earn_earnings_snapshots", "UPDATE"),
	TableGrant("loyal_yield.earn_history_day_revisions", "SELECT"),
	TableGrant("loyal_yield.managed_vaults", "SELECT"),
	TableGrant("loyal_yield.rebalance_decisions", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_deposits", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_holding_events", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_withdrawals", "SELECT"),
	TableGrant("loyal_yield.user_yield_positions", "SELECT"),
	TableGrant("loyal_yield.vault_idle_token_balances_current", "SELECT"),
	TableGrant("loyal_yield.vault_position_snapshot_positions", "SELECT"),
	TableGrant("loyal_yield.vault_position_snapshots", "SELECT"),
}

var earnEarningsFamily = earnFamily{
	name:        "earn earnings",
	yieldGrants: earnEarningsYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.Timescale != nil },
	register:    earnRoutes.registerEarnings,
}

const earningsRefreshBudget = 5 * time.Minute

type earningsRequest struct {
	SessionInput
	Timezone string `query:"timezone" required:"false"`
}

type earningsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EarningsRangeSet
}

func (h earnRoutes) registerEarnings(api huma.API) {
	responses := errorResponses[routeError](api, 401)
	maps.Copy(responses, errorResponses[earningsError](api, 503))
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-earnings",
		Method:      http.MethodGet,
		Path:        "/api/smart-accounts/yield-optimization/earnings",
		Summary:     "Earn earnings bars and lifetime totals for the signed-in wallet",
		Responses:   responses,
	}, h.earnEarnings)
}

// scheduleEarningsRefresh is the Go stand-in for Next's after(): the stale
// rebuild runs detached from the request context so the response is not held.
func (h earnRoutes) scheduleEarningsRefresh(ctx context.Context) earningsScheduleRefresh {
	return func(task func(context.Context)) {
		go func() {
			runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), earningsRefreshBudget)
			defer cancel()
			task(runCtx)
		}()
	}
}

func (h earnRoutes) earnEarnings(ctx context.Context, in *earningsRequest) (*earningsOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		warnEarningsRead("auth", err)
		return nil, newEarningsError(earningsUnavailableCode, nil)
	}
	input := earningsReadInput{
		cluster:       string(account.Cluster),
		settings:      account.Settings.String(),
		timezone:      normalizeEarningsTimezone(in.Timezone),
		vaultIndex:    int16(earnprep.EarnVaultIndex),
		walletAddress: account.Wallet.String(),
	}
	payload, err := readCachedEarnEarningsRangeSet(ctx, h.YieldDB, input,
		h.scheduleEarningsRefresh(ctx), newEarningsReadDependencies(h.YieldDB, h.Timescale, false))
	if err != nil {
		return nil, earningsRouteError(err)
	}
	return &earningsOutput{CacheControl: perWalletCacheControl(), Body: payload}, nil
}

func earningsRouteError(err error) error {
	var unavailable *earningsUnavailableError
	if errors.As(err, &unavailable) {
		return newEarningsError(unavailable.code, strPtr(unavailable.detailCode))
	}
	warnEarningsRead("route", err)
	return newEarningsError(earningsUnavailableCode, nil)
}
