package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/deposits/prepare     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/deposits/prepare/route.ts (+ web/apps/web/src/app/api/smart-accounts/mobile/earn/deposit/prepare account gate)
// POST /api/smart-accounts/yield-optimization/deposits/confirm     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/deposits/confirm/route.ts (+ web/apps/web/src/app/api/smart-accounts/mobile/earn/deposit/confirm quest report)
// POST /api/smart-accounts/yield-optimization/policies/prepare     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/policies/confirm     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/confirm/route.ts
// POST /api/smart-accounts/earn-max/context                        <- web/apps/web/src/app/api/smart-accounts/mobile/earn-max/route.ts POST
var earnDepositFamily = earnFamily{
	name: "earn deposit",
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.Chain != nil && !h.PolicySigner.IsZero()
	},
	register: earnRoutes.registerDeposit,
}

func (h earnRoutes) registerDeposit(api huma.API) {
	h.registerPoliciesPrepare(api)
}
