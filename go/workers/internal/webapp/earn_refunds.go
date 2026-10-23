package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/policy-refunds/prepare      <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/toggle           <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/toggle/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/delete/readiness <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/readiness/route.ts
var earnRefundsFamily = earnFamily{
	name:     "earn refunds and cross-mint",
	ready:    notPorted,
	register: earnRoutes.registerRefunds,
}

func (h earnRoutes) registerRefunds(api huma.API) {}
