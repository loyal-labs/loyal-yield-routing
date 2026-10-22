package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type EarnFlowPoint struct {
	Date          string `json:"date"`
	DepositedRaw  string `json:"depositedRaw"`
	LiquidityMint string `json:"liquidityMint"`
	NetRaw        string `json:"netRaw"`
	WithdrawnRaw  string `json:"withdrawnRaw"`
}

type EarnPositionRow struct {
	CollateralReserveRows         int    `json:"collateralReserveRows"`
	CollateralStoredAmountRaw     string `json:"collateralStoredAmountRaw"`
	CurrentObservedAt             string `json:"currentObservedAt"`
	CurrentPointerDeltaRaw        string `json:"currentPointerDeltaRaw"`
	CurrentReserve                string `json:"currentReserve"`
	DepositMint                   string `json:"depositMint"`
	ExcludedReserveRaw            string `json:"excludedReserveRaw"`
	IdleAmountRaw                 string `json:"idleAmountRaw"`
	MissingManagedVaultRows       int    `json:"missingManagedVaultRows"`
	MissingRedeemableMetadataRows int    `json:"missingRedeemableMetadataRows"`
	NormalizedAumRaw              string `json:"normalizedAumRaw"`
	NormalizedReserveRaw          string `json:"normalizedReserveRaw"`
	PrincipalAmountRaw            string `json:"principalAmountRaw"`
	RedeemableReserveRows         int    `json:"redeemableReserveRows"`
	Settings                      string `json:"settings"`
	StoredCurrentPointerRaw       string `json:"storedCurrentPointerRaw"`
	UnknownReserveSemanticsRows   int    `json:"unknownReserveSemanticsRows"`
	WalletAddress                 string `json:"walletAddress"`
}

type EarnStablecoinSummary struct {
	ActiveAumRaw                  string  `json:"activeAumRaw"`
	ActiveExcludedReserveRaw      string  `json:"activeExcludedReserveRaw"`
	ActiveIdleRaw                 string  `json:"activeIdleRaw"`
	ActivePositionCount           int     `json:"activePositionCount"`
	ActivePrincipalRaw            string  `json:"activePrincipalRaw"`
	ActiveReserveRaw              string  `json:"activeReserveRaw"`
	ActiveStoredCurrentPointerRaw string  `json:"activeStoredCurrentPointerRaw"`
	CurrentPointerDeltaRaw        string  `json:"currentPointerDeltaRaw"`
	Deposited30dRaw               string  `json:"deposited30dRaw"`
	LatestRebalanceAt             *string `json:"latestRebalanceAt"`
	LiquidityMint                 string  `json:"liquidityMint"`
	Symbol                        string  `json:"symbol"`
	Withdrawn30dRaw               string  `json:"withdrawn30dRaw"`
}

type EarnFreshnessMetric struct {
	Label     string  `json:"label"`
	Timestamp *string `json:"timestamp"`
}

type EarnAutodepositStatusCounts struct {
	Active       int `json:"active"`
	Closed       int `json:"closed"`
	Inconsistent int `json:"inconsistent"`
	Paused       int `json:"paused"`
	Pending      int `json:"pending"`
}

