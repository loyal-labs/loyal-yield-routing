package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

const statePositionPath = "/api/smart-accounts/yield-optimization/position"

var (
	stateSourceVaultReserve       = "vault_reserve_positions_current"
	stateSourceVaultIdle          = "vault_idle_token_balances_current"
	stateSourceUserYieldPositions = "user_yield_positions"
)

type EarnPositionProvenance struct {
	// Key order follows the four Next provenance literals: a reserve row
	// emits snapshotId then source, an idle row owner, source,
	// sourceCommitment then tokenAccount, the position fallback
	// lastHoldingEventId then source, and the current holding
	// lastHoldingEventId then lastRebalanceDecisionId with no source.
	SnapshotID            *string `json:"snapshotId,omitempty"`
	Owner                 *string `json:"owner,omitempty"`
	LastHoldingEvent      *string `json:"lastHoldingEventId,omitempty"`
	LastRebalanceDecision *string `json:"lastRebalanceDecisionId,omitempty"`
	Source                *string `json:"source,omitempty"`
	SourceCommitment      *string `json:"sourceCommitment,omitempty"`
	TokenAccount          *string `json:"tokenAccount,omitempty"`
}

type EarnPositionHolding struct {
	AmountRaw     string                 `json:"amountRaw"`
	Kind          string                 `json:"kind"`
	Label         string                 `json:"label"`
	LiquidityMint string                 `json:"liquidityMint"`
	Market        *string                `json:"market"`
	MarketName    string                 `json:"marketName"`
	ObservedAt    string                 `json:"observedAt"`
	ObservedSlot  string                 `json:"observedSlot"`
	Provenance    EarnPositionProvenance `json:"provenance"`
	Reserve       *string                `json:"reserve"`
	SourceID      string                 `json:"sourceId"`
	SupplyApyBps  *string                `json:"supplyApyBps"`
	TokenProgram  string                 `json:"tokenProgramId"`
}

type EarnPositionCurrentHolding struct {
	AmountRaw     string                 `json:"amountRaw"`
	LiquidityMint string                 `json:"liquidityMint"`
	Market        *string                `json:"market"`
	ObservedAt    string                 `json:"observedAt"`
	ObservedSlot  string                 `json:"observedSlot"`
	Provenance    EarnPositionProvenance `json:"provenance"`
	Reserve       string                 `json:"reserve"`
}

type EarnPositionInitialHolding struct {
	LiquidityMint string  `json:"liquidityMint"`
	Market        *string `json:"market"`
	Reserve       string  `json:"reserve"`
	SupplyApyBps  *string `json:"supplyApyBps"`
}

type EarnPositionDisplay struct {
	Label      string `json:"label"`
	MarketName string `json:"marketName"`
	MintSymbol string `json:"mintSymbol"`
}

type EarnPosition struct {
	CurrentHolding               EarnPositionCurrentHolding `json:"currentHolding"`
	CurrentSupplyApyBps          *string                    `json:"currentSupplyApyBps"`
	Display                      EarnPositionDisplay        `json:"display"`
	ID                           string                     `json:"id"`
	InitialHolding               EarnPositionInitialHolding `json:"initialHolding"`
	Holdings                     []EarnPositionHolding      `json:"holdings"`
	CurrentTotalAmountRaw        string                     `json:"currentTotalAmountRaw"`
	CurrentTotalNominalUsdMicros string                     `json:"currentTotalNominalUsdMicros"`
	PrincipalAmountRaw           string                     `json:"principalAmountRaw"`
	Status                       string                     `json:"status"`
}

type EarnPositionResponse struct {
	Position *EarnPosition `json:"position"`
}

type statePositionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EarnPositionResponse
}

func (h earnRoutes) registerPosition(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-position",
		Method:      http.MethodGet,
		Path:        statePositionPath,
		Summary:     "Reconciled Earn position with its current holdings",
		Responses:   errorResponses[routeError](api, 401, 409, 503),
	}, h.earnPosition)
}

