package webapp

import "github.com/danielgtaylor/huma/v2"

var earnAutodepositFamily = earnFamily{
	name: "earn autodeposit",
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil && !h.PolicySigner.IsZero()
	},
	yieldGrants: append([]Grant{
		TableGrant("loyal_yield.balance_sweep_targets", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_targets", "INSERT"),
		TableGrant("loyal_yield.balance_sweep_targets", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_wallet_balances_current", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_wallet_balances_current", "INSERT"),
		TableGrant("loyal_yield.balance_sweep_wallet_balances_current", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_wallet_balance_events", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_wallet_balance_events", "INSERT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "INSERT"),
		TableGrant("loyal_yield.balance_sweep_surplus_lots", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_scheduled_slots", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_scheduled_slots", "INSERT"),
		TableGrant("loyal_yield.balance_sweep_scheduled_slots", "UPDATE"),
		// The close confirm releases the target's selected claims after
		// reading their items.
		TableGrant("loyal_yield.balance_sweep_lot_claims", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_lot_claims", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_lot_claim_items", "SELECT"),
		// The floor confirm draws the rebaseline event id from the reserved
		// negative floor sequence.
		SequenceGrant("loyal_yield.balance_sweep_floor_rebaseline_event_id_seq", "USAGE"),
		// The progress GET joins the execution and realtime event tables.
		TableGrant("loyal_yield.balance_sweep_executions", "SELECT"),
		TableGrant("loyal_yield.realtime_events", "SELECT"),
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
