package multiply

// Confirmed route observation, ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/observe.rs with the KLend layouts
// shared by the reviewed backyard and fleet decoders in this module tree.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/solana-foundation/solana-go/v2"
)

// Account is a confirmed RPC account record.
type Account struct {
	Address    string
	Owner      string
	Lamports   uint64
	Executable bool
	Data       []byte
}

// ObservationReader is the consumer-defined chain read surface. Production
// binds the confirmed-commitment RPC client; tests bind fixtures. Missing
// accounts are returned as nil entries in the same order.
type ObservationReader interface {
	GetMultipleAccounts(ctx context.Context, keys []solana.PublicKey) (slot uint64, accounts []*Account, err error)
	GetAccount(ctx context.Context, key solana.PublicKey) (*Account, error)
}

// LiveObservationReader exposes only confirmed account reads. Root owns the
// bounded RPC transport; this adapter never receives a signing capability.
type LiveObservationReader struct {
	rpc *LiveRPCSurface
}

func NewLiveObservationReader(rpc *LiveRPCSurface) (*LiveObservationReader, error) {
	if rpc == nil || rpc.HTTP == nil || rpc.URL == "" {
		return nil, errors.New("multiply observation requires a configured RPC transport")
	}
	return &LiveObservationReader{rpc: rpc}, nil
}

func (r *LiveObservationReader) GetMultipleAccounts(ctx context.Context, keys []solana.PublicKey) (uint64, []*Account, error) {
	if len(keys) == 0 || len(keys) > 100 {
		return 0, nil, errors.New("confirmed account batch must contain 1 to 100 keys")
	}
	addresses := make([]string, len(keys))
	for index, key := range keys {
		addresses[index] = key.String()
	}
	var raw struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Owner      string `json:"owner"`
			Lamports   uint64 `json:"lamports"`
			Executable bool   `json:"executable"`
			Data       []any  `json:"data"`
		} `json:"value"`
	}
	if err := r.rpc.call(ctx, "getMultipleAccounts", []any{addresses, map[string]any{"encoding": "base64", "commitment": "confirmed"}}, &raw); err != nil {
		return 0, nil, err
	}
	if raw.Context.Slot == 0 || len(raw.Value) != len(keys) {
		return 0, nil, errors.New("confirmed account response omitted context or entries")
	}
	accounts := make([]*Account, len(keys))
	for index, value := range raw.Value {
		if value == nil {
			continue
		}
		if _, err := solana.PublicKeyFromBase58(value.Owner); err != nil {
			return 0, nil, errors.New("confirmed account owner is invalid")
		}
		if len(value.Data) != 2 || value.Data[1] != "base64" {
			return 0, nil, errors.New("confirmed account encoding is invalid")
		}
		encoded, ok := value.Data[0].(string)
		if !ok {
			return 0, nil, errors.New("confirmed account data is invalid")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return 0, nil, fmt.Errorf("confirmed account base64: %w", err)
		}
		accounts[index] = &Account{Address: addresses[index], Owner: value.Owner, Lamports: value.Lamports, Executable: value.Executable, Data: data}
	}
	return raw.Context.Slot, accounts, nil
}

