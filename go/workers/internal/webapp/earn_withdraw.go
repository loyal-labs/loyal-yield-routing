package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/withdrawals/prepare  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/withdrawals/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/confirm/route.ts
var earnWithdrawFamily = earnFamily{
	name: "earn withdraw",
	// Confirm reads auth and chain state only. The cleanup pair adds the
	// vault state and full-withdrawal projection reads, and the one write
	// each — a stale autodeposit row reconciled closed (prepare) and the
	// recorded close intent (confirm) — over the sweep tables the close
	// cancel restores and suppresses.
	yieldGrants: append([]Grant{
		{"loyal_yield.user_yield_position_withdrawals", "SELECT"},
		{"loyal_yield.balance_sweep_targets", "SELECT"},
		{"loyal_yield.balance_sweep_targets", "UPDATE"},
		{"loyal_yield.balance_sweep_policies", "SELECT"},
		{"loyal_yield.balance_sweep_policies", "UPDATE"},
		{"loyal_yield.balance_sweep_executions", "SELECT"},
		{"loyal_yield.balance_sweep_surplus_lots", "SELECT"},
		{"loyal_yield.balance_sweep_surplus_lots", "UPDATE"},
		{"loyal_yield.balance_sweep_wallet_balance_events", "SELECT"},
		{"loyal_yield.balance_sweep_lot_claims", "SELECT"},
		{"loyal_yield.balance_sweep_lot_claims", "UPDATE"},
		{"loyal_yield.balance_sweep_lot_claim_items", "SELECT"},
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
