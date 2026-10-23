package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/withdrawals/prepare  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/withdrawals/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/confirm/route.ts
var earnWithdrawFamily = earnFamily{
	name: "earn withdraw",
	// Confirm reads auth and chain state only; no table grants, and prepare
	// stays proxied to Next (the "earn withdraw prepare" family serves it).
	ready:    func(h earnRoutes) bool { return h.signedIn() && h.Chain != nil },
	register: earnRoutes.registerWithdraw,
}

func (h earnRoutes) registerWithdraw(api huma.API) {
	h.registerWithdrawConfirm(api)
	h.registerCleanupPrepare(api)
}