func (h earnRoutes) earnPosition(ctx context.Context, in *sessionRequest) (*statePositionOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn-position auth", err, "unauthenticated", "No active auth session.")
	}
	settings := account.Settings.String()
	wallet := account.Wallet.String()

	position, err := h.reconciledActiveYieldPosition(ctx, account)
	if err != nil {
		return nil, earnUnavailable("earn-position read", err, "position_unavailable", "Earn position is unavailable.")
	}
	if position == nil {
		return &statePositionOutput{CacheControl: perWalletCacheControl(), Body: EarnPositionResponse{}}, nil
	}

	reserveRows, err := h.stateCurrentReserveRows(ctx, settings, wallet, position.VaultPubkey)
	if err != nil {
		return nil, earnUnavailable("earn-position holdings", err, "position_unavailable", "Earn position is unavailable.")
	}
	idleRows, err := h.stateCurrentIdleRows(ctx, settings, wallet, position.VaultPubkey)
	if err != nil {
		return nil, earnUnavailable("earn-position idle balances", err, "position_unavailable", "Earn position is unavailable.")
	}

	supplyApyBps := h.stateCurrentSupplyApyBps(ctx, *position)

	serialized, err := serializeEarnPosition(*position, supplyApyBps, reserveRows, idleRows, account.Cluster)
	if err != nil {
		return nil, earnUnavailable("earn-position serialize", err, "position_unavailable", "Earn position is unavailable.")
	}
	return &statePositionOutput{
		CacheControl: perWalletCacheControl(),
		Body:         EarnPositionResponse{Position: serialized},
	}, nil
}

func (h earnRoutes) stateCurrentSupplyApyBps(ctx context.Context, position stateYieldPositionRow) *string {
	reserve := stateTimescaleReserveForPosition(position)
	var apy *float64
	for attempt := 0; ; attempt++ {
		value, err := h.stateTimescaleSupplyApy(ctx, reserve)
		if err == nil {
			apy = value
			break
		}
		if attempt >= 2 {
			slog.Warn("[earn-position] current Timescale reserve lookup failed", "error", err, "currentReserve", position.CurrentReserve, "timescaleReserve", reserve)
			break
		}
		select {
		case <-time.After(time.Duration(250*(attempt+1)) * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
	}
	if apy == nil {
		return nil
	}
	bps := strconvFormatInt(int64(math.Round(*apy * 10_000)))
	return &bps
}

func (h earnRoutes) stateTimescaleSupplyApy(ctx context.Context, reserve string) (*float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var apy float64
	err := h.Timescale.QueryRow(ctx, `SELECT updates.supply_apy
FROM kamino.reserve_updates AS updates
JOIN kamino.latest_reserve_updates AS latest
  ON updates.reserve = latest.reserve AND updates.slot = latest.slot
 AND updates.observed_at = latest.observed_at
WHERE updates.reserve_last_update_stale = false
  AND updates.supply_apy >= 0
  AND updates.supply_apy < $2::float8
  AND updates.reserve = $1
LIMIT 1`, reserve, dtargetMaxSupplyApy).Scan(&apy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read current Kamino reserve update: %w", err)
	}
	return &apy, nil
}

func stateProductAssets(cluster LoyalCluster) []earncatalog.Product {
	return earncatalog.ForCluster(cluster == LoyalDevnet)
}

func stateProductAssetFor(cluster LoyalCluster, mint string) (earncatalog.Product, error) {
	for _, asset := range stateProductAssets(cluster) {
		if asset.Mint == mint {
			return asset, nil
		}
	}
	return earncatalog.Product{}, errors.New("mint is not a supported Earn product mint.")
}

var stateKnownMarketName = map[string]string{
	"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF": "Main Kamino",
	"27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J": "Main Kamino",
	"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA": "Prime Market",
	"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y": "Maple Market",
	"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8": "OnRe Market",
	"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF": "Ethena Market",
	"DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek": "JLP Market",
	"GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs": "Bitcoin Market",
	"CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw": "Superstate Market",
	"52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf": "Huma Market",
	"9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU": "Solstice Market",
}

func stateKnownMintSymbol(mint string) string {
	for _, asset := range append(stateProductAssets(LoyalMainnetBeta), stateProductAssets(LoyalDevnet)...) {
		if asset.Mint == mint {
			return asset.Symbol
		}
	}
	return stateShortenAddress(mint)
}

func stateShortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:4] + "..." + address[len(address)-4:]
}

