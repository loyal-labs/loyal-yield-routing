package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/auth/privy/complete                                    <- web/apps/web/src/app/api/auth/privy/complete/route.ts + web/apps/web/src/features/identity/server/session-cookie.ts:138-167,291
var earnPrivyFamily = earnFamily{
	name:     "privy complete",
	ready:    notPorted,
	register: earnRoutes.registerPrivy,
}

func (h earnRoutes) registerPrivy(api huma.API) {}
