package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	mobileEarnEarningsPath = mobileEarnPrefix + "earn/earnings"
	mobileEarnHoldingsPath = mobileEarnPrefix + "earn/holdings"
)

var deskReadPaths = []string{mobileEarnEarningsPath, mobileEarnHoldingsPath}

var earnDeskEarningsFamily = earnFamily{
	name:        "earn desk earnings",
	yieldGrants: earnEarningsYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.Timescale != nil },
	register:    earnRoutes.registerDeskEarnings,
}

var earnDeskHoldingsFamily = earnFamily{
	name:        "earn desk holdings",
	yieldGrants: slices.Concat(EarnPolicyGrants, []Grant{TableGrant("loyal_yield.user_yield_positions", "SELECT")}),
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.Chain != nil },
	register:    earnRoutes.registerDeskHoldings,
}

type deskHoldingsRequest struct {
	WalletAddress string `query:"walletAddress" required:"false"`
}

type deskEarningsRequest struct {
	WalletAddress string `query:"walletAddress" required:"false"`
	Timezone      string `query:"timezone" required:"false"`
}

type DeskHolding struct {
	AmountRaw      string            `json:"amountRaw"`
	Kind           string            `json:"kind"`
	Label          string            `json:"label"`
	LiquidityMint  string            `json:"liquidityMint"`
	Market         *string           `json:"market"`
	MarketName     string            `json:"marketName"`
	ObservedAt     string            `json:"observedAt"`
	ObservedSlot   string            `json:"observedSlot"`
	Provenance     map[string]string `json:"provenance"`
	Reserve        *string           `json:"reserve"`
	SourceID       string            `json:"sourceId"`
	SupplyApyBps   *string           `json:"supplyApyBps"`
	TokenProgramID string            `json:"tokenProgramId"`
}

type DeskHoldingsResponse struct {
	CurrentTotalAmountRaw        string        `json:"currentTotalAmountRaw"`
	CurrentTotalNominalUsdMicros string        `json:"currentTotalNominalUsdMicros"`
	Holdings                     []DeskHolding `json:"holdings"`
	ObservedAt                   *string       `json:"observedAt"`
	ObservedSlot                 *string       `json:"observedSlot"`
	SettingsPda                  *string       `json:"settingsPda"`
	SmartAccountAddress          *string       `json:"smartAccountAddress"`
}

type deskHoldingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DeskHoldingsResponse
}

func (h earnRoutes) registerDeskEarnings(api huma.API) {
	responses := errorResponses[routeError](api, 400)
	for status, response := range errorResponses[earningsError](api, 503) {
		responses[status] = response
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-earnings",
		Method:      http.MethodGet,
		Path:        mobileEarnEarningsPath,
		Summary:     "Earn earnings bars and lifetime totals for a wallet address, no session",
		Responses:   responses,
	}, h.deskEarnings)
}

func (h earnRoutes) registerDeskHoldings(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-mobile-earn-holdings",
		Method:      http.MethodGet,
		Path:        mobileEarnHoldingsPath,
		Summary:     "Live on-chain Earn holdings for a wallet address, no session",
		Responses:   errorResponses[routeError](api, 400, 502),
	}, h.deskHoldings)
}

func deskWallet(raw string) (solana.PublicKey, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_request", "walletAddress is required.")
	}
	wallet, err := solana.PublicKeyFromBase58(text)
	if err != nil {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_wallet_address", "Wallet address is invalid.")
	}
	return wallet, nil
}

// deskAccount is the wallet's ready smart account, or nil when the wallet has
// none or does not control it, the cases Next answers with an empty read.
func (h earnRoutes) deskAccount(ctx context.Context, wallet solana.PublicKey) (*EarnAccount, error) {
	return h.walletAccount(ctx, wallet, true)
}

