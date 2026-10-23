package webapp

import "github.com/danielgtaylor/huma/v2"

// GET /api/smart-accounts/earn-forecast/summary                   <- web/apps/web/src/app/api/smart-accounts/earn-forecast/summary/route.ts (public)
// GET /api/solana-week/progress                                   <- web/apps/web/src/app/api/solana-week/progress/route.ts (Principal instead of walletAddress)
// GET /api/earn-max/apy is not ported: its RN caller is gone and the web
// caller (facelift/sidebar.tsx) is out of scope for this lane.
var earnPublicFamily = earnFamily{
	name:        "earn public reads",
	appGrants:   AuthGrants,
	yieldGrants: earnPublicYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil },
	register:    earnRoutes.registerPublic,
}

var earnPublicYieldGrants = []Grant{
	{"loyal_yield.earn_reserve_share_prices", "SELECT"},
	{"loyal_yield.earn_fleet_allocations_hourly", "SELECT"},
	{"loyal_yield.solana_week_quest_completions", "SELECT"},
}

func (h earnRoutes) registerPublic(api huma.API) {
	h.registerForecast(api)
	h.registerQuestProgress(api)
}
