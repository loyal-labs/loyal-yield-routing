package webapp

// POST /api/smart-accounts/yield-optimization/policy-refunds/prepare      <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/policy-refunds/scan         <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/scan/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/toggle           <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/toggle/route.ts
// POST /api/smart-accounts/yield-optimization/cross-mint/delete/readiness <- web/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/readiness/route.ts
var earnRefundsFamily = earnFamily{
	name: "earn refunds and cross-mint",
	// appGrants: the principal read (auth.go:254-273). yieldGrants: the vault
	// refund liveness read (refunds_snapshot.go), the policy and delegation
	// refund scan reads (txreads_refundscan.go) and the cross-mint opt-in
	// read/write plus policy and movement reads (refunds_crossmint_state.go).
	appGrants: AuthGrants,
	yieldGrants: []Grant{TableGrant("loyal_yield.user_yield_positions", "SELECT"), TableGrant("loyal_yield.managed_vaults", "SELECT"), TableGrant("loyal_yield.balance_sweep_targets", "SELECT"),
		TableGrant("loyal_yield.cross_mint_vault_opt_ins", "SELECT"), TableGrant("loyal_yield.cross_mint_vault_opt_ins", "UPDATE"),
		TableGrant("loyal_yield.cross_mint_swap_policies", "SELECT"), TableGrant("loyal_yield.rebalance_decisions", "SELECT"),
		TableGrant("loyal_yield.route_policies", "SELECT"), TableGrant("loyal_yield.balance_sweep_policies", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "SELECT"), TableGrant("loyal_yield.balance_sweep_lot_claims", "SELECT")},
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil
	},
	register: earnRoutes.registerRefunds,
}