func statePositionDisplay(liquidityMint string, market *string) EarnPositionDisplay {
	marketName := "Unknown Market"
	if market != nil {
		if known, ok := stateKnownMarketName[*market]; ok {
			marketName = known
		} else {
			marketName = stateShortenAddress(*market)
		}
	}
	mintSymbol := stateKnownMintSymbol(liquidityMint)
	return EarnPositionDisplay{Label: marketName + " · " + mintSymbol, MarketName: marketName, MintSymbol: mintSymbol}
}

// stateTimescaleReserveForPosition ports resolveTimescaleReserveForPosition
// : the devnet Earn venue (the devnet USDC target's
// reserve, market and liquidity mint together) is read through its mainnet
// twin reserve, which is what the Timescale feed samples.
func stateTimescaleReserveForPosition(position stateYieldPositionRow) string {
	const (
		devnetReserve      = "9uKMtFU9UJ9DfbwzCReGENb31appi79KTEeDGdCnvMjy"
		devnetMarket       = "27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J"
		mainnetTwinReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
	)
	if position.CurrentReserve == devnetReserve &&
		position.CurrentMarket != nil && *position.CurrentMarket == devnetMarket &&
		position.CurrentLiquidityMint == earncatalog.DevnetUSDC {
		return mainnetTwinReserve
	}
	return position.CurrentReserve
}

func serializeEarnPositionHolding(row stateVaultReserveRow, cluster LoyalCluster) (EarnPositionHolding, error) {
	asset, err := stateProductAssetFor(cluster, row.LiquidityMint)
	if err != nil {
		return EarnPositionHolding{}, err
	}
	display := statePositionDisplay(row.LiquidityMint, row.Market)
	snapshotID := strconvFormatInt(row.SnapshotID)
	return EarnPositionHolding{
		AmountRaw:     strconvFormatInt(row.AmountRaw),
		Kind:          "kamino",
		Label:         display.Label,
		LiquidityMint: row.LiquidityMint,
		Market:        stateStringPtr(row.Market),
		MarketName:    display.MarketName,
		ObservedAt:    isoMilliseconds(row.ObservedAt),
		ObservedSlot:  strconvFormatInt(row.ObservedSlot),
		Provenance: EarnPositionProvenance{
			SnapshotID: &snapshotID,
			Source:     &stateSourceVaultReserve,
		},
		Reserve:      stateStringPtr(&row.Reserve),
		SourceID:     "reserve:" + row.Reserve,
		SupplyApyBps: stateInt64Ptr(row.SupplyApyBps),
		TokenProgram: asset.TokenProgram,
	}, nil
}

func serializeEarnPositionIdleHolding(row stateVaultIdleRow, cluster LoyalCluster) (EarnPositionHolding, error) {
	asset, err := stateProductAssetFor(cluster, row.Mint)
	if err != nil {
		return EarnPositionHolding{}, err
	}
	owner := row.Owner
	tokenAccount := row.TokenAccount
	sourceCommitment := row.SourceCommitment
	return EarnPositionHolding{
		AmountRaw:     strconvFormatInt(row.AmountRaw),
		Kind:          "idle",
		Label:         "Idle Balance",
		LiquidityMint: row.Mint,
		Market:        nil,
		MarketName:    asset.Symbol,
		ObservedAt:    isoMilliseconds(row.ObservedAt),
		ObservedSlot:  strconvFormatInt(row.ObservedSlot),
		Provenance: EarnPositionProvenance{
			Owner:            &owner,
			Source:           &stateSourceVaultIdle,
			SourceCommitment: &sourceCommitment,
			TokenAccount:     &tokenAccount,
		},
		Reserve:      nil,
		SourceID:     "idle:" + row.TokenAccount,
		SupplyApyBps: nil,
		TokenProgram: asset.TokenProgram,
	}, nil
}

