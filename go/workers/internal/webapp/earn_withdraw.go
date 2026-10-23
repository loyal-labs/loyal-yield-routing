package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/withdrawals/prepare  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/withdrawals/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/confirm/route.ts
var earnWithdrawFamily = earnFamily{
	name:     "earn withdraw",
	ready:    notPorted,
	register: earnRoutes.registerWithdraw,
}

func (h earnRoutes) registerWithdraw(api huma.API) {}
