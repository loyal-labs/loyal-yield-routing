package kamino

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const (
	fractionBits   = 60
	secondsPerYear = 365.25 * 24 * 60 * 60
)

type Target struct {
	Reserve           string   `json:"reserve"`
	Market            *string  `json:"market"`
	MarketName        *string  `json:"market_name"`
	Symbol            *string  `json:"symbol"`
	LiquidityMint     *string  `json:"liquidity_mint"`
	APISupplyAPY      *float64 `json:"api_supply_apy"`
	APIBorrowAPY      *float64 `json:"api_borrow_apy"`
	APITotalSupplyUSD *float64 `json:"api_total_supply_usd"`
	APITotalBorrowUSD *float64 `json:"api_total_borrow_usd"`
}

type CurvePoint struct {
	UtilizationRateBPS uint32 `json:"utilization_rate_bps"`
	BorrowRateBPS      uint32 `json:"borrow_rate_bps"`
}
type WithdrawalCap struct {
	ConfigCapacity             int64  `json:"config_capacity"`
	CurrentTotal               int64  `json:"current_total"`
	LastIntervalStartTimestamp uint64 `json:"last_interval_start_timestamp"`
	IntervalLengthSeconds      uint64 `json:"interval_length_seconds"`
}
type Snapshot struct {
	ObservationSchemaVersion               uint16         `json:"observation_schema_version"`
	ObservedAt                             time.Time      `json:"observed_at"`
	Slot                                   uint64         `json:"slot"`
	Reserve                                string         `json:"reserve"`
	Market                                 *string        `json:"market"`
	Symbol                                 *string        `json:"symbol"`
	LiquidityMint                          string         `json:"liquidity_mint"`
	MintDecimals                           uint64         `json:"mint_decimals"`
	ReserveLastUpdateSlot                  uint64         `json:"reserve_last_update_slot"`
	ReserveLastUpdateStale                 bool           `json:"reserve_last_update_stale"`
	ReservePriceStatus                     uint8          `json:"reserve_price_status"`
	AvailableAmount                        float64        `json:"available_amount"`
	BorrowedAmount                         float64        `json:"borrowed_amount"`
	BorrowedAmountSF                       string         `json:"borrowed_amount_sf"`
	TotalSupplyAmount                      float64        `json:"total_supply_amount"`
	MarketPriceUSD                         float64        `json:"market_price_usd"`
	MarketPriceLastUpdatedTS               uint64         `json:"market_price_last_updated_ts"`
	CumulativeBorrowRateBSF                [4]uint64      `json:"cumulative_borrow_rate_bsf"`
	TotalSupplyUSDEstimate                 float64        `json:"total_supply_usd_estimate"`
	TotalBorrowUSDEstimate                 float64        `json:"total_borrow_usd_estimate"`
	Utilization                            float64        `json:"utilization"`
	BorrowAPR                              float64        `json:"borrow_apr"`
	SupplyAPR                              float64        `json:"supply_apr"`
	BorrowAPY                              float64        `json:"borrow_apy"`
	SupplyAPY                              float64        `json:"supply_apy"`
	ProtocolTakeRatePct                    uint8          `json:"protocol_take_rate_pct"`
	HostFixedInterestRateBPS               uint16         `json:"host_fixed_interest_rate_bps"`
	ReserveStatus                          uint8          `json:"reserve_status"`
	EmergencyMode                          bool           `json:"emergency_mode"`
	LoanToValuePct                         uint8          `json:"loan_to_value_pct"`
	LiquidationThresholdPct                uint8          `json:"liquidation_threshold_pct"`
	BorrowFactorPct                        uint64         `json:"borrow_factor_pct"`
	DepositLimit                           uint64         `json:"deposit_limit"`
	BorrowLimit                            uint64         `json:"borrow_limit"`
	UtilizationLimitBlockBorrowingAbovePct uint8          `json:"utilization_limit_block_borrowing_above_pct"`
	DisableUsageAsCollOutsideEmode         bool           `json:"disable_usage_as_coll_outside_emode"`
	BorrowLimitOutsideElevationGroup       uint64         `json:"borrow_limit_outside_elevation_group"`
	BorrowedAmountOutsideElevationGroup    uint64         `json:"borrowed_amount_outside_elevation_group"`
	OriginationFeeSF                       uint64         `json:"origination_fee_sf"`
	FlashLoanFeeSF                         uint64         `json:"flash_loan_fee_sf"`
	BorrowRateCurve                        [11]CurvePoint `json:"borrow_rate_curve"`
	DepositWithdrawalCap                   WithdrawalCap  `json:"deposit_withdrawal_cap"`
	DebtWithdrawalCap                      WithdrawalCap  `json:"debt_withdrawal_cap"`
}

