package kamino

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
)

const (
	reserveStructSize     = 8616
	liquidityOffset       = 120
	configOffset          = 4848
	borrowedOutsideOffset = 6696
	fractionBits          = 60
	secondsPerYear        = 365.25 * 24 * 60 * 60
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

func Decode(target Target, slot uint64, observedAt time.Time, data []byte, slotDurationMS float64) (Snapshot, error) {
	discriminator := sha256.Sum256([]byte("account:Reserve"))
	if len(data) != 8+reserveStructSize {
		return Snapshot{}, fmt.Errorf("reserve data is %d bytes, expected %d", len(data), 8+reserveStructSize)
	}
	if string(data[:8]) != string(discriminator[:8]) {
		return Snapshot{}, fmt.Errorf("reserve discriminator mismatch")
	}
	body := data[8:]
	market := key(body[24:56])
	liquidity := body[liquidityOffset : liquidityOffset+1232]
	mint := key(liquidity[:32])
	if target.Market != nil && *target.Market != market {
		return Snapshot{}, fmt.Errorf("reserve %s market %s does not match target %s", target.Reserve, market, *target.Market)
	}
	if target.LiquidityMint != nil && *target.LiquidityMint != mint {
		return Snapshot{}, fmt.Errorf("reserve %s mint %s does not match target %s", target.Reserve, mint, *target.LiquidityMint)
	}
	available := float64(u64(liquidity, 96))
	borrowedInt := u128(liquidity[104:120])
	borrowed := scaledFraction(borrowedInt)
	price := scaledFraction(u128(liquidity[120:136]))
	protocolFees := scaledFraction(u128(liquidity[216:232]))
	referrerFees := scaledFraction(u128(liquidity[232:248]))
	pendingFees := scaledFraction(u128(liquidity[248:264]))
	totalSupply := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	utilization := 0.0
	if totalSupply > 0 {
		utilization = borrowed / totalSupply
	}
	config := body[configOffset : configOffset+952]
	curve := curvePoints(config[64:152])
	curveAPR := borrowCurveAPR(curve, utilization) * (1000.0 / 2.0 / slotDurationMS)
	hostBPS := binary.LittleEndian.Uint16(config[2:4])
	hostAPR := float64(hostBPS) / 10000.0 * (1000.0 / 2.0 / slotDurationMS)
	borrowAPR := curveAPR + hostAPR
	supplyAPR := utilization * curveAPR * (1 - float64(config[14])/100)
	borrowAPY := aprToAPY(borrowAPR, slotDurationMS)
	supplyAPY := aprToAPY(supplyAPR, slotDurationMS)
	name := strings.TrimRight(string(config[176:208]), "\x00")
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
	mintDecimals := u64(liquidity, 144)
	mintFactor := math.Pow10(int(mintDecimals))
	snapshot := Snapshot{ObservationSchemaVersion: 2, ObservedAt: observedAt, Slot: slot, Reserve: target.Reserve, Market: &marketValue, Symbol: symbol, LiquidityMint: mint, MintDecimals: mintDecimals,
		ReserveLastUpdateSlot: u64(body, 8), ReserveLastUpdateStale: body[16] != 0, ReservePriceStatus: body[17], AvailableAmount: available, BorrowedAmount: borrowed, BorrowedAmountSF: borrowedInt.String(), TotalSupplyAmount: totalSupply, MarketPriceUSD: price, MarketPriceLastUpdatedTS: u64(liquidity, 136),
		TotalSupplyUSDEstimate: totalSupply * price / mintFactor, TotalBorrowUSDEstimate: borrowed * price / mintFactor, Utilization: utilization, BorrowAPR: borrowAPR, SupplyAPR: supplyAPR, BorrowAPY: borrowAPY, SupplyAPY: supplyAPY,
		ProtocolTakeRatePct: config[14], HostFixedInterestRateBPS: hostBPS, ReserveStatus: config[0], EmergencyMode: config[8] != 0, LoanToValuePct: config[16], LiquidationThresholdPct: config[17], BorrowFactorPct: u64(config, 152), DepositLimit: u64(config, 160), BorrowLimit: u64(config, 168),
		UtilizationLimitBlockBorrowingAbovePct: config[645], DisableUsageAsCollOutsideEmode: config[644] != 0, BorrowLimitOutsideElevationGroup: u64(config, 648), BorrowedAmountOutsideElevationGroup: u64(body, borrowedOutsideOffset), OriginationFeeSF: u64(config, 40), FlashLoanFeeSF: u64(config, 48), BorrowRateCurve: curve,
		DepositWithdrawalCap: withdrawalCap(config[560:592]), DebtWithdrawalCap: withdrawalCap(config[592:624])}
	for index := range 4 {
		snapshot.CumulativeBorrowRateBSF[index] = u64(liquidity, 168+index*8)
	}
	return snapshot, nil
}

func Compare(previous, current Snapshot) Diff {
	var fields []string
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

func u64(data []byte, offset int) uint64 { return binary.LittleEndian.Uint64(data[offset : offset+8]) }
func u128(data []byte) *big.Int {
	reversed := make([]byte, len(data))
	for index := range data {
		reversed[len(data)-1-index] = data[index]
	}
	return new(big.Int).SetBytes(reversed)
}
func scaledFraction(value *big.Int) float64 {
	result, _ := new(big.Rat).SetFrac(value, new(big.Int).Lsh(big.NewInt(1), fractionBits)).Float64()
	return result
}
func key(data []byte) string {
	var result solana.PublicKey
	copy(result[:], data)
	return result.String()
}
func curvePoints(data []byte) (points [11]CurvePoint) {
	for index := range points {
		offset := index * 8
		points[index] = CurvePoint{binary.LittleEndian.Uint32(data[offset : offset+4]), binary.LittleEndian.Uint32(data[offset+4 : offset+8])}
	}
	return
}
func withdrawalCap(data []byte) WithdrawalCap {
	return WithdrawalCap{int64(u64(data, 0)), int64(u64(data, 8)), u64(data, 16), u64(data, 24)}
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
