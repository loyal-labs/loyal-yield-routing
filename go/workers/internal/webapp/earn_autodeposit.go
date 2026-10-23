package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/autodeposit/setup/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/setup/confirm)  [served: autodeposit_setup.go]
// POST /api/smart-accounts/yield-optimization/autodeposit/close/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/close/confirm/route.ts  [served: autodeposit_confirm.go]
// POST /api/smart-accounts/yield-optimization/autodeposit/floor/confirm   <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/floor/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/floor/confirm)  [served: autodeposit_confirm.go]
// POST /api/smart-accounts/yield-optimization/autodeposit/toggle/confirm  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm/route.ts (+ push from web/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/toggle/confirm)  [served: autodeposit_confirm.go]
// POST /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts  [served: autodeposit_sweeps.go]
// GET  /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts (progress by slotId)  [served: autodeposit_sweeps.go]
var earnAutodepositFamily = earnFamily{
	name: "earn autodeposit",
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil && !h.PolicySigner.IsZero()
	},
	// The setup confirm reads the target row, then writes the intent and the
	// bootstrap sweep state machine (targets, wallet projections, events,
	// surplus lots, scheduled slots); lot claims are joined when the scheduled
	// sweep is re-read. The close confirm releases the target's selected
	// claims (UPDATE) after reading their items; the floor confirm draws the
	// rebaseline event id from the reserved negative floor sequence. The
	// sweeps execute op reads the target, gates on the active route policy
	// pair, flips the chosen slot to "requested" (slots, lots UPDATE) and the
	// progress GET joins the execution and realtime event tables.
	yieldGrants: append([]Grant{
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
		{"loyal_yield.balance_sweep_lot_claims", "UPDATE"},
		{"loyal_yield.balance_sweep_lot_claim_items", "SELECT"},
		{"loyal_yield.balance_sweep_floor_rebaseline_event_id_seq", "USAGE"},
		{"loyal_yield.balance_sweep_executions", "SELECT"},
		{"loyal_yield.realtime_events", "SELECT"},
	}, EarnPolicyGrants...),
	register: earnRoutes.registerAutodeposit,
}

func (h earnRoutes) registerAutodeposit(api huma.API) {
	h.registerAutodepositSetup(api)
	h.registerAutodepositClose(api)
	h.registerAutodepositFloor(api)
	h.registerAutodepositToggle(api)
	h.registerAutodepositSweeps(api)
}