type EarnData struct {
	ActiveAumRaw                        string                      `json:"activeAumRaw"`
	ActiveAutodepositPolicies           int                         `json:"activeAutodepositPolicies"`
	ActiveCollateralStoredAmountRaw     string                      `json:"activeCollateralStoredAmountRaw"`
	ActiveCurrentPointerDeltaRaw        string                      `json:"activeCurrentPointerDeltaRaw"`
	ActiveExcludedReserveRaw            string                      `json:"activeExcludedReserveRaw"`
	ActiveIdleRaw                       string                      `json:"activeIdleRaw"`
	ActiveMissingManagedVaultRows       int                         `json:"activeMissingManagedVaultRows"`
	ActiveMissingRedeemableMetadataRows int                         `json:"activeMissingRedeemableMetadataRows"`
	ActivePrincipalRaw                  string                      `json:"activePrincipalRaw"`
	ActiveRedeemableReserveRows         int                         `json:"activeRedeemableReserveRows"`
	ActiveCollateralReserveRows         int                         `json:"activeCollateralReserveRows"`
	ActiveReserveRaw                    string                      `json:"activeReserveRaw"`
	ActiveStoredCurrentPointerRaw       string                      `json:"activeStoredCurrentPointerRaw"`
	ActiveUnknownReserveSemanticsRows   int                         `json:"activeUnknownReserveSemanticsRows"`
	AutodepositExecutionAmount30dRaw    string                      `json:"autodepositExecutionAmount30dRaw"`
	AutodepositExecutionAmountRaw       string                      `json:"autodepositExecutionAmountRaw"`
	AutodepositExecutionCount30d        int                         `json:"autodepositExecutionCount30d"`
	AutodepositExecutionCount           int                         `json:"autodepositExecutionCount"`
	AutodepositStatusCounts             EarnAutodepositStatusCounts `json:"autodepositStatusCounts"`
	Flow30d                             []EarnFlowPoint             `json:"flow30d" nullable:"false"`
	Freshness                           []EarnFreshnessMetric       `json:"freshness" nullable:"false"`
	ScheduledEligibleAmountRaw          string                      `json:"scheduledEligibleAmountRaw"`
	ScheduledEligibleLotCount           int                         `json:"scheduledEligibleLotCount"`
	ScheduledOpenAmountRaw              string                      `json:"scheduledOpenAmountRaw"`
	ScheduledOpenLotCount               int                         `json:"scheduledOpenLotCount"`
	Stablecoins                         []EarnStablecoinSummary     `json:"stablecoins" nullable:"false"`
	TopPositions                        []EarnPositionRow           `json:"topPositions" nullable:"false"`
	TotalDeposited30dRaw                string                      `json:"totalDeposited30dRaw"`
	TotalWithdrawn30dRaw                string                      `json:"totalWithdrawn30dRaw"`
	UniqueEarnPolicies                  int                         `json:"uniqueEarnPolicies"`
	UniqueEarnUsers                     int                         `json:"uniqueEarnUsers"`
}

type earnDecimalText string

func (d *earnDecimalText) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*d = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var text string
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		*d = earnDecimalText(text)
		return nil
	}
	*d = earnDecimalText(s)
	return nil
}

func (d earnDecimalText) big() *big.Int {
	if n, ok := new(big.Int).SetString(strings.TrimSpace(string(d)), 10); ok {
		return n
	}
	if f, ok := new(big.Float).SetString(strings.TrimSpace(string(d))); ok {
		n, _ := f.Int(nil)
		return n
	}
	return new(big.Int)
}

func (d earnDecimalText) raw() string { return d.big().String() }

func (d earnDecimalText) count() int {
	n := d.big()
	if !n.IsInt64() {
		return 0
	}
	return int(n.Int64())
}

func earnIsoText(text *string) *string {
	if text == nil || *text == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *text)
	if err != nil {
		return nil
	}
	s := earnISO(t)
	return &s
}

type earnHeadlineRow struct {
	ActiveAumRaw                        earnDecimalText `json:"active_aum_raw"`
	ActiveCollateralStoredAmountRaw     earnDecimalText `json:"active_collateral_stored_amount_raw"`
	ActiveCurrentPointerDeltaRaw        earnDecimalText `json:"active_current_pointer_delta_raw"`
	ActiveExcludedReserveRaw            earnDecimalText `json:"active_excluded_reserve_raw"`
	ActiveIdleRaw                       earnDecimalText `json:"active_idle_raw"`
	ActiveMissingManagedVaultRows       earnDecimalText `json:"active_missing_managed_vault_rows"`
	ActiveMissingRedeemableMetadataRows earnDecimalText `json:"active_missing_redeemable_metadata_rows"`
	ActivePrincipalRaw                  earnDecimalText `json:"active_principal_raw"`
	ActiveRedeemableReserveRows         earnDecimalText `json:"active_redeemable_reserve_rows"`
	ActiveCollateralReserveRows         earnDecimalText `json:"active_collateral_reserve_rows"`
	ActiveReserveRaw                    earnDecimalText `json:"active_reserve_raw"`
	ActiveStoredCurrentPointerRaw       earnDecimalText `json:"active_stored_current_pointer_raw"`
	ActiveUnknownReserveSemanticsRows   earnDecimalText `json:"active_unknown_reserve_semantics_rows"`
	ActiveAutodepositPolicies           earnDecimalText `json:"active_autodeposit_policies"`
	LatestPositionObservedAt            *string         `json:"latest_position_observed_at"`
	UniqueEarnPolicies                  earnDecimalText `json:"unique_earn_policies"`
	UniqueEarnUsers                     earnDecimalText `json:"unique_earn_users"`
}

