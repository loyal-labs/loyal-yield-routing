package webapp

// GET /api/smart-accounts/yield-optimization/earnings
// <- web/apps/web/src/app/api/smart-accounts/yield-optimization/earnings/route.ts
// (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/earnings).
// Authenticated read over the unit-1 read service: cached snapshot when fresh,
// stale-while-revalidate otherwise, with the detached rebuild standing in for
// Next's after().

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

var earnEarningsYieldGrants = []Grant{
	{"loyal_yield.earn_earnings_snapshots", "SELECT"},
	{"loyal_yield.earn_earnings_snapshots", "INSERT"},
	{"loyal_yield.earn_earnings_snapshots", "UPDATE"},
	{"loyal_yield.earn_history_day_revisions", "SELECT"},
	{"loyal_yield.managed_vaults", "SELECT"},
	{"loyal_yield.rebalance_decisions", "SELECT"},
	{"loyal_yield.user_yield_position_deposits", "SELECT"},
	{"loyal_yield.user_yield_position_holding_events", "SELECT"},
	{"loyal_yield.user_yield_position_withdrawals", "SELECT"},
	{"loyal_yield.user_yield_positions", "SELECT"},
	{"loyal_yield.vault_idle_token_balances_current", "SELECT"},
	{"loyal_yield.vault_position_snapshot_positions", "SELECT"},
	{"loyal_yield.vault_position_snapshots", "SELECT"},
}

var earnEarningsFamily = earnFamily{
	name:        "earn earnings",
	yieldGrants: earnEarningsYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.TimescaleURL != "" },
	register:    earnRoutes.registerEarnings,
}

// earningsRefreshBudget bounds the detached rebuild. The refresh lock's own
// 15s transaction window is the real cap; this is slack for the stale path.
const earningsRefreshBudget = 20 * time.Second

type earningsRequest struct {
	SessionInput
	Timezone string `query:"timezone" required:"false"`
}

type earningsOutput struct {
	// Next sets no explicit cache header on this route; no-store is pinned
	// here because every byte is per-wallet state.
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
		timezone:      in.Timezone,
		vaultIndex:    int16(earnVaultIndex),
		walletAddress: account.Wallet.String(),
	}
	payload, err := readCachedEarnEarningsRangeSet(ctx, h.YieldDB, input,
		h.scheduleEarningsRefresh(ctx), newEarningsReadDependencies(h.YieldDB, h.TimescaleURL, false))
	if err != nil {
		return nil, earningsRouteError(err)
	}
	return &earningsOutput{CacheControl: "no-store", Body: payload}, nil
}

// earningsRouteError maps a read-service failure onto the route's 503
// envelope exactly as the TS catch block does: a classified unavailable error
// keeps its code and detailCode, anything else degrades to
// earnings_unavailable.
func earningsRouteError(err error) error {
	var unavailable *earningsUnavailableError
	if errors.As(err, &unavailable) {
		return newEarningsError(unavailable.code, strPtr(unavailable.detailCode))
	}
	warnEarningsRead("route", err)
	return newEarningsError(earningsUnavailableCode, nil)
}
