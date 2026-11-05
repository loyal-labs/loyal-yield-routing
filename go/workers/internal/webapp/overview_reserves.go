package webapp

import (
	"context"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

var overviewMediumMarkets = append(append([]string(nil), dtargetSafeMarkets[LoyalMainnetBeta]...),
	"DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek",
	"GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs",
	"CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw",
)

var overviewRiskBaskets = map[string][]string{
	"safe":   dtargetSafeMarkets[LoyalMainnetBeta],
	"medium": overviewMediumMarkets,
	"aggressive": append(append([]string(nil), overviewMediumMarkets...),
		"52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf",
		"9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU",
		"5wJeMrUYECGq41fxRESKALVcHnNX26TAWy4W98yULsua",
		"ByYiZxp8QrdN9qbdtaAiePN8AAr3qvTPppNJDpf5DVJ5",
	),
}

func overviewRiskBasketMarkets(riskProfile string) ([]string, bool) {
	if riskProfile == "" {
		riskProfile = "safe"
	}
	markets, ok := overviewRiskBaskets[riskProfile]
	return markets, ok
}

type overviewReserve struct {
	ObservedAt             string   `json:"observedAt"`
	Slot                   int64    `json:"slot"`
	Source                 string   `json:"source"`
	Reserve                string   `json:"reserve"`
	Market                 *string  `json:"market"`
	MarketName             *string  `json:"marketName"`
	Symbol                 *string  `json:"symbol"`
	LiquidityMint          string   `json:"liquidityMint"`
	SupplyApy              float64  `json:"supplyApy"`
	BorrowApy              float64  `json:"borrowApy"`
	Utilization            float64  `json:"utilization"`
	TotalSupplyUsdEstimate float64  `json:"totalSupplyUsdEstimate"`
	TotalBorrowUsdEstimate float64  `json:"totalBorrowUsdEstimate"`
	ReserveLastUpdateStale bool     `json:"reserveLastUpdateStale"`
	DiffChanged            bool     `json:"diffChanged"`
	ChangedFields          []string `json:"changedFields"`
	DiffSummary            string   `json:"diffSummary"`
	Stablecoin             string   `json:"stablecoin"`
}

func (o *overviewRoutes) bestApyReserves(ctx context.Context, markets []string) ([]overviewReserve, error) {
	best := []overviewReserve{}
	if o.Timescale == nil {
		return best, nil
	}
	stablecoins := map[string]string{}
	var mints []string
	for _, product := range earncatalog.Mainnet() {
		stablecoins[product.Mint] = product.Symbol
		mints = append(mints, product.Mint)
	}
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := o.Timescale.Query(queryCtx, `
		WITH `+dtargetConfirmedCurrentReservesSQL+`
		SELECT latest.observed_at, latest.slot, latest.source, latest.reserve, latest.market,
		       latest.market_name, latest.symbol, latest.liquidity_mint, latest.supply_apy::float8,
		       latest.borrow_apy::float8, latest.utilization::float8, latest.total_supply_usd_estimate::float8,
		       latest.total_borrow_usd_estimate::float8, latest.reserve_last_update_stale, latest.diff_changed,
		       latest.changed_fields, latest.diff_summary, frontier.slot
		FROM confirmed_current_reserves AS latest
`+dtargetFrontierFreshnessSQL+`
		WHERE latest.total_supply_usd_estimate > $2::float8
		  AND latest.supply_apy >= 0
		  AND latest.supply_apy < $3::float8
		  AND latest.market = ANY($4::text[])
		  AND latest.liquidity_mint = ANY($5::text[])
		ORDER BY latest.supply_apy DESC`,
		dtargetReserveStreamMaxAgeMS, dtargetMinVenueTotalSupplyUSD, dtargetMaxSupplyApy, markets, mints)
	if err != nil {
		return nil, fmt.Errorf("fetch eligible Kamino reserves: %w", err)
	}
	defer rows.Close()
	picked := map[string]overviewReserve{}
	var frontierSlot *int64
	for rows.Next() {
		var row overviewReserve
		var observedAt time.Time
		var slot int64
		if err := rows.Scan(&observedAt, &row.Slot, &row.Source, &row.Reserve, &row.Market, &row.MarketName, &row.Symbol, &row.LiquidityMint,
			&row.SupplyApy, &row.BorrowApy, &row.Utilization, &row.TotalSupplyUsdEstimate, &row.TotalBorrowUsdEstimate,
			&row.ReserveLastUpdateStale, &row.DiffChanged, &row.ChangedFields, &row.DiffSummary, &slot); err != nil {
			return nil, err
		}
		if frontierSlot == nil {
			frontierSlot = &slot
		}
		row.ObservedAt = observedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		if row.ChangedFields == nil {
			row.ChangedFields = []string{}
		}
		row.Stablecoin = stablecoins[row.LiquidityMint]
		if current, ok := picked[row.Stablecoin]; !ok || current.SupplyApy < row.SupplyApy {
			picked[row.Stablecoin] = row
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !dtargetRecentChainFrontier(ctx, dprepChainBlockTime(o.Chain), frontierSlot) {
		return best, nil
	}
	for _, product := range earncatalog.Mainnet() {
		if row, ok := picked[product.Symbol]; ok {
			best = append(best, row)
		}
	}
	return best, nil
}
