package webapp

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

var earnTransactionsFamily = earnFamily{
	name:        "earn transactions",
	appGrants:   nil,
	yieldGrants: earnTransactionYieldGrants,
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil
	},
	register: earnRoutes.registerTransactions,
}

// Every loyal_yield table the earn-transactions and policy-refund-scan read
// paths touch, including the sync write the history runs first
// (user_yield_position_holding_events INSERT, user_yield_positions UPDATE).
var earnTransactionYieldGrants = []Grant{
	TableGrant("loyal_yield.balance_sweep_executions", "SELECT"),
	TableGrant("loyal_yield.balance_sweep_lot_claims", "SELECT"),
	TableGrant("loyal_yield.balance_sweep_policies", "SELECT"),
	TableGrant("loyal_yield.balance_sweep_surplus_lots", "SELECT"),
	TableGrant("loyal_yield.balance_sweep_targets", "SELECT"),
	TableGrant("loyal_yield.managed_vaults", "SELECT"),
	TableGrant("loyal_yield.rebalance_decisions", "SELECT"),
	TableGrant("loyal_yield.route_policies", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_deposits", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_holding_events", "INSERT"),
	TableGrant("loyal_yield.user_yield_position_holding_events", "SELECT"),
	TableGrant("loyal_yield.user_yield_position_withdrawals", "SELECT"),
	TableGrant("loyal_yield.user_yield_positions", "SELECT"),
	TableGrant("loyal_yield.user_yield_positions", "UPDATE"),
	TableGrant("loyal_yield.vault_position_snapshot_positions", "SELECT"),
	TableGrant("loyal_yield.vault_position_snapshots", "SELECT"),
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
	huma.Register(api, huma.Operation{
		OperationID: "scan-earn-policy-refunds",
		Method:      http.MethodPost,
		Path:        earnPolicyRefundScanPath,
		Summary:     "Which Earn policies and vault accounts are refundable right now",
		Responses:   errorResponses[routeError](api, 401, 409, 500),
	}, h.earnPolicyRefundScan)
}

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

	transactions := txreadsAssembleTransactions(positionEvents, autodepositEvents)
	return &earnTransactionsOutput{Body: EarnTransactionsResponse{Transactions: transactions}}, nil
}

const earnPolicyRefundScanPath = "/api/smart-accounts/yield-optimization/policy-refunds/scan"

// EarnPolicyRefundRecurringDelegation mirrors
// EarnPolicyRefundRecurringDelegation
// (web/apps/web/src/lib/yield-optimization/earn-policy-refund-contracts.shared.ts).
type EarnPolicyRefundRecurringDelegation struct {
	Account             string  `json:"account"`
	Active              bool    `json:"active"`
	AmountPerPeriodRaw  *string `json:"amountPerPeriodRaw"`
	AmountPulledRaw     *string `json:"amountPulledRaw"`
	Authority           *string `json:"authority"`
	BlockedReason       *string `json:"blockedReason"`
	CanRefund           bool    `json:"canRefund"`
	Delegatee           *string `json:"delegatee"`
	Delegator           *string `json:"delegator"`
	Exists              bool    `json:"exists"`
	ExpiryTimestamp     *string `json:"expiryTimestamp"`
	Lamports            *uint64 `json:"lamports"`
	LifecycleStatus     string  `json:"lifecycleStatus"`
	Mint                *string `json:"mint"`
	PolicyAccount       *string `json:"policyAccount"`
	Protected           bool    `json:"protected"`
	ScheduledSweepCount int     `json:"scheduledSweepCount"`
	Source              string  `json:"source"`
	Status              string  `json:"status"`
	TargetActive        bool    `json:"targetActive"`
	TargetID            *string `json:"targetId"`
	Usage               string  `json:"usage"`
}

type EarnPolicyRefundScanPolicy struct {
	Account                    string                                `json:"account"`
	AccountIndex               *uint8                                `json:"accountIndex"`
	ActiveAutodeposit          bool                                  `json:"activeAutodeposit"`
	ActiveManagedVault         bool                                  `json:"activeManagedVault"`
	BlockedReason              *string                               `json:"blockedReason"`
	CanRefund                  bool                                  `json:"canRefund"`
	DBPresent                  bool                                  `json:"dbPresent"`
	Lamports                   *uint64                               `json:"lamports"`
	RecurringDelegations       []EarnPolicyRefundRecurringDelegation `json:"recurringDelegations"`
	ReferencedByActivePosition bool                                  `json:"referencedByActivePosition"`
	Seed                       string                                `json:"seed"`
	State                      string                                `json:"state"`
}

type EarnVaultRefundTokenAccount struct {
	Account   string `json:"account"`
	AmountRaw string `json:"amountRaw"`
	IsUsdc    bool   `json:"isUsdc"`
	Lamports  uint64 `json:"lamports"`
	Mint      string `json:"mint"`
}

type EarnPolicyRefundVaultEntry struct {
	Account                 string                        `json:"account"`
	BlockedReason           *string                       `json:"blockedReason"`
	CanRefund               bool                          `json:"canRefund"`
	Lamports                uint64                        `json:"lamports"`
	TokenAccounts           []EarnVaultRefundTokenAccount `json:"tokenAccounts"`
	TotalRefundableLamports uint64                        `json:"totalRefundableLamports"`
}

type EarnPolicyRefundScanResponse struct {
	Policies             []EarnPolicyRefundScanPolicy          `json:"policies"`
	RecurringDelegations []EarnPolicyRefundRecurringDelegation `json:"recurringDelegations"`
	SettingsPDA          string                                `json:"settingsPda"`
	Vault                *EarnPolicyRefundVaultEntry           `json:"vault"`
	VaultIndex           uint8                                 `json:"vaultIndex"`
	VaultPubkey          string                                `json:"vaultPubkey"`
}

type earnPolicyRefundScanOutput struct {
	Body EarnPolicyRefundScanResponse
}

// earnPolicyRefundScan ports the Next policy-refunds scan route
// (yield-optimization/policy-refunds/scan/route.ts): the chain and DB liveness
// facts each route policy and the Earn vault need before a refund may run.
// Next answers failures with its own 500 scan_failed envelope rather than the
// 503 earn envelope, so only the auth refusal goes through earnUnavailable.
func (h earnRoutes) earnPolicyRefundScan(ctx context.Context, in *sessionRequest) (*earnPolicyRefundScanOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn policy refunds session", err,
			"scan_failed", "Failed to scan policies.")
	}
	scan, err := h.txreadsScanPolicyRefunds(ctx, account)
	if err != nil {
		slog.Warn("earn policy refund scan failed", "settings", account.Settings.String(),
			"wallet", account.Wallet.String(), "error", err)
		return nil, earnError(http.StatusInternalServerError, "scan_failed", err.Error())
	}
	return &earnPolicyRefundScanOutput{Body: scan}, nil
}
