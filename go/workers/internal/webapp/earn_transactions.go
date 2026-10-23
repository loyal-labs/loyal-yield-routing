package webapp

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// GET  /api/smart-accounts/earn-transactions                       <- web/apps/web/src/app/api/smart-accounts/earn-transactions/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/transactions)
// POST /api/smart-accounts/yield-optimization/policy-refunds/scan  <- web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/scan/route.ts (mobile twin web/apps/web/src/app/api/smart-accounts/mobile/earn/policy-refunds/scan, GET)
var earnTransactionsFamily = earnFamily{
	name:        "earn transactions",
	appGrants:   nil,
	yieldGrants: earnTransactionYieldGrants,
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil
	},
	register: earnRoutes.registerTransactions,
}

// Every loyal_yield table the earn-transactions read path touches, including
// the sync write it runs first (user_yield_position_holding_events INSERT,
// user_yield_positions UPDATE).
var earnTransactionYieldGrants = []Grant{
	{"loyal_yield.balance_sweep_executions", "SELECT"},
	{"loyal_yield.balance_sweep_targets", "SELECT"},
	{"loyal_yield.managed_vaults", "SELECT"},
	{"loyal_yield.rebalance_decisions", "SELECT"},
	{"loyal_yield.route_policies", "SELECT"},
	{"loyal_yield.user_yield_position_deposits", "SELECT"},
	{"loyal_yield.user_yield_position_holding_events", "INSERT"},
	{"loyal_yield.user_yield_position_holding_events", "SELECT"},
	{"loyal_yield.user_yield_position_withdrawals", "SELECT"},
	{"loyal_yield.user_yield_positions", "SELECT"},
	{"loyal_yield.user_yield_positions", "UPDATE"},
	{"loyal_yield.vault_position_snapshot_positions", "SELECT"},
	{"loyal_yield.vault_position_snapshots", "SELECT"},
}

const earnTransactionsPath = "/api/smart-accounts/earn-transactions"

// EarnTransactionAccount and SerializedEarnTransaction mirror
// @loyal-labs/shared earn-transactions.ts; the Next
// earn-transactions/formatter.ts emits exactly these fields, icons null when
// the formatter says null.
type EarnTransactionAccount struct {
	Icon  *string `json:"icon"`
	Label string  `json:"label"`
}

type SerializedEarnTransaction struct {
	Amount        string                 `json:"amount"`
	ConfirmedAt   string                 `json:"confirmedAt"`
	ConfirmedSlot string                 `json:"confirmedSlot"`
	DateGroup     string                 `json:"dateGroup"`
	Destination   EarnTransactionAccount `json:"destination"`
	EventType     string                 `json:"eventType"`
	ID            string                 `json:"id"`
	Kind          string                 `json:"kind"`
	LiquidityMint *string                `json:"liquidityMint"`
	RawAmount     string                 `json:"rawAmount"`
	Signature     string                 `json:"signature"`
	SortTimestamp string                 `json:"sortTimestamp"`
	Source        EarnTransactionAccount `json:"source"`
	Timestamp     string                 `json:"timestamp"`
}

type EarnTransactionsResponse struct {
	Transactions []SerializedEarnTransaction `json:"transactions"`
}

type earnTransactionsOutput struct {
	Body EarnTransactionsResponse
}

func (h earnRoutes) registerTransactions(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-transactions",
		Method:      http.MethodGet,
		Path:        earnTransactionsPath,
		Summary:     "Earn transaction history for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 401, 503),
	}, h.earnTransactions)
}

// earnTransactions ports the Next earn-transactions route: sync the confirmed
// rebalance holding events, then serve the position and autodeposit history
// through the formatter's serializer, collapse and ordering.
func (h earnRoutes) earnTransactions(ctx context.Context, in *sessionRequest) (*earnTransactionsOutput, error) {
	const unavailableCode = "earn_transactions_unavailable"
	const unavailableMessage = "Earn transactions are unavailable."

	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn transactions session", err, unavailableCode, unavailableMessage)
	}
	if err := h.syncRebalanceHoldingEvents(ctx, account); err != nil {
		return nil, earnUnavailable("sync rebalance holding events", err, unavailableCode, unavailableMessage)
	}
	positionEvents, err := h.yieldPositionHistoryEvents(ctx, account)
	if err != nil {
		return nil, earnUnavailable("position history", err, unavailableCode, unavailableMessage)
	}
	autodepositEvents, err := h.autodepositHistoryEvents(ctx, account)
	if err != nil {
		return nil, earnUnavailable("autodeposit history", err, unavailableCode, unavailableMessage)
	}

	depositSignatures := map[string]struct{}{}
	for _, event := range autodepositEvents {
		if event.depositSignature != nil {
			depositSignatures[*event.depositSignature] = struct{}{}
		}
	}
	transactions := make([]SerializedEarnTransaction, 0, len(positionEvents)+len(autodepositEvents))
	for _, event := range positionEvents {
		if event.typ == "reconciliation" {
			continue
		}
		if _, swept := depositSignatures[event.signature]; event.signature != "" && swept {
			continue
		}
		transactions = append(transactions, txreadsSerializePositionEvent(event))
	}
	for _, event := range autodepositEvents {
		transactions = append(transactions, txreadsSerializeAutodepositEvent(event))
	}
	transactions = collapseDuplicateRebalanceTransactions(transactions)
	sortEarnTransactions(transactions)
	return &earnTransactionsOutput{Body: EarnTransactionsResponse{Transactions: transactions}}, nil
}
