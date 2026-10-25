package webapp

import "github.com/danielgtaylor/huma/v2"

var earnWithdrawFamily = earnFamily{
	name: "earn withdraw",
	// The only writes beyond reads: prepare reconciles a stale autodeposit
	// row closed; confirm records the close intent.
	yieldGrants: append([]Grant{
		TableGrant("loyal_yield.user_yield_position_withdrawals", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_targets", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_targets", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_policies", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_policies", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_executions", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_wallet_balance_events", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_lot_claims", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_lot_claims", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_lot_claim_items", "SELECT"),
	}, EarnPolicyGrants...),
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil && !h.PolicySigner.IsZero()
	},
	register: earnRoutes.registerWithdraw,
}

func (h earnRoutes) registerWithdraw(api huma.API) {
	h.registerWithdrawConfirm(api)
	h.registerCleanupPrepare(api)
	h.registerCleanupConfirm(api)
}
