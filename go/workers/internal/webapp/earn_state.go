package webapp

import "github.com/danielgtaylor/huma/v2"

// GET  /api/smart-accounts/yield-optimization/earn-state           <- web/apps/web/src/app/api/smart-accounts/yield-optimization/earn-state/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/state, web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/state)
// GET  /api/smart-accounts/yield-optimization/position             <- web/apps/web/src/app/api/smart-accounts/yield-optimization/position/route.ts
// POST /api/smart-accounts/yield-optimization/position/reconcile   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/position/reconcile/route.ts
// POST /api/smart-accounts/yield-optimization/realtime/token       <- web/apps/web/src/app/api/smart-accounts/yield-optimization/realtime/token/route.ts
var earnStateFamily = earnFamily{
	name:     "earn state",
	ready:    notPorted,
	register: earnRoutes.registerState,
}

func (h earnRoutes) registerState(api huma.API) {}
