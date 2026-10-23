package webapp

// POST /api/smart-accounts/yield-optimization/policy-refunds/prepare      <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/policy-refunds/scan         <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/scan/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/toggle           <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/toggle/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/delete/readiness <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/readiness/route.ts
var earnRefundsFamily = earnFamily{
	name: "earn refunds and cross-mint",
	// appGrants: the principal read (auth.go:254-273). yieldGrants: the vault
	// refund liveness read (refunds_snapshot.go).
	appGrants:   AuthGrants,
	yieldGrants: []Grant{{"loyal_yield.user_yield_positions", "SELECT"}, {"loyal_yield.managed_vaults", "SELECT"}, {"loyal_yield.balance_sweep_targets", "SELECT"}},
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil
	},
	register: earnRoutes.registerRefunds,
}
