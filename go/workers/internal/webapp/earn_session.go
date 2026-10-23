package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/earn/session                            <- web/apps/web/src/app/api/smart-accounts/mobile/earn/session/route.ts + web/apps/web/src/features/identity/server/mobile-earn-session.ts:28-53
var earnSessionFamily = earnFamily{
	name:     "earn session",
	ready:    notPorted,
	register: earnRoutes.registerSession,
}

func (h earnRoutes) registerSession(api huma.API) {}
