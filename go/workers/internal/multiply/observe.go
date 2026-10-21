package multiply

import (
	"context"
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

type ObservationReader interface {
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
	SlotsPerSecond(ctx context.Context) (float64, error)
}

// MarketValueSF is a market value as a checked u128 scaled fraction; the zero value is unknown.
type MarketValueSF struct{ value *big.Int }

func knownMarketValue(value *big.Int) MarketValueSF { return MarketValueSF{value} }

func (v MarketValueSF) Int() *big.Int { return v.value }

type StrategyObservation struct {
	StrategyKey StrategyKey

	ObligationLastUpdateSlot        uint64
	CollateralReserveLastUpdateSlot uint64
	DebtReserveLastUpdateSlot       uint64
	CollateralDepositedRaw          uint64
	DebtRaw                         uint64
	DebtAmountSF                    string
	CollateralValueSF               MarketValueSF
	DebtValueSF                     *big.Int
	UnhealthyValueSF                *big.Int
	DebtMarketPriceSF               *big.Int
	DebtMintFactor                  uint64
	CollateralTotalSupplyRaw        uint64
	CollateralTotalLiquiditySF      *big.Int
	CollateralSupplyAPYBPS          uint64
	DebtBorrowAPYBPS                uint64
}

type CustodyBalance struct {
	StrategyKey StrategyKey
	Balance     TokenBalance
}

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

func (o *ObservedRoute) balances() []*TokenBalance {
	all := []*TokenBalance{&o.Claim}
	for index := range o.CollateralCustodies {
		all = append(all, &o.CollateralCustodies[index].Balance)
	}
	for index := range o.DebtCustodies {
		all = append(all, &o.DebtCustodies[index].Balance)
	}
	for index := range o.ExternalCustody {
		all = append(all, &o.ExternalCustody[index])
	}
	return all
}

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

func ObserveConfirmed(ctx context.Context, reader ObservationReader, topology *EarnMaxTopology, extra []TokenBalance) (*ObservedRoute, error) {
	if reader == nil || topology == nil {
		return nil, errors.New("confirmed observation dependencies are absent")
	}
	keys := []solana.PublicKey{topology.ClaimCustody}
	for _, config := range topology.StrategyCatalog() {
		keys = append(keys, config.CollateralCustody, config.Obligation, config.CollateralReserve, config.DebtCustody, config.DebtReserve)
	}
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
		collateralAPY, err := reserveAPYBPS(ctx, reader, collateralReserve, true)
		if err != nil {
			return nil, err
		}
		debtAPY, err := reserveAPYBPS(ctx, reader, debtReserve, false)
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
		debtSF = borrow.BorrowedAmountSF.Int()
	}
	if deposits > 1 || borrows > 1 {
		return nil, errors.New("obligation reserve topology drifted")
	}
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
	unhealthy := obligation.UnhealthyBorrowValueSF.Int()
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
		CollateralValueSF:               knownMarketValue(new(big.Int)),
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
	price := reserve.MarketPriceSF.Int()
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

const fractionOneSF = uint64(1) << kamino.FractionBits

func collateralMarketValueSF(reserve *decodedReserve, collateralRaw uint64) (MarketValueSF, error) {
	if collateralRaw == 0 {
		return knownMarketValue(new(big.Int)), nil
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(reserve.CollateralMintSupply), kamino.FractionBits)
	if denominator.Sign() == 0 {
		return MarketValueSF{}, errors.New("collateral reserve supply is zero")
	}
	liquidityRaw := new(big.Int).Mul(new(big.Int).SetUint64(collateralRaw), reserve.TotalLiquiditySF)
	liquidityRaw.Div(liquidityRaw, denominator)
	mintFactor, err := mintFactor(reserve.Decimals)
	if err != nil {
		return MarketValueSF{}, err
	}
	value := new(big.Int).Mul(liquidityRaw, reserve.MarketPriceSF)
	value.Div(value, new(big.Int).SetUint64(mintFactor))
	checked, err := checkedU128(value)
	if err != nil {
		return MarketValueSF{}, err
	}
	return knownMarketValue(checked), nil
}

func debtMarketValueSF(reserve *decodedReserve, debtSF *big.Int) (*big.Int, error) {
	if debtSF == nil || debtSF.Sign() == 0 {
		return new(big.Int), nil
	}
	mintFactor, err := mintFactor(reserve.Decimals)
	if err != nil {
		return nil, err
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(mintFactor), kamino.FractionBits)
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
	for _, value := range []*big.Int{position.CollateralValueSF.Int(), position.DebtValueSF, position.DebtMarketPriceSF} {
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
	result := new(big.Int).Rsh(value, kamino.FractionBits)
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

func reserveAPYBPS(ctx context.Context, reader ObservationReader, decoded *decodedReserve, supply bool) (uint64, error) {
	var slotsPerSecond float64
	var rateErr error
	if decoded.reserve.NeedsSlotRate() {
		slotsPerSecond, rateErr = reader.SlotsPerSecond(ctx)
	}
	rates, err := decoded.reserve.Rates(slotsPerSecond)
	if errors.Is(err, kamino.ErrSlotRateUnavailable) && rateErr != nil {
		return 0, fmt.Errorf("%w: %w", err, rateErr)
	}
	if err != nil {
		return 0, err
	}
	apy := rates.BorrowAPY
	if supply {
		apy = rates.SupplyAPY
	}
	bps := math.Round(apy * 10_000)
	if !(bps >= 0 && bps < math.Ldexp(1, 64)) {
		return 0, errors.New("reserve APY is outside the supported range")
	}
	return uint64(bps), nil
}

func CollateralToLiquidityRaw(position *StrategyObservation, collateralRaw uint64) (uint64, error) {
	if position == nil {
		return kamino.CollateralToLiquidity(nil, 0, collateralRaw)
	}
	return kamino.CollateralToLiquidity(position.CollateralTotalLiquiditySF, position.CollateralTotalSupplyRaw, collateralRaw)
}

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