func (r *LiveObservationReader) GetAccount(ctx context.Context, key solana.PublicKey) (*Account, error) {
	_, accounts, err := r.GetMultipleAccounts(ctx, []solana.PublicKey{key})
	if err != nil {
		return nil, err
	}
	return accounts[0], nil
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
	slot, accounts, err := reader.GetMultipleAccounts(ctx, unique)
	if err != nil {
		return nil, err
	}
	if len(accounts) != len(unique) || slot == 0 {
		return nil, errors.New("confirmed account response omitted its slot or account entries")
	}
	for index, account := range accounts {
		if account != nil && account.Address != unique[index].String() {
			return nil, errors.New("confirmed account response identity drifted")
		}
	}
	required := func(key solana.PublicKey) (*Account, error) {
		for position, candidate := range unique {
			if candidate == key {
				if account := accounts[position]; account != nil {
					return account, nil
				}
			}
		}
		return nil, fmt.Errorf("required mainnet account %s is absent", key)
	}
	optional := func(key solana.PublicKey) *Account {
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
	claim, err := classicBalance(claimAccount, USDCMint, topology.Vault)
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
		collateralAPY, err := reserveAPYBPS(collateralReserveAccount.Data, collateralReserve, true)
		if err != nil {
			return nil, err
		}
		debtAPY, err := reserveAPYBPS(debtReserveAccount.Data, debtReserve, false)
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
			collateralAmount, err = classicBalance(account, config.CollateralMint, topology.Vault)
			if err != nil {
				return nil, err
			}
		}
		debtAmount := uint64(0)
		if account := optional(config.DebtCustody); account != nil {
			debtAmount, err = tokenBalanceAmount(account, config.DebtMint, config.DebtTokenProgram, topology.Vault)
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
		if program != mustKey(TokenProgram) && program != mustKey(Token2022Program) {
			return nil, errors.New("external custody token program is unsupported")
		}
		account, err := required(key)
		if err != nil {
			return nil, err
		}
		amount, err := tokenBalanceForOwner(account, custody.Mint, program, nil)
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

func tokenBalanceAmount(account *Account, mint string, tokenProgram solana.PublicKey, owner solana.PublicKey) (uint64, error) {
	if tokenProgram == mustKey(TokenProgram) {
		return classicBalance(account, mint, owner)
	}
	if tokenProgram == mustKey(Token2022Program) {
		return tokenBalanceForOwner(account, mint, tokenProgram, &owner)
	}
	return 0, errors.New("configured custody token program is unsupported")
}

func classicBalance(account *Account, mint string, owner solana.PublicKey) (uint64, error) {
	return tokenBalanceForOwner(account, mint, mustKey(TokenProgram), &owner)
}

// tokenBalanceForOwner validates an SPL token account (classic base layout;
// Token-2022 shares it before extensions) and returns its raw amount.
func tokenBalanceForOwner(account *Account, mint string, tokenProgram solana.PublicKey, owner *solana.PublicKey) (uint64, error) {
	if account == nil {
		return 0, errors.New("token account is absent")
	}
	expectedOwner := tokenProgram.String()
	if account.Owner != expectedOwner {
		return 0, errors.New("token account has the wrong owner")
	}
	if len(account.Data) < 165 {
		return 0, errors.New("token account is too short")
	}
	if account.Executable || (account.Data[108] != 1 && account.Data[108] != 2) {
		return 0, errors.New("token account is not initialized")
	}
	if tokenProgram == mustKey(TokenProgram) && len(account.Data) != 165 {
		return 0, errors.New("classic token account layout drifted")
	}
	if tokenProgram == mustKey(Token2022Program) && len(account.Data) > 165 {
		if len(account.Data) < 166 || account.Data[165] != 2 {
			return 0, errors.New("Token-2022 account type drifted")
		}
		for offset := 166; offset < len(account.Data); {
			if allZero(account.Data[offset:]) {
				break
			}
			if len(account.Data)-offset < 4 {
				return 0, errors.New("Token-2022 extension header is truncated")
			}
			length := int(binary.LittleEndian.Uint16(account.Data[offset+2 : offset+4]))
			if length > len(account.Data)-offset-4 {
				return 0, errors.New("Token-2022 extension is truncated")
			}
			offset += 4 + length
		}
	}
	gotMint := solana.PublicKeyFromBytes(account.Data[0:32])
	expectedMint, err := solana.PublicKeyFromBase58(mint)
	if err != nil {
		return 0, errors.New("custody mint is invalid")
	}
	if gotMint != expectedMint {
		return 0, errors.New("custody mint or authority drifted")
	}
	if owner != nil {
		gotOwner := solana.PublicKeyFromBytes(account.Data[32:64])
		if gotOwner != *owner {
			return 0, errors.New("custody mint or authority drifted")
		}
	}
	return binary.LittleEndian.Uint64(account.Data[64:72]), nil
}

// KLend layout offsets, shared with the reviewed decoders in this module tree.
const (
	obligationLength        = 3344
	obligationDiscriminator = "\xa8\xce\x8djXL\xac\xa7"
	reserveLength           = 8624
	reserveDiscriminator    = "+\xf2\xcc\xca\x1a\xf7;\x7f"
	reserveConfigOffset     = 4856
	// Cargo.lock pins klend-interface 23b9f2b; repr(C) Obligation includes
	// four u128 value fields after five 200-byte borrow slots.
	obligationUnhealthyOffset = 2256
	obligationElevationOffset = 2285
)

func obligationEnvelope(account *Account, address solana.PublicKey, market, vault solana.PublicKey) ([]byte, error) {
	if err := klendEnvelope(account, address, obligationLength, obligationDiscriminator); err != nil {
		return nil, err
	}
	if !sameKey(account.Data[32:64], market) || !sameKey(account.Data[64:96], vault) {
		return nil, errors.New("obligation identity drifted")
	}
	return account.Data, nil
}

func klendEnvelope(account *Account, address solana.PublicKey, length int, discriminator string) error {
	if account == nil || account.Address != address.String() || account.Owner != KlendProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) != length || string(account.Data[:8]) != discriminator {
		return errors.New("KLend account envelope or layout drifted")
	}
	return nil
}

func decodeObligation(account *Account, config StrategyConfig, vault solana.PublicKey, collateralReserve, debtReserve *decodedReserve, collateralAPY, debtAPY uint64) (*StrategyObservation, error) {
	data, err := obligationEnvelope(account, config.Obligation, config.Market, vault)
	if err != nil {
		return nil, err
	}
	deposits, borrows := 0, 0
	var collateralDepositedRaw uint64
	var debtSF *big.Int
	for index := 0; index < 8; index++ {
		offset := 96 + index*136
		amount := binary.LittleEndian.Uint64(data[offset+32 : offset+40])
		if zeroKey(data[offset : offset+32]) {
			continue
		}
		if !sameKey(data[offset:offset+32], config.CollateralReserve) {
			return nil, errors.New("obligation reserve topology drifted")
		}
		deposits++
		collateralDepositedRaw = amount
	}
	for index := 0; index < 5; index++ {
		offset := 1208 + index*200
		if zeroKey(data[offset : offset+32]) {
			continue
		}
		if !sameKey(data[offset:offset+32], config.DebtReserve) {
			return nil, errors.New("obligation reserve topology drifted")
		}
		borrows++
		debtSF = littleInt(data[offset+88 : offset+104])
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
	if data[obligationElevationOffset] != 0 {
		return nil, errors.New("obligation identity drifted")
	}
	unhealthy := littleInt(data[obligationUnhealthyOffset : obligationUnhealthyOffset+16])
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
		ObligationLastUpdateSlot:        binary.LittleEndian.Uint64(data[16:24]),
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
}

func decodeReserve(account *Account, config StrategyConfig) (*decodedReserve, error) {
	address := config.CollateralReserve
	mint := config.CollateralMint
	if account != nil && account.Address != address.String() && config.DebtReserve.String() == account.Address {
		address = config.DebtReserve
		mint = config.DebtMint
	}
	if err := klendEnvelope(account, address, reserveLength, reserveDiscriminator); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint64(account.Data[8:16]) != 1 || !sameKey(account.Data[32:64], config.Market) {
		return nil, errors.New("reserve identity, status, or price drifted")
	}
	if !sameKey(account.Data[128:160], mustKey(mint)) {
		return nil, errors.New("reserve identity, status, or price drifted")
	}
	status := account.Data[reserveConfigOffset]
	if status != 0 {
		return nil, errors.New("reserve identity, status, or price drifted")
	}
	price := littleInt(account.Data[248:264])
	if price.Sign() == 0 {
		return nil, errors.New("reserve identity, status, or price drifted")
	}
	totalLiquidity, err := reserveTotalLiquiditySF(account.Data)
	if err != nil {
		return nil, err
	}
	return &decodedReserve{
		LastUpdateSlot:       binary.LittleEndian.Uint64(account.Data[16:24]),
		Status:               status,
		MarketPriceSF:        price,
		Decimals:             binary.LittleEndian.Uint64(account.Data[272:280]),
		CollateralMintSupply: binary.LittleEndian.Uint64(account.Data[2592:2600]),
		TotalLiquiditySF:     totalLiquidity,
	}, nil
}

func reserveTotalLiquiditySF(data []byte) (*big.Int, error) {
	total := new(big.Int).Lsh(new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[224:232])), 60)
	total.Add(total, littleInt(data[232:248]))
	for _, offset := range []int{344, 360, 376} {
		fee := littleInt(data[offset : offset+16])
		if total.Cmp(fee) < 0 {
			return nil, errors.New("reserve total liquidity underflowed fees")
		}
		total.Sub(total, fee)
	}
	if total.Sign() == 0 {
		return nil, errors.New("reserve total liquidity is zero")
	}
	return total, nil
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

func littleInt(value []byte) *big.Int {
	reversed := append([]byte(nil), value...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return new(big.Int).SetBytes(reversed)
}

func sameKey(data []byte, key solana.PublicKey) bool {
	if len(data) != 32 {
		return false
	}
	got := solana.PublicKeyFromBytes(data)
	return got == key
}

func zeroKey(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func allZero(data []byte) bool { return zeroKey(data) }

// APY: 500ms slot clock, matching the codec call site in observe.rs.
const (
	slotsPerSecond = 2.0
	secondsPerYear = 365.25 * 24 * 60 * 60
	slotDurationMS = 500.0
)

// reserveAPYBPS ports the pinned loyal-kamino-codec curve calculation. Host
// fixed interest belongs to borrowing APR, not supplier income.
func reserveAPYBPS(data []byte, reserve *decodedReserve, supply bool) (uint64, error) {
	available := float64(binary.LittleEndian.Uint64(data[224:232]))
	borrowed := scaledFractionToFloat(data[232:248])
	protocolFees := scaledFractionToFloat(data[344:360])
	referrerFees := scaledFractionToFloat(data[360:376])
	pendingFees := scaledFractionToFloat(data[376:392])
	total := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	if total <= 0 || math.IsInf(total, 0) || math.IsNaN(total) {
		return 0, errors.New("reserve utilization is invalid")
	}
	utilization := borrowed / total
	if !finiteFloat(utilization) || utilization < 0 || utilization > 1.01 {
		return 0, errors.New("reserve utilization is invalid")
	}
	curveAPR := borrowCurveAPR(data, utilization) * (1000.0 / slotsPerSecond / slotDurationMS)
	ratio := 0.0
	if supply {
		takeRate := data[reserveConfigOffset+14]
		if takeRate > 100 {
			return 0, errors.New("reserve take rate is invalid")
		}
		ratio = utilization * curveAPR * (1 - float64(takeRate)/100)
	} else {
		hostFixedBPS := binary.LittleEndian.Uint16(data[reserveConfigOffset+2 : reserveConfigOffset+4])
		ratio = curveAPR + float64(hostFixedBPS)/10_000*(1000.0/slotsPerSecond/slotDurationMS)
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

func borrowCurveAPR(data []byte, utilization float64) float64 {
	type point struct{ utilization, rate float64 }
	points := make([]point, 0, 11)
	config := data[reserveConfigOffset:]
	for index := 0; index < 11; index++ {
		offset := 64 + index*8
		points = append(points, point{
			utilization: float64(binary.LittleEndian.Uint32(config[offset:offset+4])) / 10_000,
			rate:        float64(binary.LittleEndian.Uint32(config[offset+4:offset+8])) / 10_000,
		})
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

func scaledFractionToFloat(value []byte) float64 {
	high := binary.LittleEndian.Uint64(value[8:16])
	low := binary.LittleEndian.Uint64(value[:8])
	return float64(high)*16 + float64(low)/float64(fractionOneSF)
}

func finiteFloat(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// CollateralToLiquidityRaw mirrors collateral_to_liquidity_raw.
func CollateralToLiquidityRaw(position *StrategyObservation, collateralRaw uint64) (uint64, error) {
	if collateralRaw == 0 {
		return 0, nil
	}
	if position == nil || position.CollateralTotalLiquiditySF == nil || position.CollateralTotalLiquiditySF.Sign() <= 0 {
		return 0, errors.New("collateral reserve liquidity is unknown")
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(position.CollateralTotalSupplyRaw), 60)
	if denominator.Sign() == 0 {
		return 0, errors.New("collateral reserve supply is zero")
	}
	liquidity := new(big.Int).Mul(new(big.Int).SetUint64(collateralRaw), position.CollateralTotalLiquiditySF)
	liquidity.Div(liquidity, denominator)
	if !liquidity.IsUint64() {
		return 0, errors.New("redeemable collateral exceeds u64")
	}
	return liquidity.Uint64(), nil
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
