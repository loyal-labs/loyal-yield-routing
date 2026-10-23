package webapp

import "github.com/danielgtaylor/huma/v2"

// GET  /api/smart-accounts/yield-optimization/earnings             <- web/apps/web/src/app/api/smart-accounts/yield-optimization/earnings/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/earnings)
var earnEarningsFamily = earnFamily{
	name:     "earn earnings",
	ready:    notPorted,
	register: earnRoutes.registerEarnings,
}

func (h earnRoutes) registerEarnings(api huma.API) {}
