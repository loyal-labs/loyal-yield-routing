package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/autodeposit/setup/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/setup/confirm)
// POST /api/smart-accounts/yield-optimization/autodeposit/close/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/close/confirm/route.ts
// POST /api/smart-accounts/yield-optimization/autodeposit/floor/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/floor/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/floor/confirm)
// POST /api/smart-accounts/yield-optimization/autodeposit/toggle/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/toggle/confirm)
// POST /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts
// GET  /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts (progress by slotId)
var earnAutodepositFamily = earnFamily{
	name:     "earn autodeposit",
	ready:    notPorted,
	register: earnRoutes.registerAutodeposit,
}

func (h earnRoutes) registerAutodeposit(api huma.API) {}