type Diff struct {
	Changed       bool     `json:"changed"`
	ChangedFields []string `json:"changed_fields"`
}

func Decode(target Target, slot uint64, observedAt time.Time, account *chain.Account, slotDurationMS float64) (Snapshot, error) {
	reserve, err := klend.DecodeReserve(account)
	if err != nil {
		return Snapshot{}, err
	}
	market, mint := reserve.LendingMarket.String(), reserve.LiquidityMint.String()
	if target.Market != nil && *target.Market != market {
		return Snapshot{}, fmt.Errorf("reserve %s market %s does not match target %s", target.Reserve, market, *target.Market)
	}
	if target.LiquidityMint != nil && *target.LiquidityMint != mint {
		return Snapshot{}, fmt.Errorf("reserve %s mint %s does not match target %s", target.Reserve, mint, *target.LiquidityMint)
	}
	available := float64(reserve.AvailableAmount)
	borrowedInt := klend.U128(reserve.BorrowedAmountSF)
	borrowed := scaledFraction(borrowedInt)
	price := scaledFraction(klend.U128(reserve.MarketPriceSF))
	protocolFees := scaledFraction(klend.U128(reserve.AccumulatedProtocolFeesSF))
	referrerFees := scaledFraction(klend.U128(reserve.AccumulatedReferrerFeesSF))
	pendingFees := scaledFraction(klend.U128(reserve.PendingReferrerFeesSF))
	totalSupply := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	utilization := 0.0
	if totalSupply > 0 {
		utilization = borrowed / totalSupply
	}
	var curve [11]CurvePoint
	for index, point := range reserve.BorrowRateCurve {
		curve[index] = CurvePoint{point.UtilizationRateBPS, point.BorrowRateBPS}
	}
	curveAPR := borrowCurveAPR(curve, utilization) * (1000.0 / 2.0 / slotDurationMS)
	hostBPS := reserve.HostFixedInterestRateBPS
	hostAPR := float64(hostBPS) / 10000.0 * (1000.0 / 2.0 / slotDurationMS)
	borrowAPR := curveAPR + hostAPR
	supplyAPR := utilization * curveAPR * (1 - float64(reserve.ProtocolTakeRatePct)/100)
	borrowAPY := aprToAPY(borrowAPR, slotDurationMS)
	supplyAPY := aprToAPY(supplyAPR, slotDurationMS)
	name := strings.TrimRight(string(reserve.Name[:]), "\x00")
	symbol := target.Symbol
	if symbol == nil && strings.TrimSpace(name) != "" {
		value := strings.TrimSpace(name)
		symbol = &value
	}
	if symbol == nil {
		if value := mintSymbol(mint); value != "" {
			symbol = &value
		}
	}
	marketValue := market
	mintFactor := math.Pow10(int(reserve.MintDecimals))
	withdrawalCap := func(c klend.WithdrawalCap) WithdrawalCap {
		return WithdrawalCap{c.ConfigCapacity, c.CurrentTotal, c.LastIntervalStartTimestamp, c.IntervalLengthSeconds}
	}
	snapshot := Snapshot{ObservationSchemaVersion: 2, ObservedAt: observedAt, Slot: slot, Reserve: target.Reserve, Market: &marketValue, Symbol: symbol, LiquidityMint: mint, MintDecimals: reserve.MintDecimals,
		ReserveLastUpdateSlot: reserve.LastUpdateSlot, ReserveLastUpdateStale: reserve.LastUpdateStale, ReservePriceStatus: reserve.PriceStatus, AvailableAmount: available, BorrowedAmount: borrowed, BorrowedAmountSF: borrowedInt.String(), TotalSupplyAmount: totalSupply, MarketPriceUSD: price, MarketPriceLastUpdatedTS: reserve.MarketPriceLastUpdatedTS,
		TotalSupplyUSDEstimate: totalSupply * price / mintFactor, TotalBorrowUSDEstimate: borrowed * price / mintFactor, Utilization: utilization, BorrowAPR: borrowAPR, SupplyAPR: supplyAPR, BorrowAPY: borrowAPY, SupplyAPY: supplyAPY,
		ProtocolTakeRatePct: reserve.ProtocolTakeRatePct, HostFixedInterestRateBPS: hostBPS, ReserveStatus: reserve.Status, EmergencyMode: reserve.EmergencyMode, LoanToValuePct: reserve.LoanToValuePct, LiquidationThresholdPct: reserve.LiquidationThresholdPct, BorrowFactorPct: reserve.BorrowFactorPct, DepositLimit: reserve.DepositLimit, BorrowLimit: reserve.BorrowLimit,
		UtilizationLimitBlockBorrowingAbovePct: reserve.UtilizationLimitBlockBorrowingAbovePct, DisableUsageAsCollOutsideEmode: reserve.DisableUsageAsCollOutsideEmode, BorrowLimitOutsideElevationGroup: reserve.BorrowLimitOutsideElevationGroup, BorrowedAmountOutsideElevationGroup: reserve.BorrowedAmountOutsideElevationGroup, OriginationFeeSF: reserve.BorrowFeeSF, FlashLoanFeeSF: reserve.FlashLoanFeeSF, BorrowRateCurve: curve,
		DepositWithdrawalCap: withdrawalCap(reserve.DepositWithdrawalCap), DebtWithdrawalCap: withdrawalCap(reserve.DebtWithdrawalCap)}
	for index := range 4 {
		snapshot.CumulativeBorrowRateBSF[index] = binary.LittleEndian.Uint64(reserve.CumulativeBorrowRateBSF[index*8:])
	}
	return snapshot, nil
}

