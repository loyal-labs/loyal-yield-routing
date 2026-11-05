package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	mobileEarnStatePath            = mobileEarnPrefix + "earn/state"
	mobileEarnAutodepositStatePath = mobileEarnPrefix + "earn/autodeposit/state"
	mobileEarnTransactionsPath     = mobileEarnPrefix + "earn/transactions"
	mobileEarnRefundScanPath       = mobileEarnPrefix + "earn/policy-refunds/scan"
	mobileEarnMaxPath              = mobileEarnPrefix + "earn-max"
)

var legacyReadPaths = []string{
	mobileEarnStatePath,
	mobileEarnAutodepositStatePath,
	mobileEarnTransactionsPath,
	mobileEarnRefundScanPath,
	mobileEarnMaxPath,
}

var earnLegacyStateFamily = earnFamily{
	name:        "earn legacy state",
	yieldGrants: earnStateYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.Timescale != nil },
	register:    earnRoutes.registerLegacyState,
}

var earnLegacyTransactionsFamily = earnFamily{
	name:        "earn legacy transactions",
	yieldGrants: earnTransactionYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.Chain != nil },
	register:    earnRoutes.registerLegacyTransactions,
}

type legacyWalletRequest struct {
	WalletAddress string `query:"walletAddress" required:"false"`
}

type MobileEarnStatePosition struct {
	CurrentAmountRaw    string  `json:"currentAmountRaw"`
	CurrentSupplyApyBps *string `json:"currentSupplyApyBps"`
	PrincipalAmountRaw  string  `json:"principalAmountRaw"`
	Status              string  `json:"status"`
}

type MobileEarnStateResponse struct {
	Position            *MobileEarnStatePosition `json:"position"`
	SettingsPda         *string                  `json:"settingsPda"`
	SmartAccountAddress *string                  `json:"smartAccountAddress"`
}

type MobileEarnAutodeposit struct {
	Active                             bool                      `json:"active"`
	Status                             string                    `json:"status"`
	PolicyAccount                      string                    `json:"policyAccount"`
	RecurringDelegation                *string                   `json:"recurringDelegation"`
	WalletBalanceFloorRaw              *string                   `json:"walletBalanceFloorRaw"`
	LifecycleStatus                    string                    `json:"lifecycleStatus"`
	VaultIndex                         int                       `json:"vaultIndex"`
	PolicySeed                         string                    `json:"policySeed"`
	RecurringDelegationNonce           *string                   `json:"recurringDelegationNonce"`
	PeriodLengthSeconds                *string                   `json:"periodLengthSeconds"`
	StartTimestamp                     *string                   `json:"startTimestamp"`
	RecurringDelegationExpiryTimestamp *string                   `json:"recurringDelegationExpiryTimestamp"`
	ScheduledSweeps                    []EarnStateScheduledSweep `json:"scheduledSweeps"`
}

type MobileEarnPrepareContext struct {
	Cluster      string `json:"cluster"`
	PolicySigner string `json:"policySigner"`
	ProgramID    string `json:"programId"`
}

type MobileEarnAutodepositStateResponse struct {
	Autodeposit         *MobileEarnAutodeposit    `json:"autodeposit"`
	PrepareContext      *MobileEarnPrepareContext `json:"prepareContext"`
	SettingsPda         *string                   `json:"settingsPda"`
	SmartAccountAddress *string                   `json:"smartAccountAddress"`
}

type MobileEarnRefundScanResponse struct {
	Scan *EarnPolicyRefundScanResponse `json:"scan"`
}

type legacyStateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MobileEarnStateResponse
}

type legacyAutodepositOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MobileEarnAutodepositStateResponse
}

type legacyTransactionsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EarnTransactionsResponse
}

type legacyRefundScanOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MobileEarnRefundScanResponse
}

func (h earnRoutes) registerLegacyState(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-state",
		Method:      http.MethodGet,
		Path:        mobileEarnStatePath,
		Summary:     "Earn position headline for a wallet address, no session",
		Responses:   errorResponses[routeError](api, 400, 502),
	}, h.legacyState)
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-autodeposit-state",
		Method:      http.MethodGet,
		Path:        mobileEarnAutodepositStatePath,
		Summary:     "Autodeposit state and prepare context for a wallet address, no session",
		Responses:   errorResponses[routeError](api, 400, 502),
	}, h.legacyAutodepositState)
}

