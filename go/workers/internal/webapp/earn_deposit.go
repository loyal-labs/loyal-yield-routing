package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/smart-accounts/yield-optimization/deposits/prepare     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/deposits/prepare/route.ts (+ web/apps/web/src/app/api/smart-accounts/mobile/earn/deposit/prepare account gate)
// POST /api/smart-accounts/yield-optimization/deposits/confirm     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/deposits/confirm/route.ts (+ web/apps/web/src/app/api/smart-accounts/mobile/earn/deposit/confirm quest report)
// POST /api/smart-accounts/yield-optimization/policies/prepare     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/prepare/route.ts
// POST /api/smart-accounts/yield-optimization/policies/confirm     <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/confirm/route.ts
// POST /api/smart-accounts/earn-max/context                        <- web/apps/web/src/app/api/smart-accounts/mobile/earn-max/route.ts POST
// earnDepositYieldGrants covers the confirm unit's reads and writes: the
// policy pair read behind the citation (EARN.md's activePolicyPair) and the
// mobile quest report's solana_week_quest_completions rows.
var earnDepositYieldGrants = []Grant{
	{"loyal_yield.managed_vaults", "SELECT"},
	{"loyal_yield.route_policies", "SELECT"},
	{"loyal_yield.solana_week_quest_completions", "SELECT"},
	{"loyal_yield.solana_week_quest_completions", "INSERT"},
	{"loyal_yield.solana_week_quest_completions", "UPDATE"},
}

// earnDepositAppGrants is the earn-max context op's app schema footprint:
// the find-or-create app user (privy_provisioning.go) and the smart-account
// rows the sponsored-provisioning seam reads and writes — the same set the
// privy-complete family declares for the same helpers.
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
