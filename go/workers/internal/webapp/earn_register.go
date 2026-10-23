package webapp

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type earnFamily struct {
	name        string
	appGrants   []Grant
	yieldGrants []Grant
	ready       func(earnRoutes) bool
	register    func(earnRoutes, huma.API)
}

var earnFamilies = []earnFamily{
	earnStateFamily,
	earnEarningsFamily,
	earnTransactionsFamily,
	earnPublicFamily,
	earnObservabilityFamily,
	earnSessionFamily,
	earnPrivyFamily,
	earnDepositFamily,
	earnWithdrawFamily,
	earnAutodepositFamily,
	earnRefundsFamily,
	earnWprepFamily,
	earnDprepFamily,
}

func registerEarn(api huma.API, h earnRoutes) {
	for _, family := range earnFamilies {
		if h.Granted[family.name] && family.ready(h) {
			family.register(h, api)
		}
	}
}

func documentEarn(api huma.API) {
	for _, family := range earnFamilies {
		family.register(earnRoutes{}, api)
	}
}

func CheckEarnGrants(ctx context.Context, appDB, yieldDB *pgxpool.Pool) map[string]bool {
	granted := map[string]bool{}
	for _, family := range earnFamilies {
		granted[family.name] = familyGranted(ctx, family.name, appDB, family.appGrants) && familyGranted(ctx, family.name, yieldDB, family.yieldGrants)
	}
	return granted
}

func familyGranted(ctx context.Context, family string, pool *pgxpool.Pool, want []Grant) bool {
	if len(want) == 0 {
		return true
	}
	if pool == nil {
		slog.Warn(family+" not served by Go: its database is not configured", "grants", want)
		return false
	}
	missing, err := MissingGrants(ctx, pool, want)
	switch {
	case err != nil:
		slog.Warn(family+" not served by Go: grant check failed", "error", err)
	case len(missing) > 0:
		slog.Warn(family+" not served by Go: role lacks grants", "missing", missing)
	default:
		return true
	}
	return false
}