func (h earnRoutes) registerLegacyTransactions(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-transactions",
		Method:      http.MethodGet,
		Path:        mobileEarnTransactionsPath,
		Summary:     "Earn transaction history for a wallet address, no session",
		Responses:   errorResponses[routeError](api, 400, 502),
	}, h.legacyTransactions)
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-policy-refunds-scan",
		Method:      http.MethodGet,
		Path:        mobileEarnRefundScanPath,
		Summary:     "Which Earn policies and vault accounts are refundable for a wallet address, no session",
		Responses:   errorResponses[routeError](api, 400, 500),
	}, h.legacyRefundScan)
}

func legacyFailed(step string, err error, status int, code, message string) error {
	slog.Warn("[mobile-earn-legacy] read failed", "step", step, "error", err)
	return earnError(status, code, message)
}

func (h earnRoutes) legacyState(ctx context.Context, in *legacyWalletRequest) (*legacyStateOutput, error) {
	const code, message = "earn_state_failed", "Failed to load Earn state."
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	out := func(body MobileEarnStateResponse) *legacyStateOutput {
		return &legacyStateOutput{CacheControl: perWalletCacheControl(), Body: body}
	}
	account, err := h.walletAccount(ctx, wallet, false)
	if err != nil {
		return nil, legacyFailed("state account", err, http.StatusBadGateway, code, message)
	}
	if account == nil {
		return out(MobileEarnStateResponse{}), nil
	}
	settings, smartAccount := account.Settings.String(), account.SmartAccount.String()
	position, err := h.reconciledActiveYieldPosition(ctx, *account)
	if err != nil {
		return nil, legacyFailed("state position", err, http.StatusBadGateway, code, message)
	}
	if position == nil {
		return out(MobileEarnStateResponse{SettingsPda: &settings, SmartAccountAddress: &smartAccount}), nil
	}
	total, err := h.legacyActiveTotal(ctx, *account)
	if err != nil {
		slog.Warn("[mobile-earn-state] failed to sum active position rows", "error", err)
	}
	if total <= 0 {
		total = position.CurrentAmountRaw
	}
	return out(MobileEarnStateResponse{
		Position: &MobileEarnStatePosition{
			CurrentAmountRaw:    strconvFormatInt(total),
			CurrentSupplyApyBps: h.stateCurrentSupplyApyBps(ctx, *position),
			PrincipalAmountRaw:  strconvFormatInt(position.PrincipalAmountRaw),
			Status:              position.Status,
		},
		SettingsPda:         &settings,
		SmartAccountAddress: &smartAccount,
	}), nil
}

func (h earnRoutes) legacyActiveTotal(ctx context.Context, account EarnAccount) (int64, error) {
	var total int64
	err := h.YieldDB.QueryRow(ctx, `SELECT COALESCE(SUM(current_amount_raw), 0)::bigint
FROM loyal_yield.user_yield_positions
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND status = 'active'`,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String()).Scan(&total)
	return total, err
}

func (h earnRoutes) legacyAutodepositState(ctx context.Context, in *legacyWalletRequest) (*legacyAutodepositOutput, error) {
	const code, message = "autodeposit_state_failed", "Failed to load Autodeposit state."
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	out := func(body MobileEarnAutodepositStateResponse) *legacyAutodepositOutput {
		return &legacyAutodepositOutput{CacheControl: perWalletCacheControl(), Body: body}
	}
	account, err := h.walletAccount(ctx, wallet, false)
	if err != nil {
		return nil, legacyFailed("autodeposit account", err, http.StatusBadGateway, code, message)
	}
	if account == nil {
		return out(MobileEarnAutodepositStateResponse{}), nil
	}
	settings, smartAccount := account.Settings.String(), account.SmartAccount.String()
	body := MobileEarnAutodepositStateResponse{SettingsPda: &settings, SmartAccountAddress: &smartAccount}
	target, _, err := h.stateAutodepositState(ctx, settings, wallet.String())
	if err != nil {
		return nil, legacyFailed("autodeposit target", err, http.StatusBadGateway, code, message)
	}
	body.PrepareContext = h.legacyPrepareContext(*account)
	if target == nil {
		return out(body), nil
	}
	status := stateAutodepositStatus(target.Active, target.ChainStatus)
	sweeps := []statePendingSweepRow{}
	if status == "active" {
		if sweeps, err = h.statePendingSweeps(ctx, target); err != nil {
			return nil, legacyFailed("autodeposit sweeps", err, http.StatusBadGateway, code, message)
		}
	}
	body.Autodeposit = &MobileEarnAutodeposit{
		Active:                             target.Active,
		Status:                             status,
		PolicyAccount:                      target.PolicyAccount,
		RecurringDelegation:                target.RecurringDelegation,
		WalletBalanceFloorRaw:              stateInt64Ptr(target.WalletBalanceFloorRaw),
		LifecycleStatus:                    target.ChainStatus,
		VaultIndex:                         int(earnprep.EarnVaultIndex),
		PolicySeed:                         strconvFormatInt(target.PolicySeed),
		RecurringDelegationNonce:           stateInt64Ptr(target.RecurringDelegationNonce),
		PeriodLengthSeconds:                stateInt64Ptr(target.PeriodLengthSeconds),
		StartTimestamp:                     stateInt64Ptr(target.StartTimestamp),
		RecurringDelegationExpiryTimestamp: stateInt64Ptr(target.RecurringDelegationExpiryTimestamp),
		ScheduledSweeps:                    serializeEarnStateScheduledSweeps(status, sweeps),
	}
	return out(body), nil
}

