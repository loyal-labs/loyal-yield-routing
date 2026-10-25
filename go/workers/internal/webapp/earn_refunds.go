package webapp

var earnRefundsFamily = earnFamily{
	name:      "earn refunds and cross-mint",
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
