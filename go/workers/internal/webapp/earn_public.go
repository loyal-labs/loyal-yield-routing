package webapp

import "github.com/danielgtaylor/huma/v2"

// GET  /api/smart-accounts/earn-forecast/summary                   <- web/apps/web/src/app/api/smart-accounts/earn-forecast/summary/route.ts
// GET  /api/earn-max/apy                                           <- web/apps/web/src/app/api/earn-max/apy/route.ts (only if a web caller still needs it; else the Next route goes)
// GET  /api/solana-week/progress                                   <- web/apps/web/src/app/api/solana-week/progress/route.ts (walletAddress param becomes Principal)
var earnPublicFamily = earnFamily{
	name:     "earn public reads",
	ready:    notPorted,
	register: earnRoutes.registerPublic,
}

func (h earnRoutes) registerPublic(api huma.API) {}