func (h earnRoutes) legacyPrepareContext(account EarnAccount) *MobileEarnPrepareContext {
	if h.PolicySigner.IsZero() {
		return nil
	}
	return &MobileEarnPrepareContext{
		Cluster:      string(account.Cluster),
		PolicySigner: h.PolicySigner.String(),
		ProgramID:    squads.ProgramID.String(),
	}
}

func (h earnRoutes) legacyTransactions(ctx context.Context, in *legacyWalletRequest) (*legacyTransactionsOutput, error) {
	const code, message = "earn_transactions_failed", "Failed to load Earn transactions."
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	account, err := h.walletAccount(ctx, wallet, false)
	if err != nil {
		return nil, legacyFailed("transactions account", err, http.StatusBadGateway, code, message)
	}
	transactions := []SerializedEarnTransaction{}
	if account != nil {
		if transactions, err = h.earnTransactionsFor(ctx, *account); err != nil {
			return nil, legacyFailed("transactions history", err, http.StatusBadGateway, code, message)
		}
	}
	return &legacyTransactionsOutput{CacheControl: perWalletCacheControl(), Body: EarnTransactionsResponse{Transactions: transactions}}, nil
}

func (h earnRoutes) legacyRefundScan(ctx context.Context, in *legacyWalletRequest) (*legacyRefundScanOutput, error) {
	const code = "scan_failed"
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	account, err := h.walletAccount(ctx, wallet, false)
	if err != nil {
		return nil, legacyFailed("scan account", err, http.StatusInternalServerError, code, "Failed to scan for refunds.")
	}
	body := MobileEarnRefundScanResponse{}
	if account != nil {
		scan, err := h.txreadsScanPolicyRefunds(ctx, *account)
		if err != nil {
			return nil, legacyFailed("scan", err, http.StatusInternalServerError, code, err.Error())
		}
		body.Scan = &scan
	}
	return &legacyRefundScanOutput{CacheControl: perWalletCacheControl(), Body: body}, nil
}

type MobileEarnMaxConfig struct {
	DelegatedSigner string `json:"delegatedSigner"`
	ProgramID       string `json:"programId"`
}

type MobileEarnMaxSummaryResponse struct {
	Config  MobileEarnMaxConfig `json:"config"`
	Summary *EarnMaxSummary     `json:"summary"`
}

type MobileEarnMaxActivityResponse struct {
	Operations  []earnmax.ActivityItem     `json:"operations" nullable:"false"`
	Performance []earnmax.PerformancePoint `json:"performance" nullable:"false"`
}

type legacyEarnMaxRequest struct {
	Action        string `query:"action" required:"false"`
	WalletAddress string `query:"walletAddress" required:"false"`
}

type MobileEarnMaxResponse struct {
	Config      *MobileEarnMaxConfig       `json:"config,omitempty"`
	Summary     *EarnMaxSummary            `json:"summary,omitempty"`
	Operations  []earnmax.ActivityItem     `json:"operations,omitempty" nullable:"false"`
	Performance []earnmax.PerformancePoint `json:"performance,omitempty" nullable:"false"`
	activity    bool
}

