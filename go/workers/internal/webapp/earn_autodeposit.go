package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/autodeposit/setup/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/setup/confirm)  [served: autodeposit_setup.go]
// POST /api/smart-accounts/yield-optimization/autodeposit/close/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/close/confirm/route.ts
// POST /api/smart-accounts/yield-optimization/autodeposit/floor/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/floor/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/floor/confirm)
// POST /api/smart-accounts/yield-optimization/autodeposit/toggle/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/toggle/confirm)
// POST /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts
// GET  /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts (progress by slotId)
var earnAutodepositFamily = earnFamily{
	name: "earn autodeposit",
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil && !h.PolicySigner.IsZero()
	},
	// The setup confirm reads the target row, then writes the intent and the
	// bootstrap sweep state machine (targets, wallet projections, events,
	// surplus lots, scheduled slots); lot claims are joined when the scheduled
	// sweep is re-read.
	yieldGrants: []Grant{
		{"loyal_yield.balance_sweep_targets", "SELECT"},
		{"loyal_yield.balance_sweep_targets", "INSERT"},
		{"loyal_yield.balance_sweep_targets", "UPDATE"},
		{"loyal_yield.balance_sweep_wallet_balances_current", "SELECT"},
		{"loyal_yield.balance_sweep_wallet_balances_current", "INSERT"},
		{"loyal_yield.balance_sweep_wallet_balances_current", "UPDATE"},
		{"loyal_yield.balance_sweep_wallet_balance_events", "SELECT"},
		{"loyal_yield.balance_sweep_wallet_balance_events", "INSERT"},
		{"loyal_yield.balance_sweep_surplus_lots", "SELECT"},
		{"loyal_yield.balance_sweep_surplus_lots", "INSERT"},
		{"loyal_yield.balance_sweep_surplus_lots", "UPDATE"},
		{"loyal_yield.balance_sweep_scheduled_slots", "SELECT"},
		{"loyal_yield.balance_sweep_scheduled_slots", "INSERT"},
		{"loyal_yield.balance_sweep_scheduled_slots", "UPDATE"},
		{"loyal_yield.balance_sweep_lot_claims", "SELECT"},
	},
	register: earnRoutes.registerAutodeposit,
}

func (h earnRoutes) registerAutodeposit(api huma.API) {
	h.registerAutodepositSetup(api)
}
