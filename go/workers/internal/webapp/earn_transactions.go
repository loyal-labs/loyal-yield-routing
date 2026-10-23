package webapp

import "github.com/danielgtaylor/huma/v2"

// GET  /api/smart-accounts/earn-transactions                       <- web/apps/web/src/app/api/smart-accounts/earn-transactions/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/transactions)
// POST /api/smart-accounts/yield-optimization/policy-refunds/scan  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/scan/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/policy-refunds/scan, GET)
var earnTransactionsFamily = earnFamily{
	name:     "earn transactions",
	ready:    notPorted,
	register: earnRoutes.registerTransactions,
}

func (h earnRoutes) registerTransactions(api huma.API) {}