func (r MobileEarnMaxResponse) MarshalJSON() ([]byte, error) {
	if r.activity {
		return json.Marshal(MobileEarnMaxActivityResponse{Operations: r.Operations, Performance: r.Performance})
	}
	return json.Marshal(MobileEarnMaxSummaryResponse{Config: *r.Config, Summary: r.Summary})
}

type legacyEarnMaxOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MobileEarnMaxResponse
}

func (h earnMaxRoutes) registerLegacyRead(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-max",
		Method:      http.MethodGet,
		Path:        mobileEarnMaxPath,
		Summary:     "Earn MAX summary or activity for a wallet address, no session; action=summary returns config and summary, action=activity returns operations and performance",
		Responses:   errorResponses[routeError](api, 400, 409, 502),
	}, h.legacyEarnMax)
}

func legacyEarnMaxFail(status int, code, message string) error {
	return huma.ErrorWithHeaders(earnError(status, code, message), http.Header{"Cache-Control": {perWalletCacheControl()}})
}

func (h earnMaxRoutes) legacyEarnMax(ctx context.Context, in *legacyEarnMaxRequest) (*legacyEarnMaxOutput, error) {
	const unavailable = "Unable to update Earn MAX."
	if in.Action != "summary" && in.Action != "activity" {
		return nil, legacyEarnMaxFail(http.StatusBadRequest, "invalid_request", "Unknown Earn MAX read action.")
	}
	wallet, err := solana.PublicKeyFromBase58(strings.TrimSpace(in.WalletAddress))
	if err != nil {
		return nil, legacyEarnMaxFail(http.StatusBadRequest, "invalid_wallet_address", "Wallet address is invalid.")
	}
	if h.cfg.Cluster != Mainnet {
		return nil, legacyEarnMaxFail(http.StatusConflict, "unsupported_cluster", "Earn MAX requires mainnet.")
	}
	settings, err := h.auth.readySettings(ctx, wallet)
	ready := err == nil
	var refused *AuthError
	if err != nil && !(errors.As(err, &refused) && refused.Failure == SmartAccountRequired) {
		slog.Warn("[mobile-earn-max] account read failed", "error", err)
		return nil, legacyEarnMaxFail(http.StatusBadGateway, "earn_max_unavailable", unavailable)
	}
	if ready {
		smartAccount, _, err := squads.SmartAccountAddress(settings, 0)
		if err == nil {
			err = h.auth.controls(ctx, Principal{Wallet: wallet, Settings: settings, SmartAccount: smartAccount})
		}
		if errors.As(err, &refused) && refused.Failure == SmartAccountRequired {
			return nil, legacyEarnMaxFail(http.StatusConflict, "smart_account_signer_mismatch", "The smart account settings are no longer controlled by this wallet.")
		}
		if err != nil {
			slog.Warn("[mobile-earn-max] signer check failed", "error", err)
			return nil, legacyEarnMaxFail(http.StatusBadGateway, "earn_max_unavailable", unavailable)
		}
	}
	out := &legacyEarnMaxOutput{CacheControl: perWalletCacheControl()}
	if in.Action == "activity" {
		out.Body = MobileEarnMaxResponse{Operations: []earnmax.ActivityItem{}, Performance: []earnmax.PerformancePoint{}, activity: true}
		if ready {
			activity, err := h.cfg.Reader.Activity(ctx, settings)
			if err != nil {
				slog.Warn("[mobile-earn-max] activity read failed", "error", err)
				return nil, legacyEarnMaxFail(http.StatusBadGateway, "earn_max_unavailable", unavailable)
			}
			out.Body = MobileEarnMaxResponse{Operations: activity.Operations, Performance: activity.Performance, activity: true}
		}
		return out, nil
	}
	summaryBody := MobileEarnMaxResponse{Config: &MobileEarnMaxConfig{ProgramID: squads.ProgramID.String()}}
	if ready {
		summary, err := h.cfg.Reader.Summary(ctx, settings)
		if err != nil {
			slog.Warn("[mobile-earn-max] summary read failed", "error", err)
			return nil, legacyEarnMaxFail(http.StatusBadGateway, "earn_max_unavailable", unavailable)
		}
		earnMaxSummary := EarnMaxSummary(summary)
		summaryBody.Summary = &earnMaxSummary
	}
	out.Body = summaryBody
	return out, nil
}
