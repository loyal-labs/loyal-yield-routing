package webapp

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"
)

const (
	dtargetSelectionRetries    = 2
	dtargetSelectionRetryDelay = 700 * time.Millisecond
)

// dtargetEarnReserveTarget mirrors EarnUsdcReserveTarget
// : base58 addresses plus the supply APY
// in basis points; nil SupplyApyBps is "no APY" for a position-derived or
// static target, never zero.
type dtargetEarnReserveTarget struct {
	Reserve               string
	Market                string
	LiquidityMint         string
	LiquidityTokenProgram string
	SupplyApyBps          *int64
}

// dtargetActivePosition is the reconciled-position slice the selection reads
// (Pick<UserYieldPositionRecord, "currentLiquidityMint" | "currentMarket" |
// "currentReserve">); CurrentMarket is null until the position reconciles.
type dtargetActivePosition struct {
	CurrentLiquidityMint string
	CurrentMarket        *string
	CurrentReserve       string
}

// dtargetReserveIneligibility mirrors EarnReserveIneligibilityReason
// ; the empty string means eligible.
type dtargetReserveIneligibility string

const (
	dtargetBelowLiquidityFloor dtargetReserveIneligibility = "below_liquidity_floor"
	dtargetUnsampledReserve    dtargetReserveIneligibility = "unsampled_reserve"
)

type dtargetProductMint struct {
	Symbol       string
	Mint         string
	TokenProgram string
}