func (h earnRoutes) walletAccount(ctx context.Context, wallet solana.PublicKey, verifySigner bool) (*EarnAccount, error) {
	settings, err := h.auth.readySettings(ctx, wallet)
	var principal Principal
	if err == nil {
		var smartAccount solana.PublicKey
		smartAccount, _, err = squads.SmartAccountAddress(settings, 0)
		if err == nil {
			principal = Principal{Wallet: wallet, Settings: settings, SmartAccount: smartAccount}
			if verifySigner {
				err = h.auth.controls(ctx, principal)
			}
		}
	}
	var refused *AuthError
	if errors.As(err, &refused) && refused.Failure == SmartAccountRequired {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	vault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
	if err != nil {
		return nil, err
	}
	return &EarnAccount{Principal: principal, Vault: vault, Cluster: h.Cluster.Loyal()}, nil
}

func (h earnRoutes) deskEarnings(ctx context.Context, in *deskEarningsRequest) (*earningsOutput, error) {
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	account, err := h.deskAccount(ctx, wallet)
	if err != nil {
		warnEarningsRead("desk account", err)
		return nil, newEarningsError(earningsUnavailableCode, nil)
	}
	if account == nil {
		empty, err := createEmptyEarnEarningsRangeSet(time.Now(), in.Timezone)
		if err != nil {
			warnEarningsRead("desk empty", err)
			return nil, newEarningsError(earningsUnavailableCode, nil)
		}
		return &earningsOutput{CacheControl: perWalletCacheControl(), Body: empty}, nil
	}
	payload, err := h.earningsFor(ctx, *account, in.Timezone)
	if err != nil {
		return nil, err
	}
	return &earningsOutput{CacheControl: perWalletCacheControl(), Body: payload}, nil
}

func (h earnRoutes) deskHoldings(ctx context.Context, in *deskHoldingsRequest) (*deskHoldingsOutput, error) {
	wallet, err := deskWallet(in.WalletAddress)
	if err != nil {
		return nil, err
	}
	body, err := h.deskHoldingsRead(ctx, wallet)
	if err != nil {
		slog.Error("[mobile-earn-holdings] read failed", "error", err, "walletAddress", wallet.String())
		return nil, earnError(http.StatusBadGateway, "earn_holdings_failed", "Failed to load Earn holdings.")
	}
	return &deskHoldingsOutput{CacheControl: perWalletCacheControl(), Body: body}, nil
}

func (h earnRoutes) deskHoldingsRead(ctx context.Context, wallet solana.PublicKey) (DeskHoldingsResponse, error) {
	empty := DeskHoldingsResponse{CurrentTotalAmountRaw: "0", CurrentTotalNominalUsdMicros: "0", Holdings: []DeskHolding{}}
	account, err := h.deskAccount(ctx, wallet)
	if err != nil || account == nil {
		return empty, err
	}
	settings, smartAccount := account.Settings.String(), account.SmartAccount.String()
	empty.SettingsPda, empty.SmartAccountAddress = &settings, &smartAccount
	pair, err := h.activePolicyPair(ctx, *account)
	if err != nil || pair == nil {
		return empty, err
	}
	floor, err := h.deskConfirmedSlotFloor(ctx, settings, wallet.String())
	if err != nil {
		return DeskHoldingsResponse{}, err
	}
	read := func() (earnprep.WithdrawSnapshot, error) {
		return earnprep.FetchWithdrawHoldingsSnapshot(ctx, h.Chain, earnprep.WithdrawHoldingsArgs{
			Cluster:        string(account.Cluster),
			MinContextSlot: floor,
			Policy: &earnprep.WithdrawPolicyUniverse{
				VaultIndex:           earnprep.EarnVaultIndex,
				StableMints:          pair.Route.StableMints,
				KaminoLiquidityMints: pair.Route.KaminoLiquidityMints,
				KaminoMarkets:        pair.Route.KaminoMarkets,
			},
			SettingsPda: account.Settings,
		})
	}
	snapshot, err := read()
	if err != nil {
		if floor == 0 {
			return DeskHoldingsResponse{}, err
		}
		slog.Warn("[mobile-earn-holdings] snapshot read retried", "error", err, "minContextSlot", floor, "walletAddress", wallet.String())
		if snapshot, err = read(); err != nil {
			return DeskHoldingsResponse{}, err
		}
	}
	stale := floor > 0 && snapshot.ObservedSlot < floor
	if stale {
		slog.Warn("[mobile-earn-holdings] stale live read suppressed", "confirmedSlot", floor, "observedSlot", snapshot.ObservedSlot, "walletAddress", wallet.String())
	}
	total := strconv.FormatUint(snapshot.CurrentTotalAmountRaw, 10)
	observedSlot := strconv.FormatUint(snapshot.ObservedSlot, 10)
	body := DeskHoldingsResponse{
		CurrentTotalAmountRaw:        total,
		CurrentTotalNominalUsdMicros: total,
		Holdings:                     make([]DeskHolding, 0, len(snapshot.Holdings)),
		ObservedSlot:                 &observedSlot,
		SettingsPda:                  &settings,
		SmartAccountAddress:          &smartAccount,
	}
	if !stale {
		body.ObservedAt = &snapshot.ObservedAt
	}
	for _, holding := range snapshot.Holdings {
		body.Holdings = append(body.Holdings, deskHolding(holding, snapshot))
	}
	return body, nil
}

func deskHolding(holding earnprep.WithdrawHolding, snapshot earnprep.WithdrawSnapshot) DeskHolding {
	out := DeskHolding{
		AmountRaw:      strconv.FormatUint(holding.AmountRaw, 10),
		Kind:           string(holding.Kind),
		Label:          holding.Label,
		LiquidityMint:  holding.LiquidityMint.String(),
		MarketName:     holding.MarketName,
		ObservedAt:     snapshot.ObservedAt,
		ObservedSlot:   strconv.FormatUint(snapshot.ObservedSlot, 10),
		Provenance:     holding.Provenance,
		SourceID:       holding.SourceID,
		SupplyApyBps:   holding.SupplyApyBps,
		TokenProgramID: holding.TokenProgram.String(),
	}
	if holding.Market != nil {
		market := holding.Market.String()
		out.Market = &market
	}
	if holding.Reserve != nil {
		reserve := holding.Reserve.String()
		out.Reserve = &reserve
	}
	return out
}

func (h earnRoutes) deskConfirmedSlotFloor(ctx context.Context, settings, wallet string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var floor int64
	err := h.YieldDB.QueryRow(ctx, `SELECT COALESCE(MAX(current_observed_slot), 0)
FROM loyal_yield.user_yield_positions
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND status = 'active'`,
		settings, int16(earnprep.EarnVaultIndex), wallet).Scan(&floor)
	return uint64(max(floor, 0)), err
}
