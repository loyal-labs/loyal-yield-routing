package webapp

import (
	"context"
	"log/slog"
	"sync"

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
	earnWalletAuthFamily,
	earnAuthSessionFamily,
	earnDepositFamily,
	earnWithdrawFamily,
	earnAutodepositFamily,
	earnRefundsFamily,
	earnWprepFamily,
	earnDprepFamily,
	earnEvmDepositFamily,
	earnTronDepositFamily,
	earnBridgeFamily,
	earnReferralsFamily,
	earnLaunchFamily,
	earnMixpanelCohortFamily,
	earnSweepNotifyFamily,
	earnOverviewFamily,
	earnDeskEarningsFamily,
	earnDeskHoldingsFamily,
	earnLegacyStateFamily,
	earnLegacyTransactionsFamily,
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
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, family := range earnFamilies {
		wg.Go(func() {
			ok := familyGranted(ctx, family.name, appDB, family.appGrants) && familyGranted(ctx, family.name, yieldDB, family.yieldGrants)
			mu.Lock()
			granted[family.name] = ok
			mu.Unlock()
		})
	}
	wg.Wait()
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
	return GrantsHeld(ctx, pool, family, want)
}
