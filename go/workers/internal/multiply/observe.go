package multiply

// Confirmed route observation, ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/observe.rs with the KLend layouts
// shared by the reviewed backyard and fleet decoders in this module tree.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// ObservationReader is the consumer-defined chain read surface; *chain.Client
// is the production binding and tests bind fixtures. Missing accounts are nil
// entries in the same order.
type ObservationReader interface {
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
}

// StrategyObservation is one decoded obligation/reserve pair.
type StrategyObservation struct {
	StrategyKey StrategyKey

	ObligationLastUpdateSlot        uint64
	CollateralReserveLastUpdateSlot uint64
	DebtReserveLastUpdateSlot       uint64
	CollateralDepositedRaw          uint64
	DebtRaw                         uint64
	DebtAmountSF                    string
	CollateralValueSF               *big.Int // checked u128; nil is unknown
	DebtValueSF                     *big.Int
	UnhealthyValueSF                *big.Int
	DebtMarketPriceSF               *big.Int
	DebtMintFactor                  uint64
	CollateralTotalSupplyRaw        uint64
	CollateralTotalLiquiditySF      *big.Int
	CollateralSupplyAPYBPS          uint64
	DebtBorrowAPYBPS                uint64
}

// CustodyBalance pairs a strategy with its custody balance.
type CustodyBalance struct {
	StrategyKey StrategyKey
	Balance     TokenBalance
}

// ObservedRoute is the complete confirmed route snapshot.
type ObservedRoute struct {
	Slot                uint64
	Claim               TokenBalance
	CollateralCustodies []CustodyBalance
	DebtCustodies       []CustodyBalance
	Strategies          []*StrategyObservation
	ExternalCustody     []TokenBalance
}

func (o *ObservedRoute) Position(key StrategyKey) *StrategyObservation {
	if o == nil {
		return nil
	}
	for _, value := range o.Strategies {
		if value != nil && value.StrategyKey == key {
			return value
		}
	}
	return nil
}

func (o *ObservedRoute) DebtCustody(key StrategyKey) *TokenBalance {
	for index := range o.DebtCustodies {
		if o.DebtCustodies[index].StrategyKey == key {
			return &o.DebtCustodies[index].Balance
		}
	}
	return nil
}

func (o *ObservedRoute) CollateralCustody(key StrategyKey) *TokenBalance {
	for index := range o.CollateralCustodies {
		if o.CollateralCustodies[index].StrategyKey == key {
			return &o.CollateralCustodies[index].Balance
		}
	}
	return nil
}

// ActiveStrategyIsCoherent mirrors active_strategy_is_coherent.
func (o *ObservedRoute) ActiveStrategyIsCoherent() bool {
	if o == nil {
		return false
	}
	var active *StrategyObservation
	for _, position := range o.Strategies {
		if position == nil {
			return false
		}
		if position.CollateralDepositedRaw > 0 || position.DebtRaw > 0 {
			if active != nil {
				return false
			}
			active = position
		}
	}
	if active == nil {
		return true
	}
	return active.CollateralReserveLastUpdateSlot >= active.ObligationLastUpdateSlot &&
		active.DebtReserveLastUpdateSlot >= active.ObligationLastUpdateSlot
}