type earnMintSummaryRow struct {
	ActiveAumRaw                  earnDecimalText `json:"active_aum_raw"`
	ActiveExcludedReserveRaw      earnDecimalText `json:"active_excluded_reserve_raw"`
	ActiveIdleRaw                 earnDecimalText `json:"active_idle_raw"`
	ActivePositionCount           earnDecimalText `json:"active_position_count"`
	ActivePrincipalRaw            earnDecimalText `json:"active_principal_raw"`
	ActiveReserveRaw              earnDecimalText `json:"active_reserve_raw"`
	ActiveStoredCurrentPointerRaw earnDecimalText `json:"active_stored_current_pointer_raw"`
	CurrentPointerDeltaRaw        earnDecimalText `json:"current_pointer_delta_raw"`
	DepositMint                   string          `json:"deposit_mint"`
	LatestRebalanceAt             *string         `json:"latest_rebalance_at"`
}

type earnPositionRow struct {
	CollateralReserveRows         earnDecimalText `json:"collateral_reserve_rows"`
	CollateralStoredAmountRaw     earnDecimalText `json:"collateral_stored_amount_raw"`
	CurrentObservedAt             *string         `json:"current_observed_at"`
	CurrentPointerDeltaRaw        earnDecimalText `json:"current_pointer_delta_raw"`
	CurrentReserve                string          `json:"current_reserve"`
	DepositMint                   string          `json:"deposit_mint"`
	ExcludedReserveRaw            earnDecimalText `json:"excluded_reserve_raw"`
	IdleRaw                       earnDecimalText `json:"idle_raw"`
	MissingManagedVaultRows       earnDecimalText `json:"missing_managed_vault_rows"`
	MissingRedeemableMetadataRows earnDecimalText `json:"missing_redeemable_metadata_rows"`
	NormalizedAumRaw              earnDecimalText `json:"normalized_aum_raw"`
	NormalizedReserveRaw          earnDecimalText `json:"normalized_reserve_raw"`
	PrincipalAmountRaw            earnDecimalText `json:"principal_amount_raw"`
	RedeemableReserveRows         earnDecimalText `json:"redeemable_reserve_rows"`
	Settings                      string          `json:"settings"`
	StoredCurrentPointerRaw       earnDecimalText `json:"stored_current_pointer_raw"`
	UnknownReserveSemanticsRows   earnDecimalText `json:"unknown_reserve_semantics_rows"`
	WalletAddress                 string          `json:"wallet_address"`
}

type earnFlowRow struct {
	Day           string          `json:"day"`
	DepositedRaw  earnDecimalText `json:"deposited_raw"`
	LiquidityMint string          `json:"liquidity_mint"`
	WithdrawnRaw  earnDecimalText `json:"withdrawn_raw"`
}

type earnStatusRow struct {
	Status string          `json:"status"`
	Total  earnDecimalText `json:"total"`
}

type earnScheduledRow struct {
	EligibleAmountRaw earnDecimalText `json:"eligible_amount_raw"`
	EligibleLotCount  earnDecimalText `json:"eligible_lot_count"`
	OpenAmountRaw     earnDecimalText `json:"open_amount_raw"`
	OpenLotCount      earnDecimalText `json:"open_lot_count"`
}

type earnExecutionRow struct {
	Amount30dRaw earnDecimalText `json:"amount_30d_raw"`
	AmountRaw    earnDecimalText `json:"amount_raw"`
	Count30d     earnDecimalText `json:"count_30d"`
	Count        earnDecimalText `json:"count"`
}

type earnFreshnessRow struct {
	LatestPositionObservedAt           *string `json:"latest_position_observed_at"`
	LatestTargetSeenAt                 *string `json:"latest_target_seen_at"`
	LatestWalletBalanceEventObservedAt *string `json:"latest_wallet_balance_event_observed_at"`
	LatestWalletBalanceObservedAt      *string `json:"latest_wallet_balance_observed_at"`
	LatestWalletBalanceProjectedAt     *string `json:"latest_wallet_balance_projected_at"`
	LatestSweepExecutionReceivedAt     *string `json:"latest_sweep_execution_received_at"`
}