// Compare reports the changed fields. "Nothing changed" is an empty list, as
// Rust's ReserveDiff.changed_fields Vec is: reserve_updates.changed_fields is
// TEXT[] NOT NULL and the diff JSON carries [], never null.
func Compare(previous, current Snapshot) Diff {
	fields := []string{}
	checks := []struct {
		name    string
		changed bool
	}{{"reserve_last_update_slot", previous.ReserveLastUpdateSlot != current.ReserveLastUpdateSlot}, {"reserve_last_update_stale", previous.ReserveLastUpdateStale != current.ReserveLastUpdateStale}, {"reserve_price_status", previous.ReservePriceStatus != current.ReservePriceStatus}, {"available_amount", previous.AvailableAmount != current.AvailableAmount}, {"borrowed_amount", previous.BorrowedAmountSF != current.BorrowedAmountSF}, {"total_supply_amount", previous.TotalSupplyAmount != current.TotalSupplyAmount}, {"market_price_usd", previous.MarketPriceUSD != current.MarketPriceUSD}, {"market_price_last_updated_ts", previous.MarketPriceLastUpdatedTS != current.MarketPriceLastUpdatedTS}, {"cumulative_borrow_rate_bsf", previous.CumulativeBorrowRateBSF != current.CumulativeBorrowRateBSF}, {"utilization", previous.Utilization != current.Utilization}, {"borrow_apy", previous.BorrowAPY != current.BorrowAPY}, {"supply_apy", previous.SupplyAPY != current.SupplyAPY}, {"total_supply_usd_estimate", previous.TotalSupplyUSDEstimate != current.TotalSupplyUSDEstimate}, {"total_borrow_usd_estimate", previous.TotalBorrowUSDEstimate != current.TotalBorrowUSDEstimate}}
	for _, check := range checks {
		if check.changed {
			fields = append(fields, check.name)
		}
	}
	return Diff{Changed: len(fields) > 0, ChangedFields: fields}
}

func scaledFraction(value *big.Int) float64 {
	result, _ := new(big.Rat).SetFrac(value, new(big.Int).Lsh(big.NewInt(1), fractionBits)).Float64()
	return result
}
func borrowCurveAPR(points [11]CurvePoint, utilization float64) float64 {
	values := append([]CurvePoint(nil), points[:]...)
	sort.Slice(values, func(i, j int) bool { return values[i].UtilizationRateBPS < values[j].UtilizationRateBPS })
	first := values[0]
	if utilization <= float64(first.UtilizationRateBPS)/10000 {
		return float64(first.BorrowRateBPS) / 10000
	}
	for index := 1; index < len(values); index++ {
		floor, ceil := values[index-1], values[index]
		floorU, ceilU := float64(floor.UtilizationRateBPS)/10000, float64(ceil.UtilizationRateBPS)/10000
		if utilization <= ceilU {
			if ceilU <= floorU {
				return float64(ceil.BorrowRateBPS) / 10000
			}
			t := (utilization - floorU) / (ceilU - floorU)
			return (float64(floor.BorrowRateBPS) + float64(int64(ceil.BorrowRateBPS)-int64(floor.BorrowRateBPS))*t) / 10000
		}
	}
	return float64(values[len(values)-1].BorrowRateBPS) / 10000
}
func aprToAPY(apr, slotDurationMS float64) float64 {
	if apr <= 0 {
		return 0
	}
	periods := secondsPerYear * 1000 / slotDurationMS
	return math.Pow(1+apr/periods, periods) - 1
}
func mintSymbol(mint string) string {
	return map[string]string{"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA": "USDS", "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH": "USDG", "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT": "USDE", "Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN": "SUSDE"}[mint]
}