// ObserveConfirmed observes the whole route plus optional external custody.
func ObserveConfirmed(ctx context.Context, reader ObservationReader, topology *EarnMaxTopology, extra []TokenBalance) (*ObservedRoute, error) {
	if reader == nil || topology == nil {
		return nil, errors.New("confirmed observation dependencies are absent")
	}
	keys := []solana.PublicKey{topology.ClaimCustody}
	for _, config := range topology.StrategyCatalog() {
		keys = append(keys, config.CollateralCustody, config.Obligation, config.CollateralReserve, config.DebtCustody, config.DebtReserve)
	}
	// Receipt destinations participate in the same bank snapshot as vault
	// custody and obligations. A second slotless read can combine incompatible
	// before/after states and cannot support financial reconciliation.
	for _, custody := range extra {
		key, err := solana.PublicKeyFromBase58(custody.Account)
		if err != nil {
			return nil, errors.New("external custody account is invalid")
		}
		keys = append(keys, key)
	}
	unique := dedupKeys(keys)
	slot, accounts, err := reader.Accounts(ctx, unique, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	if len(accounts) != len(unique) || slot == 0 {
		return nil, errors.New("confirmed account response omitted its slot or account entries")
	}
	for index, account := range accounts {
		if account != nil && account.Key != unique[index] {
			return nil, errors.New("confirmed account response identity drifted")
		}
	}
	required := func(key solana.PublicKey) (*chain.Account, error) {
		for position, candidate := range unique {
			if candidate == key {
				if account := accounts[position]; account != nil {
					return account, nil
				}
			}
		}
		return nil, fmt.Errorf("required mainnet account %s is absent", key)
	}
	optional := func(key solana.PublicKey) *chain.Account {
		for position, candidate := range unique {
			if candidate == key {
				return accounts[position]
			}
		}
		return nil
	}
	claimAccount, err := required(topology.ClaimCustody)
	if err != nil {
		return nil, err
	}
	claim, err := tokenBalance(claimAccount, USDCMint, solana.TokenProgramID, &topology.Vault)
	if err != nil {
		return nil, err
	}
	observed := &ObservedRoute{
		Slot: slot,
		Claim: TokenBalance{
			Account: topology.ClaimCustody.String(), Mint: USDCMint,
			TokenProgram: TokenProgram, AmountRaw: claim,
		},
	}
	for _, config := range topology.StrategyCatalog() {
		collateralReserveAccount, err := required(config.CollateralReserve)
		if err != nil {
			return nil, err
		}
		debtReserveAccount, err := required(config.DebtReserve)
		if err != nil {
			return nil, err
		}
		collateralReserve, err := decodeReserve(collateralReserveAccount, config)
		if err != nil {
			return nil, err
		}
		debtReserve, err := decodeReserve(debtReserveAccount, config)
		if err != nil {
			return nil, err
		}
		collateralAPY, err := reserveAPYBPS(collateralReserve, true)
		if err != nil {
			return nil, err
		}
		debtAPY, err := reserveAPYBPS(debtReserve, false)
		if err != nil {
			return nil, err
		}
		var observation *StrategyObservation
		if obligationAccount := optional(config.Obligation); obligationAccount != nil {
			observation, err = decodeObligation(obligationAccount, config, topology.Vault, collateralReserve, debtReserve, collateralAPY, debtAPY)
		} else {
			observation, err = emptyObligation(config, collateralReserve, debtReserve, collateralAPY, debtAPY)
		}
		if err != nil {
			return nil, err
		}
		if observation.ObligationLastUpdateSlot > slot || observation.CollateralReserveLastUpdateSlot > slot || observation.DebtReserveLastUpdateSlot > slot {
			return nil, errors.New("account update is newer than the confirmed bank snapshot")
		}
		collateralAmount := uint64(0)
		if account := optional(config.CollateralCustody); account != nil {
			collateralAmount, err = tokenBalance(account, config.CollateralMint, solana.TokenProgramID, &topology.Vault)
			if err != nil {
				return nil, err
			}
		}
		debtAmount := uint64(0)
		if account := optional(config.DebtCustody); account != nil {
			debtAmount, err = tokenBalance(account, config.DebtMint, config.DebtTokenProgram, &topology.Vault)
			if err != nil {
				return nil, err
			}
		}
		observed.CollateralCustodies = append(observed.CollateralCustodies, CustodyBalance{
			StrategyKey: config.Key,
			Balance: TokenBalance{
				Account: config.CollateralCustody.String(), Mint: config.CollateralMint,
				TokenProgram: TokenProgram, AmountRaw: collateralAmount,
			},
		})
		observed.DebtCustodies = append(observed.DebtCustodies, CustodyBalance{
			StrategyKey: config.Key,
			Balance: TokenBalance{
				Account: config.DebtCustody.String(), Mint: config.DebtMint,
				TokenProgram: config.DebtTokenProgram.String(), AmountRaw: debtAmount,
			},
		})
		observed.Strategies = append(observed.Strategies, observation)
	}
	for _, custody := range extra {
		program, err := solana.PublicKeyFromBase58(custody.TokenProgram)
		if err != nil {
			return nil, errors.New("external custody token program is invalid")
		}
		key, err := solana.PublicKeyFromBase58(custody.Account)
		if err != nil {
			return nil, errors.New("external custody account is invalid")
		}
		if program != solana.TokenProgramID && program != solana.Token2022ProgramID {
			return nil, errors.New("external custody token program is unsupported")
		}
		account, err := required(key)
		if err != nil {
			return nil, err
		}
		amount, err := tokenBalance(account, custody.Mint, program, nil)
		if err != nil {
			return nil, err
		}
		observed.ExternalCustody = append(observed.ExternalCustody, TokenBalance{
			Account: custody.Account, Mint: custody.Mint,
			TokenProgram: custody.TokenProgram, AmountRaw: amount,
		})
	}
	return observed, nil
}

func dedupKeys(keys []solana.PublicKey) []solana.PublicKey {
	sorted := append([]solana.PublicKey(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool {
		return lessKey(sorted[i], sorted[j])
	})
	unique := sorted[:0]
	var previous *solana.PublicKey
	for _, key := range sorted {
		if previous != nil && *previous == key {
			continue
		}
		unique = append(unique, key)
		previous = &unique[len(unique)-1]
	}
	return unique
}

func lessKey(left, right solana.PublicKey) bool {
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

// tokenBalance is the raw amount of a mint's account under tokenProgram, held
// by owner when owner is set.
func tokenBalance(account *chain.Account, mint string, tokenProgram solana.PublicKey, owner *solana.PublicKey) (uint64, error) {
	held, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return 0, err
	}
	if held.Program != tokenProgram || held.Mint.String() != mint || owner != nil && held.Owner != *owner {
		return 0, errors.New("custody token program, mint or authority drifted")
	}
	return held.Amount, nil
}

func decodeObligation(account *chain.Account, config StrategyConfig, vault solana.PublicKey, collateralReserve, debtReserve *decodedReserve, collateralAPY, debtAPY uint64) (*StrategyObservation, error) {
	obligation, err := kamino.DecodeObligation(account)
	if err != nil || account.Key != config.Obligation {
		return nil, errors.New("KLend account envelope or layout drifted")
	}
	if obligation.LendingMarket != config.Market || obligation.Owner != vault {
		return nil, errors.New("obligation identity drifted")
	}
	deposits, borrows := 0, 0
	var collateralDepositedRaw uint64
	var debtSF *big.Int
	for _, deposit := range obligation.Deposits {
		if deposit.Reserve.IsZero() {
			continue
		}
		if deposit.Reserve != config.CollateralReserve {
			return nil, errors.New("obligation reserve topology drifted")
		}
		deposits++
		collateralDepositedRaw = deposit.DepositedAmount
	}
	for _, borrow := range obligation.Borrows {
		if borrow.Reserve.IsZero() {
			continue
		}
		if borrow.Reserve != config.DebtReserve {
			return nil, errors.New("obligation reserve topology drifted")
		}
		borrows++
		debtSF = kamino.U128(borrow.BorrowedAmountSF)
	}
	if deposits > 1 || borrows > 1 {
		return nil, errors.New("obligation reserve topology drifted")
	}
	// ceil(debt_sf / 2^60), bounded to u64 like the Rust checked conversion.
	debtRaw := uint64(0)
	if debtSF != nil {
		ceil := ceilDiv60(debtSF)
		if !ceil.IsUint64() {
			return nil, errors.New("obligation debt exceeds u64")
		}
		debtRaw = ceil.Uint64()
	}
	if obligation.ElevationGroup != 0 {
		return nil, errors.New("obligation identity drifted")
	}
	unhealthy := kamino.U128(obligation.UnhealthyBorrowValueSF)
	collateralValue, err := collateralMarketValueSF(collateralReserve, collateralDepositedRaw)
	if err != nil {
		return nil, err
	}
	debtValue, err := debtMarketValueSF(debtReserve, debtSF)
	if err != nil {
		return nil, err
	}
	debtMintFactor, err := mintFactor(debtReserve.Decimals)
	if err != nil {
		return nil, err
	}
	sfString := "0"
	if debtSF != nil {
		sfString = debtSF.String()
	}
	return &StrategyObservation{
		StrategyKey:                     config.Key,
		ObligationLastUpdateSlot:        obligation.LastUpdateSlot,
		CollateralReserveLastUpdateSlot: collateralReserve.LastUpdateSlot,
		DebtReserveLastUpdateSlot:       debtReserve.LastUpdateSlot,
		CollateralDepositedRaw:          collateralDepositedRaw,
		DebtRaw:                         debtRaw,
		DebtAmountSF:                    sfString,
		CollateralValueSF:               collateralValue,
		DebtValueSF:                     debtValue,
		UnhealthyValueSF:                unhealthy,
		DebtMarketPriceSF:               new(big.Int).Set(debtReserve.MarketPriceSF),
		DebtMintFactor:                  debtMintFactor,
		CollateralTotalSupplyRaw:        collateralReserve.CollateralMintSupply,
		CollateralTotalLiquiditySF:      collateralReserve.TotalLiquiditySF,
		CollateralSupplyAPYBPS:          collateralAPY,
		DebtBorrowAPYBPS:                debtAPY,
	}, nil
}

func emptyObligation(config StrategyConfig, collateralReserve, debtReserve *decodedReserve, collateralAPY, debtAPY uint64) (*StrategyObservation, error) {
	debtMintFactor, err := mintFactor(debtReserve.Decimals)
	if err != nil {
		return nil, err
	}
	return &StrategyObservation{
		StrategyKey:                     config.Key,
		ObligationLastUpdateSlot:        0,
		DebtAmountSF:                    "0",
		CollateralValueSF:               new(big.Int),
		DebtValueSF:                     new(big.Int),
		CollateralReserveLastUpdateSlot: collateralReserve.LastUpdateSlot,
		DebtReserveLastUpdateSlot:       debtReserve.LastUpdateSlot,
		DebtMintFactor:                  debtMintFactor,
		UnhealthyValueSF:                new(big.Int),
		CollateralTotalSupplyRaw:        collateralReserve.CollateralMintSupply,
		CollateralTotalLiquiditySF:      collateralReserve.TotalLiquiditySF,
		CollateralSupplyAPYBPS:          collateralAPY,
		DebtBorrowAPYBPS:                debtAPY,
		DebtMarketPriceSF:               new(big.Int).Set(debtReserve.MarketPriceSF),
	}, nil
}

type decodedReserve struct {
	LastUpdateSlot       uint64
	Status               byte
	MarketPriceSF        *big.Int
	Decimals             uint64
	CollateralMintSupply uint64
	TotalLiquiditySF     *big.Int
	reserve              kamino.Reserve
}

func decodeReserve(account *chain.Account, config StrategyConfig) (*decodedReserve, error) {
	address := config.CollateralReserve
	mint := config.CollateralMint
	if account != nil && account.Key != address && config.DebtReserve == account.Key {
		address = config.DebtReserve
		mint = config.DebtMint
	}
	reserve, err := kamino.DecodeReserve(account)
	if err != nil || account.Key != address {
		return nil, errors.New("KLend account envelope or layout drifted")
	}
	price := kamino.U128(reserve.MarketPriceSF)
	if reserve.LendingMarket != config.Market || reserve.LiquidityMint != mustKey(mint) || reserve.Status != 0 || price.Sign() == 0 {
		return nil, errors.New("reserve identity, status, or price drifted")
	}
	totalLiquidity, err := reserve.TotalLiquiditySF()
	if err != nil {
		return nil, err
	}
	if totalLiquidity.Sign() == 0 {
		return nil, errors.New("reserve total liquidity is zero")
	}
	return &decodedReserve{
		LastUpdateSlot:       reserve.LastUpdateSlot,
		Status:               reserve.Status,
		MarketPriceSF:        price,
		Decimals:             reserve.MintDecimals,
		CollateralMintSupply: reserve.CollateralMintTotalSupply,
		TotalLiquiditySF:     totalLiquidity,
		reserve:              reserve,
	}, nil
}

const fractionOneSF = uint64(1) << 60

func collateralMarketValueSF(reserve *decodedReserve, collateralRaw uint64) (*big.Int, error) {
	if collateralRaw == 0 {
		return new(big.Int), nil
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(reserve.CollateralMintSupply), 60)
	if denominator.Sign() == 0 {
		return nil, errors.New("collateral reserve supply is zero")
	}
	liquidityRaw := new(big.Int).Mul(new(big.Int).SetUint64(collateralRaw), reserve.TotalLiquiditySF)
	liquidityRaw.Div(liquidityRaw, denominator)
	mintFactor, err := mintFactor(reserve.Decimals)
	if err != nil {
		return nil, err
	}
	value := new(big.Int).Mul(liquidityRaw, reserve.MarketPriceSF)
	value.Div(value, new(big.Int).SetUint64(mintFactor))
	return checkedU128(value)
}

func debtMarketValueSF(reserve *decodedReserve, debtSF *big.Int) (*big.Int, error) {
	if debtSF == nil || debtSF.Sign() == 0 {
		return new(big.Int), nil
	}
	mintFactor, err := mintFactor(reserve.Decimals)
	if err != nil {
		return nil, err
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(mintFactor), 60)
	value := new(big.Int).Mul(debtSF, reserve.MarketPriceSF)
	value.Div(value, denominator)
	return checkedU128(value)
}

func checkedU128(value *big.Int) (*big.Int, error) {
	if value == nil || value.Sign() < 0 || value.BitLen() > 128 {
		return nil, errors.New("scaled fraction is unknown or exceeds u128")
	}
	return new(big.Int).Set(value), nil
}

// saturatingU128 preserves the Rust planner's u128 saturating operations;
// intermediates remain exact and never alias observed values.
func saturatingU128(value *big.Int) *big.Int {
	if value.Sign() < 0 {
		return new(big.Int)
	}
	if value.BitLen() > 128 {
		return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	}
	return value
}

func validMarketValues(position *StrategyObservation) bool {
	if position == nil {
		return false
	}
	for _, value := range []*big.Int{position.CollateralValueSF, position.DebtValueSF, position.DebtMarketPriceSF} {
		if value == nil || value.Sign() < 0 || value.BitLen() > 128 {
			return false
		}
	}
	return true
}

func mintFactor(decimals uint64) (uint64, error) {
	if decimals > 18 {
		return 0, errors.New("mint factor overflow")
	}
	factor := uint64(1)
	for index := uint64(0); index < decimals; index++ {
		next := factor * 10
		if next/10 != factor {
			return 0, errors.New("mint factor overflow")
		}
		factor = next
	}
	return factor, nil
}

func ceilDiv60(value *big.Int) *big.Int {
	result := new(big.Int).Rsh(value, 60)
	remainder := new(big.Int).And(value, new(big.Int).SetUint64(fractionOneSF-1))
	if remainder.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

// APY: 500ms slot clock, matching the codec call site in observe.rs.
const (
	slotsPerSecond = 2.0
	secondsPerYear = 365.25 * 24 * 60 * 60
	slotDurationMS = 500.0
)

// reserveAPYBPS ports the pinned loyal-kamino-codec curve calculation. Host
// fixed interest belongs to borrowing APR, not supplier income.
func reserveAPYBPS(decoded *decodedReserve, supply bool) (uint64, error) {
	reserve := decoded.reserve
	available := float64(reserve.AvailableAmount)
	borrowed := scaledFractionToFloat(reserve.BorrowedAmountSF)
	protocolFees := scaledFractionToFloat(reserve.AccumulatedProtocolFeesSF)
	referrerFees := scaledFractionToFloat(reserve.AccumulatedReferrerFeesSF)
	pendingFees := scaledFractionToFloat(reserve.PendingReferrerFeesSF)
	total := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	if total <= 0 || math.IsInf(total, 0) || math.IsNaN(total) {
		return 0, errors.New("reserve utilization is invalid")
	}
	utilization := borrowed / total
	if !finiteFloat(utilization) || utilization < 0 || utilization > 1.01 {
		return 0, errors.New("reserve utilization is invalid")
	}
	curveAPR := borrowCurveAPR(reserve.BorrowRateCurve, utilization) * (1000.0 / slotsPerSecond / slotDurationMS)
	ratio := 0.0
	if supply {
		takeRate := reserve.ProtocolTakeRatePct
		if takeRate > 100 {
			return 0, errors.New("reserve take rate is invalid")
		}
		ratio = utilization * curveAPR * (1 - float64(takeRate)/100)
	} else {
		ratio = curveAPR + float64(reserve.HostFixedInterestRateBPS)/10_000*(1000.0/slotsPerSecond/slotDurationMS)
	}
	if !finiteFloat(ratio) || ratio < 0 || ratio > float64(math.MaxUint64)/10_000.0 {
		return 0, errors.New("reserve APY is outside the supported range")
	}
	apy := 0.0
	if ratio > 0 {
		periods := secondsPerYear * 1000.0 / slotDurationMS
		apy = math.Pow(1+ratio/periods, periods) - 1
	}
	if !finiteFloat(apy) || apy < 0 || apy > float64(math.MaxUint64)/10_000.0 {
		return 0, errors.New("reserve APY is outside the supported range")
	}
	bps := math.Round(apy * 10_000)
	if bps >= math.Ldexp(1, 64) {
		return 0, errors.New("reserve APY bps exceeds u64")
	}
	return uint64(bps), nil
}

func borrowCurveAPR(curve [11]kamino.CurvePoint, utilization float64) float64 {
	type point struct{ utilization, rate float64 }
	points := make([]point, 0, len(curve))
	for _, value := range curve {
		points = append(points, point{utilization: float64(value.UtilizationRateBPS) / 10_000, rate: float64(value.BorrowRateBPS) / 10_000})
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].utilization < points[j].utilization })
	if len(points) == 0 {
		return 0
	}
	if utilization <= points[0].utilization {
		return points[0].rate
	}
	for index := 1; index < len(points); index++ {
		floor, ceiling := points[index-1], points[index]
		if utilization <= ceiling.utilization {
			width := ceiling.utilization - floor.utilization
			if width <= math.Nextafter(1, 2)-1 {
				return ceiling.rate
			}
			return floor.rate + (ceiling.rate-floor.rate)*(utilization-floor.utilization)/width
		}
	}
	return points[len(points)-1].rate
}

func scaledFractionToFloat(value [16]byte) float64 {
	high := binary.LittleEndian.Uint64(value[8:16])
	low := binary.LittleEndian.Uint64(value[:8])
	return float64(high)*16 + float64(low)/float64(fractionOneSF)
}

func finiteFloat(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// CollateralToLiquidityRaw mirrors collateral_to_liquidity_raw.
func CollateralToLiquidityRaw(position *StrategyObservation, collateralRaw uint64) (uint64, error) {
	if position == nil {
		return kamino.CollateralToLiquidity(nil, 0, collateralRaw)
	}
	return kamino.CollateralToLiquidity(position.CollateralTotalLiquiditySF, position.CollateralTotalSupplyRaw, collateralRaw)
}

// PositionBalance mirrors position_balance.
func PositionBalance(observation *ObservedRoute, key StrategyKey, topology *EarnMaxTopology) (MultiplyPosition, error) {
	config, err := topology.Strategy(key)
	if err != nil {
		return MultiplyPosition{}, err
	}
	position := observation.Position(key)
	if !validMarketValues(position) || position.UnhealthyValueSF == nil || position.UnhealthyValueSF.Sign() < 0 || position.UnhealthyValueSF.BitLen() > 128 {
		return MultiplyPosition{}, errors.New("position valuation is unknown or invalid")
	}
	health := uint64(math.MaxUint64)
	if position.DebtValueSF.Sign() != 0 {
		healthValue := saturatingU128(new(big.Int).Mul(position.UnhealthyValueSF, big.NewInt(1_000_000)))
		healthValue.Div(healthValue, position.DebtValueSF)
		if healthValue.IsUint64() {
			health = healthValue.Uint64()
		}
	}
	return NewActivePosition(
		key, config.Obligation.String(),
		TokenBalance{Account: config.CollateralCustody.String(), Mint: config.CollateralMint, TokenProgram: TokenProgram, AmountRaw: position.CollateralDepositedRaw},
		TokenBalance{Account: config.DebtCustody.String(), Mint: config.DebtMint, TokenProgram: config.DebtTokenProgram.String(), AmountRaw: position.DebtRaw},
		position.DebtAmountSF, health,
	), nil
}