func earnDecodeColumn(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

func earnSmallMetricsQuery() string {
	values := make([]string, len(EarnStablecoins))
	for i, coin := range EarnStablecoins {
		values[i] = fmt.Sprintf("('%s')", coin.Mint)
	}
	return strings.Replace(earnSmallMetricsTemplate, "__MINTS__", strings.Join(values, ", "), 1)
}

func (d *Deps) EarnData(ctx context.Context) (EarnData, error) {
	var holdings, mints, tops []byte
	var flow, status, scheduled, executions, freshness []byte
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, earnCombinedHoldingsQuery).Scan(&holdings, &mints, &tops); err != nil {
			return err
		}
		return tx.QueryRow(ctx, earnSmallMetricsQuery()).Scan(&flow, &status, &scheduled, &executions, &freshness)
	})
	if err != nil {
		return EarnData{}, err
	}

	var headline earnHeadlineRow
	var mintRows []earnMintSummaryRow
	var positionRows []earnPositionRow
	var flowRows []earnFlowRow
	var statusRows []earnStatusRow
	var schedule earnScheduledRow
	var execution earnExecutionRow
	var fresh earnFreshnessRow
	for _, column := range []struct {
		raw  []byte
		into any
	}{
		{holdings, &headline}, {mints, &mintRows}, {tops, &positionRows},
		{flow, &flowRows}, {status, &statusRows}, {scheduled, &schedule}, {executions, &execution}, {freshness, &fresh},
	} {
		if err := earnDecodeColumn(column.raw, column.into); err != nil {
			return EarnData{}, fmt.Errorf("earn data: %w", err)
		}
	}
	if headline.LatestPositionObservedAt != nil {
		fresh.LatestPositionObservedAt = headline.LatestPositionObservedAt
	}

	counts := EarnAutodepositStatusCounts{}
	for _, row := range statusRows {
		n := row.Total.count()
		switch row.Status {
		case "active":
			counts.Active = n
		case "closed":
			counts.Closed = n
		case "inconsistent":
			counts.Inconsistent = n
		case "paused":
			counts.Paused = n
		case "pending":
			counts.Pending = n
		}
	}

	totalDeposited, totalWithdrawn := new(big.Int), new(big.Int)
	flow30d := make([]EarnFlowPoint, 0, len(flowRows))
	for _, row := range flowRows {
		deposited, withdrawn := row.DepositedRaw.big(), row.WithdrawnRaw.big()
		totalDeposited.Add(totalDeposited, deposited)
		totalWithdrawn.Add(totalWithdrawn, withdrawn)
		flow30d = append(flow30d, EarnFlowPoint{
			Date:          row.Day,
			DepositedRaw:  deposited.String(),
			LiquidityMint: row.LiquidityMint,
			NetRaw:        new(big.Int).Sub(deposited, withdrawn).String(),
			WithdrawnRaw:  withdrawn.String(),
		})
	}

	mintByDeposit := map[string]earnMintSummaryRow{}
	for _, row := range mintRows {
		mintByDeposit[row.DepositMint] = row
	}
	stablecoins := make([]EarnStablecoinSummary, 0, len(EarnStablecoins))
	for _, coin := range EarnStablecoins {
		holding := mintByDeposit[coin.Mint]
		deposited, withdrawn := new(big.Int), new(big.Int)
		for _, row := range flowRows {
			if row.LiquidityMint == coin.Mint {
				deposited.Add(deposited, row.DepositedRaw.big())
				withdrawn.Add(withdrawn, row.WithdrawnRaw.big())
			}
		}
		stablecoins = append(stablecoins, EarnStablecoinSummary{
			ActiveAumRaw:                  holding.ActiveAumRaw.raw(),
			ActiveExcludedReserveRaw:      holding.ActiveExcludedReserveRaw.raw(),
			ActiveIdleRaw:                 holding.ActiveIdleRaw.raw(),
			ActivePositionCount:           holding.ActivePositionCount.count(),
			ActivePrincipalRaw:            holding.ActivePrincipalRaw.raw(),
			ActiveReserveRaw:              holding.ActiveReserveRaw.raw(),
			ActiveStoredCurrentPointerRaw: holding.ActiveStoredCurrentPointerRaw.raw(),
			CurrentPointerDeltaRaw:        holding.CurrentPointerDeltaRaw.raw(),
			Deposited30dRaw:               deposited.String(),
			LatestRebalanceAt:             earnIsoText(holding.LatestRebalanceAt),
			LiquidityMint:                 coin.Mint,
			Symbol:                        coin.Symbol,
			Withdrawn30dRaw:               withdrawn.String(),
		})
	}

	topPositions := make([]EarnPositionRow, 0, len(positionRows))
	for _, row := range positionRows {
		observed := ""
		if iso := earnIsoText(row.CurrentObservedAt); iso != nil {
			observed = *iso
		}
		topPositions = append(topPositions, EarnPositionRow{
			CollateralReserveRows:         row.CollateralReserveRows.count(),
			CollateralStoredAmountRaw:     row.CollateralStoredAmountRaw.raw(),
			CurrentObservedAt:             observed,
			CurrentPointerDeltaRaw:        row.CurrentPointerDeltaRaw.raw(),
			CurrentReserve:                row.CurrentReserve,
			DepositMint:                   row.DepositMint,
			ExcludedReserveRaw:            row.ExcludedReserveRaw.raw(),
			IdleAmountRaw:                 row.IdleRaw.raw(),
			MissingManagedVaultRows:       row.MissingManagedVaultRows.count(),
			MissingRedeemableMetadataRows: row.MissingRedeemableMetadataRows.count(),
			NormalizedAumRaw:              row.NormalizedAumRaw.raw(),
			NormalizedReserveRaw:          row.NormalizedReserveRaw.raw(),
			PrincipalAmountRaw:            row.PrincipalAmountRaw.raw(),
			RedeemableReserveRows:         row.RedeemableReserveRows.count(),
			Settings:                      row.Settings,
			StoredCurrentPointerRaw:       row.StoredCurrentPointerRaw.raw(),
			UnknownReserveSemanticsRows:   row.UnknownReserveSemanticsRows.count(),
			WalletAddress:                 row.WalletAddress,
		})
	}

	return EarnData{
		ActiveAumRaw:                        headline.ActiveAumRaw.raw(),
		ActiveAutodepositPolicies:           headline.ActiveAutodepositPolicies.count(),
		ActiveCollateralStoredAmountRaw:     headline.ActiveCollateralStoredAmountRaw.raw(),
		ActiveCurrentPointerDeltaRaw:        headline.ActiveCurrentPointerDeltaRaw.raw(),
		ActiveExcludedReserveRaw:            headline.ActiveExcludedReserveRaw.raw(),
		ActiveIdleRaw:                       headline.ActiveIdleRaw.raw(),
		ActiveMissingManagedVaultRows:       headline.ActiveMissingManagedVaultRows.count(),
		ActiveMissingRedeemableMetadataRows: headline.ActiveMissingRedeemableMetadataRows.count(),
		ActivePrincipalRaw:                  headline.ActivePrincipalRaw.raw(),
		ActiveRedeemableReserveRows:         headline.ActiveRedeemableReserveRows.count(),
		ActiveCollateralReserveRows:         headline.ActiveCollateralReserveRows.count(),
		ActiveReserveRaw:                    headline.ActiveReserveRaw.raw(),
		ActiveStoredCurrentPointerRaw:       headline.ActiveStoredCurrentPointerRaw.raw(),
		ActiveUnknownReserveSemanticsRows:   headline.ActiveUnknownReserveSemanticsRows.count(),
		AutodepositExecutionAmount30dRaw:    execution.Amount30dRaw.raw(),
		AutodepositExecutionAmountRaw:       execution.AmountRaw.raw(),
		AutodepositExecutionCount30d:        execution.Count30d.count(),
		AutodepositExecutionCount:           execution.Count.count(),
		AutodepositStatusCounts:             counts,
		Flow30d:                             flow30d,
		Freshness:                           earnFreshnessMetrics(fresh),
		ScheduledEligibleAmountRaw:          schedule.EligibleAmountRaw.raw(),
		ScheduledEligibleLotCount:           schedule.EligibleLotCount.count(),
		ScheduledOpenAmountRaw:              schedule.OpenAmountRaw.raw(),
		ScheduledOpenLotCount:               schedule.OpenLotCount.count(),
		Stablecoins:                         stablecoins,
		TopPositions:                        topPositions,
		TotalDeposited30dRaw:                totalDeposited.String(),
		TotalWithdrawn30dRaw:                totalWithdrawn.String(),
		UniqueEarnPolicies:                  headline.UniqueEarnPolicies.count(),
		UniqueEarnUsers:                     headline.UniqueEarnUsers.count(),
	}, nil
}

func earnFreshnessMetrics(row earnFreshnessRow) []EarnFreshnessMetric {
	return []EarnFreshnessMetric{
		{"Earn positions", earnIsoText(row.LatestPositionObservedAt)},
		{"Autodeposit targets", earnIsoText(row.LatestTargetSeenAt)},
		{"Sweep executions", earnIsoText(row.LatestSweepExecutionReceivedAt)},
		{"Wallet balances current", earnIsoText(row.LatestWalletBalanceObservedAt)},
		{"Wallet balance events", earnIsoText(row.LatestWalletBalanceEventObservedAt)},
		{"Wallet balance projections", earnIsoText(row.LatestWalletBalanceProjectedAt)},
	}
}
