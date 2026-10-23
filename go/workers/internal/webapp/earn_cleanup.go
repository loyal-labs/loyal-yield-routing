package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/withdrawals/cleanup/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/cleanup/confirm/route.ts
var earnCleanupFamily = earnFamily{
	name:     "earn cleanup",
	ready:    notPorted,
	register: earnRoutes.registerCleanup,
}

func (h earnRoutes) registerCleanup(api huma.API) {}
