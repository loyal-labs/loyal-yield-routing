package webapp

import "github.com/danielgtaylor/huma/v2"

// earnDepositYieldGrants includes the quest-table writes because the confirm
// op itself reports quest completion.
var earnDepositYieldGrants = []Grant{
	TableGrant("loyal_yield.managed_vaults", "SELECT"),
	TableGrant("loyal_yield.route_policies", "SELECT"),
	TableGrant("loyal_yield.solana_week_quest_completions", "SELECT"),
	TableGrant("loyal_yield.solana_week_quest_completions", "INSERT"),
	TableGrant("loyal_yield.solana_week_quest_completions", "UPDATE"),
}

var earnDepositAppGrants = earnPrivyFamily.appGrants

var earnDepositFamily = earnFamily{
	name:        "earn deposit",
	appGrants:   earnDepositAppGrants,
	yieldGrants: earnDepositYieldGrants,
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.Chain != nil && h.YieldDB != nil && !h.PolicySigner.IsZero()
	},
	register: earnRoutes.registerDeposit,
}

func (h earnRoutes) registerDeposit(api huma.API) {
	h.registerPoliciesPrepare(api)
	h.registerPoliciesConfirm(api)
	h.registerDepositsConfirm(api)
	h.registerEarnMaxContext(api)
}