func serializeEarnPositionCurrentHoldingAsKamino(position stateYieldPositionRow, cluster LoyalCluster) (EarnPositionHolding, error) {
	asset, err := stateProductAssetFor(cluster, position.CurrentLiquidityMint)
	if err != nil {
		return EarnPositionHolding{}, err
	}
	display := statePositionDisplay(position.CurrentLiquidityMint, position.CurrentMarket)
	reserve := position.CurrentReserve
	return EarnPositionHolding{
		AmountRaw:     strconvFormatInt(position.CurrentAmountRaw),
		Kind:          "kamino",
		Label:         display.Label,
		LiquidityMint: position.CurrentLiquidityMint,
		Market:        stateStringPtr(position.CurrentMarket),
		MarketName:    display.MarketName,
		ObservedAt:    isoMilliseconds(position.CurrentObservedAt),
		ObservedSlot:  strconvFormatInt(position.CurrentObservedSlot),
		Provenance: EarnPositionProvenance{
			LastHoldingEvent: stateInt64Ptr(position.LastHoldingEventID),
			Source:           &stateSourceUserYieldPositions,
		},
		Reserve:      &reserve,
		SourceID:     "reserve:" + position.CurrentReserve,
		SupplyApyBps: nil,
		TokenProgram: asset.TokenProgram,
	}, nil
}

func serializeEarnPositionHoldings(position stateYieldPositionRow, reserveRows []stateVaultReserveRow, idleRows []stateVaultIdleRow, cluster LoyalCluster) ([]EarnPositionHolding, error) {
	var idle []EarnPositionHolding
	for _, row := range idleRows {
		if row.AmountRaw <= 0 {
			continue
		}
		holding, err := serializeEarnPositionIdleHolding(row, cluster)
		if err != nil {
			return nil, err
		}
		idle = append(idle, holding)
	}
	var kamino []EarnPositionHolding
	if len(reserveRows) > 0 {
		kamino = make([]EarnPositionHolding, 0, len(reserveRows))
		for _, row := range reserveRows {
			holding, err := serializeEarnPositionHolding(row, cluster)
			if err != nil {
				return nil, err
			}
			kamino = append(kamino, holding)
		}
	} else if len(idle) == 0 && position.CurrentAmountRaw > 0 {
		fallback, err := serializeEarnPositionCurrentHoldingAsKamino(position, cluster)
		if err != nil {
			return nil, err
		}
		kamino = []EarnPositionHolding{fallback}
	}
	return append(kamino, idle...), nil
}

// serializeEarnPosition ports serializePosition. The
// total is the holdings sum floored at zero, emitted as both
// currentTotalAmountRaw and currentTotalNominalUsdMicros.
func serializeEarnPosition(position stateYieldPositionRow, currentSupplyApyBps *string, reserveRows []stateVaultReserveRow, idleRows []stateVaultIdleRow, cluster LoyalCluster) (*EarnPosition, error) {
	holdings, err := serializeEarnPositionHoldings(position, reserveRows, idleRows, cluster)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, holding := range holdings {
		value, err := strconv.ParseInt(holding.AmountRaw, 10, 64)
		if err != nil {
			continue
		}
		total += value
	}
	if total < 0 {
		total = 0
	}
	return &EarnPosition{
		CurrentHolding: EarnPositionCurrentHolding{
			AmountRaw:     strconvFormatInt(position.CurrentAmountRaw),
			LiquidityMint: position.CurrentLiquidityMint,
			Market:        stateStringPtr(position.CurrentMarket),
			ObservedAt:    isoMilliseconds(position.CurrentObservedAt),
			ObservedSlot:  strconvFormatInt(position.CurrentObservedSlot),
			Provenance: EarnPositionProvenance{
				LastHoldingEvent:      stateInt64Ptr(position.LastHoldingEventID),
				LastRebalanceDecision: stateInt64Ptr(position.LastRebalanceDecisionID),
			},
			Reserve: position.CurrentReserve,
		},
		CurrentSupplyApyBps: currentSupplyApyBps,
		Display:             statePositionDisplay(position.CurrentLiquidityMint, position.CurrentMarket),
		ID:                  strconvFormatInt(position.ID),
		InitialHolding: EarnPositionInitialHolding{
			LiquidityMint: position.InitialLiquidityMint,
			Market:        stateStringPtr(position.InitialMarket),
			Reserve:       position.InitialReserve,
			SupplyApyBps:  stateInt64Ptr(position.InitialSupplyApyBps),
		},
		Holdings:                     holdings,
		CurrentTotalAmountRaw:        strconvFormatInt(total),
		CurrentTotalNominalUsdMicros: strconvFormatInt(total),
		PrincipalAmountRaw:           strconvFormatInt(position.PrincipalAmountRaw),
		Status:                       position.Status,
	}, nil
}