// dtargetProductMints lists mainnet Earn products in STABLECOINS order
// with the token programs
// from STABLECOIN_TOKEN_PROGRAMS.
var dtargetProductMints = []dtargetProductMint{
	{"CASH", "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"},
	{"USDG", "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"},
	{"PYUSD", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"},
	{"USDC", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
	{"USDT", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
	{"USDS", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
}

const (
	dtargetUSDCMint               = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	dtargetDevnetUSDCMint         = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	dtargetKaminoMainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	dtargetDevnetMainMarket       = "27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J"
	dtargetDevnetMainUSDCReserve  = "9uKMtFU9UJ9DfbwzCReGENb31appi79KTEeDGdCnvMjy"
	dtargetMainnetMainUSDCReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
	dtargetSplTokenProgram        = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	dtargetSplToken2022Program    = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
)

var dtargetSafeMarkets = map[LoyalCluster][]string{
	LoyalMainnetBeta: {
		"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
		"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
		"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
		"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
		"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
	},
	LoyalDevnet: {dtargetDevnetMainMarket},
}

func dtargetProductMintsForCluster(cluster LoyalCluster) []dtargetProductMint {
	if cluster == LoyalDevnet {
		return []dtargetProductMint{{Symbol: "USDC", Mint: dtargetDevnetUSDCMint, TokenProgram: dtargetSplTokenProgram}}
	}
	return dtargetProductMints
}

func dtargetResolveEarnProductAsset(cluster LoyalCluster, mint string) (dtargetProductMint, error) {
	for _, product := range dtargetProductMintsForCluster(cluster) {
		if product.Mint == mint {
			return product, nil
		}
	}
	return dtargetProductMint{}, errors.New("mint is not a supported Earn product mint.")
}

// dtargetSupplyApyToBps mirrors supplyApyToBps
// . Math.round rounds halves toward
// +Infinity and math.Round away from zero; the eligible-reserve query only
// admits non-negative APYs, where the two agree.
func dtargetSupplyApyToBps(supplyApy float64) int64 {
	return int64(math.Round(supplyApy * 10_000))
}

func dtargetMainUsdcEarnReserveTarget(cluster LoyalCluster) dtargetEarnReserveTarget {
	if cluster == LoyalDevnet {
		return dtargetEarnReserveTarget{
			Reserve:               dtargetDevnetMainUSDCReserve,
			Market:                dtargetDevnetMainMarket,
			LiquidityMint:         dtargetDevnetUSDCMint,
			LiquidityTokenProgram: dtargetSplTokenProgram,
		}
	}
	return dtargetEarnReserveTarget{
		Reserve:               dtargetMainnetMainUSDCReserve,
		Market:                dtargetKaminoMainMarket,
		LiquidityMint:         dtargetUSDCMint,
		LiquidityTokenProgram: dtargetSplTokenProgram,
	}
}

func dtargetEarnReserveTargetFromActivePosition(position dtargetActivePosition, cluster LoyalCluster) (*dtargetEarnReserveTarget, error) {
	if position.CurrentMarket == nil || *position.CurrentMarket == "" {
		return nil, errors.New("Active Earn position is missing current reserve market.")
	}
	product, err := dtargetResolveEarnProductAsset(cluster, position.CurrentLiquidityMint)
	if err != nil {
		return nil, err
	}
	return &dtargetEarnReserveTarget{
		Reserve:               position.CurrentReserve,
		Market:                *position.CurrentMarket,
		LiquidityMint:         position.CurrentLiquidityMint,
		LiquidityTokenProgram: product.TokenProgram,
	}, nil
}

// dtargetBestApyReserveByStablecoin mirrors selectCurrentBestApyReserveByStablecoin
// : the highest supply APY per
// stablecoin, unknown mints dropped, ties keeping the earlier row, output in
// STABLECOINS order. Rows arrive ordered by supply_apy DESC (:769).
func dtargetBestApyReserveByStablecoin(rows []dtargetReserveRow) []dtargetReserveRow {
	mintToSymbol := make(map[string]string, len(dtargetProductMints))
	for _, product := range dtargetProductMints {
		mintToSymbol[product.Mint] = product.Symbol
	}
	best := make(map[string]dtargetReserveRow, len(dtargetProductMints))
	for _, row := range rows {
		symbol, ok := mintToSymbol[row.LiquidityMint]
		if !ok {
			continue
		}
		if current, has := best[symbol]; has && current.SupplyApy >= row.SupplyApy {
			continue
		}
		best[symbol] = row
	}
	out := make([]dtargetReserveRow, 0, len(best))
	for _, product := range dtargetProductMints {
		if row, ok := best[product.Symbol]; ok {
			out = append(out, row)
		}
	}
	return out
}

func dtargetSelectBestSafeEarnReserveTarget(cluster LoyalCluster, product dtargetProductMint, rows []dtargetReserveRow) (*dtargetEarnReserveTarget, error) {
	safeMarkets := make(map[string]bool, len(dtargetSafeMarkets[cluster]))
	for _, market := range dtargetSafeMarkets[cluster] {
		safeMarkets[market] = true
	}
	for _, candidate := range dtargetBestApyReserveByStablecoin(rows) {
		if candidate.Market == nil {
			continue
		}
		if candidate.LiquidityMint == product.Mint && safeMarkets[*candidate.Market] {
			return dtargetReserveRowToTarget(product, candidate)
		}
	}
	return nil, nil
}

// dtargetReserveRowToTarget mirrors reserveRowToTarget
// : the row's token program is the
// product's, since Timescale tracks the liquidity mint.
func dtargetReserveRowToTarget(product dtargetProductMint, row dtargetReserveRow) (*dtargetEarnReserveTarget, error) {
	if row.Market == nil {
		return nil, errors.New("Kamino reserve candidate is missing a market.")
	}
	supplyApyBps := dtargetSupplyApyToBps(row.SupplyApy)
	return &dtargetEarnReserveTarget{
		Reserve:               row.Reserve,
		Market:                *row.Market,
		LiquidityMint:         row.LiquidityMint,
		LiquidityTokenProgram: product.TokenProgram,
		SupplyApyBps:          &supplyApyBps,
	}, nil
}

// dtargetFindBestSafeEarnReserveTarget mirrors findBestSafeEarnReserveTarget
// : devnet falls back to the static
// USDC venue and never samples Timescale (the feed only tracks mainnet).
func dtargetFindBestSafeEarnReserveTarget(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, cluster LoyalCluster, product dtargetProductMint) (*dtargetEarnReserveTarget, error) {
	if cluster == LoyalDevnet {
		if product.Symbol != "USDC" {
			return nil, nil
		}
		target := dtargetMainUsdcEarnReserveTarget(cluster)
		return &target, nil
	}
	rows, err := dtargetCurrentEligibleSafeReserves(ctx, q, getBlockTime, dtargetSafeMarkets[cluster], dtargetStablecoinMints(cluster))
	if err != nil {
		return nil, err
	}
	return dtargetSelectBestSafeEarnReserveTarget(cluster, product, rows)
}

func dtargetStablecoinMints(cluster LoyalCluster) []string {
	products := dtargetProductMintsForCluster(cluster)
	mints := make([]string, 0, len(products))
	for _, product := range products {
		mints = append(mints, product.Mint)
	}
	return mints
}

func dtargetFindBestSafeEarnReserveTargetWithRetry(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, cluster LoyalCluster, product dtargetProductMint) (*dtargetEarnReserveTarget, error) {
	for attempt := 0; ; attempt++ {
		target, err := dtargetFindBestSafeEarnReserveTarget(ctx, q, getBlockTime, cluster, product)
		if err != nil {
			return nil, err
		}
		if target != nil || cluster == LoyalDevnet || attempt >= dtargetSelectionRetries {
			return target, nil
		}
		select {
		case <-time.After(dtargetSelectionRetryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// dtargetFindEarnReserveTargetIneligibility mirrors
// findEarnReserveTargetIneligibility:
// the Timescale feed only tracks mainnet, an unconfigured feed never blocks,
// and a reserve the observer does not sample or one without real liquidity is
// refused.
func dtargetFindEarnReserveTargetIneligibility(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, cluster LoyalCluster, reserve string) (dtargetReserveIneligibility, error) {
	if cluster != LoyalMainnetBeta {
		return "", nil
	}
	if q == nil {
		return "", nil
	}
	observations, err := dtargetLatestReserveObservations(ctx, q, getBlockTime, []string{reserve})
	if err != nil {
		return "", err
	}
	var observation *dtargetReserveObservation
	for i := range observations {
		if observations[i].Reserve == reserve {
			observation = &observations[i]
			break
		}
	}
	if observation == nil {
		return dtargetUnsampledReserve, nil
	}
	if observation.TotalSupplyUsdEstimate <= dtargetMinVenueTotalSupplyUSD {
		return dtargetBelowLiquidityFloor, nil
	}
	return "", nil
}

func dtargetResolveEligibleEarnDepositTarget(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, cluster LoyalCluster, liquidityMint string, logTag string, position dtargetActivePosition) (*dtargetEarnReserveTarget, error) {
	if position.CurrentLiquidityMint != liquidityMint {
		return nil, nil
	}
	target, err := dtargetEarnReserveTargetFromActivePosition(position, cluster)
	if err != nil {
		return nil, err
	}
	ineligibility, err := dtargetFindEarnReserveTargetIneligibility(ctx, q, getBlockTime, cluster, target.Reserve)
	if err != nil {
		return nil, err
	}
	if ineligibility == "" {
		return target, nil
	}
	slog.Error("refusing ineligible Earn deposit target; selecting another same-mint reserve",
		"tag", logTag,
		"cluster", string(cluster),
		"market", target.Market,
		"reason", string(ineligibility),
		"reserve", target.Reserve)
	return nil, nil
}
