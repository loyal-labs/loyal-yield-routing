package webapp

import "github.com/danielgtaylor/huma/v2"

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
		TableGrant("loyal_yield.balance_sweep_lot_claims", "SELECT"),
		TableGrant("loyal_yield.balance_sweep_lot_claims", "UPDATE"),
		TableGrant("loyal_yield.balance_sweep_lot_claim_items", "SELECT"),
		SequenceGrant("loyal_yield.balance_sweep_floor_rebaseline_event_id_seq", "USAGE"),
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
