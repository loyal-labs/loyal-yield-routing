package earnprep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// The Kamino reserve and obligation readers for the withdraw family, mirrors
// of the TS parsers (CL = web/packages/smart-account-vaults/src/client.ts):
// parseKaminoReserveSnapshot (CL:1249), parseKaminoReserveTokenAccounts
// (CL:1095), parseKaminoObligationAccount (CL:1184) and the redeemable math
// (CL:1312-1354). The TS math clamps a reserve whose fees exceed its liquidity
// to a zero rate and falls back to the 1:1 identity when the rate is
// unavailable; kamino.Reserve.TotalLiquiditySF and kamino.CollateralToLiquidity
// error in those cases instead, so this path never calls them.

// Reserve layout offsets, each from the account start so they include the
// 8-byte anchor discriminator (KAMINO_RESERVE_LAYOUT_OFFSETS, CL:366-385).
const (
	withdrawReserveLendingMarketOffset   = 32
	withdrawReserveFarmCollateralOffset  = 64
	withdrawReserveFarmDebtOffset        = 96
	withdrawReserveLiquidityMintOffset   = 128
	withdrawReserveLiquiditySupplyOffset = 160
	withdrawReserveAvailableOffset       = 224
	withdrawReserveBorrowedOffset        = 232
	withdrawReserveProtocolFeesOffset    = 344
	withdrawReserveReferrerFeesOffset    = 360
	withdrawReservePendingFeesOffset     = 376
	withdrawReserveTokenProgramOffset    = 408
	withdrawReserveCollateralMintOffset  = 2560
	withdrawReserveCollateralMintSupply  = 2592
	withdrawReserveCollateralVaultOffset = 2600

	withdrawReserveTokenAccountsLength = withdrawReserveCollateralVaultOffset + 32
	withdrawReserveSnapshotLength      = withdrawReserveCollateralMintSupply + 8
)

const (
	withdrawObligationLendingMarketOffset = 32
	withdrawObligationOwnerOffset         = 64
	withdrawObligationDepositsOffset      = 96
	withdrawObligationDepositCount        = 8
	withdrawObligationDepositSize         = 136
	withdrawObligationDepositAmountOffset = 32

	withdrawObligationMinLength = withdrawObligationDepositsOffset + withdrawObligationDepositCount*withdrawObligationDepositSize
)

type WithdrawReserveSnapshot struct {
	CollateralSupplyRaw  uint64
	TotalLiquidityScaled *big.Int
}

func withdrawReserveData(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, kamino.ReserveDiscriminator[:]) {
		return nil, errors.New("Kamino reserve account has an invalid discriminator.")
	}
	return data, nil
}

func ParseWithdrawReserveSnapshot(data []byte) (WithdrawReserveSnapshot, error) {
	data, err := withdrawReserveData(data)
	if err != nil {
		return WithdrawReserveSnapshot{}, err
	}
	if len(data) < withdrawReserveSnapshotLength {
		return WithdrawReserveSnapshot{}, errors.New("Kamino reserve account is smaller than expected.")
	}
	gross := new(big.Int).Lsh(new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[withdrawReserveAvailableOffset:withdrawReserveAvailableOffset+8])), kamino.FractionBits)
	gross.Add(gross, withdrawU128(data, withdrawReserveBorrowedOffset))
	fees := withdrawU128(data, withdrawReserveProtocolFeesOffset)
	fees.Add(fees, withdrawU128(data, withdrawReserveReferrerFeesOffset))
	fees.Add(fees, withdrawU128(data, withdrawReservePendingFeesOffset))
	total := gross
	if gross.Cmp(fees) <= 0 {
		total = big.NewInt(0)
	} else {
		total = total.Sub(total, fees)
	}
	return WithdrawReserveSnapshot{
		CollateralSupplyRaw:  binary.LittleEndian.Uint64(data[withdrawReserveCollateralMintSupply : withdrawReserveCollateralMintSupply+8]),
		TotalLiquidityScaled: total,
	}, nil
}

type WithdrawReserveTokenAccounts struct {
	FarmCollateral, FarmDebt                       solana.PublicKey
	LendingMarket                                  solana.PublicKey
	ReserveCollateralMint, ReserveCollateralSupply solana.PublicKey
	ReserveLiquidityMint, ReserveLiquiditySupply   solana.PublicKey
	ReserveLiquidityTokenProgram                   solana.PublicKey
}

func ParseWithdrawReserveTokenAccounts(data []byte) (WithdrawReserveTokenAccounts, error) {
	data, err := withdrawReserveData(data)
	if err != nil {
		return WithdrawReserveTokenAccounts{}, err
	}
	if len(data) < withdrawReserveTokenAccountsLength {
		return WithdrawReserveTokenAccounts{}, errors.New("Kamino reserve account is smaller than expected.")
	}
	return WithdrawReserveTokenAccounts{
		FarmCollateral:               withdrawKey(data, withdrawReserveFarmCollateralOffset),
		FarmDebt:                     withdrawKey(data, withdrawReserveFarmDebtOffset),
		LendingMarket:                withdrawKey(data, withdrawReserveLendingMarketOffset),
		ReserveCollateralMint:        withdrawKey(data, withdrawReserveCollateralMintOffset),
		ReserveCollateralSupply:      withdrawKey(data, withdrawReserveCollateralVaultOffset),
		ReserveLiquidityMint:         withdrawKey(data, withdrawReserveLiquidityMintOffset),
		ReserveLiquiditySupply:       withdrawKey(data, withdrawReserveLiquiditySupplyOffset),
		ReserveLiquidityTokenProgram: withdrawKey(data, withdrawReserveTokenProgramOffset),
	}, nil
}

type WithdrawObligationDeposit struct {
	DepositedAmountRaw uint64
	Reserve            solana.PublicKey
	SlotIndex          int
}

type WithdrawObligation struct {
	Deposits      []WithdrawObligationDeposit
	LendingMarket solana.PublicKey
	Owner         solana.PublicKey
}

func ParseWithdrawObligation(data []byte) (WithdrawObligation, error) {
	if !bytes.HasPrefix(data, kamino.ObligationDiscriminator[:]) {
		return WithdrawObligation{}, errors.New("Kamino obligation account has an invalid discriminator.")
	}
	if len(data) < withdrawObligationMinLength {
		return WithdrawObligation{}, errors.New("Kamino obligation account is smaller than expected.")
	}
	obligation := WithdrawObligation{
		LendingMarket: withdrawKey(data, withdrawObligationLendingMarketOffset),
		Owner:         withdrawKey(data, withdrawObligationOwnerOffset),
	}
	for slot := 0; slot < withdrawObligationDepositCount; slot++ {
		offset := withdrawObligationDepositsOffset + slot*withdrawObligationDepositSize
		reserve := withdrawKey(data, offset)
		amount := binary.LittleEndian.Uint64(data[offset+withdrawObligationDepositAmountOffset : offset+withdrawObligationDepositAmountOffset+8])
		if reserve.IsZero() || amount == 0 {
			continue
		}
		obligation.Deposits = append(obligation.Deposits, WithdrawObligationDeposit{
			DepositedAmountRaw: amount,
			Reserve:            reserve,
			SlotIndex:          slot,
		})
	}
	return obligation, nil
}

func WithdrawRedeemableLiquidityRaw(collateralAmountRaw uint64, snapshot WithdrawReserveSnapshot) (uint64, error) {
	if collateralAmountRaw == 0 {
		return 0, nil
	}
	if snapshot.CollateralSupplyRaw == 0 || snapshot.TotalLiquidityScaled.Sign() == 0 {
		return collateralAmountRaw, nil
	}
	liquidity := new(big.Int).Mul(new(big.Int).SetUint64(collateralAmountRaw), snapshot.TotalLiquidityScaled)
	liquidity.Quo(liquidity, new(big.Int).Lsh(new(big.Int).SetUint64(snapshot.CollateralSupplyRaw), kamino.FractionBits))
	if !liquidity.IsUint64() {
		return 0, fmt.Errorf("Kamino redeemable liquidity %s exceeds u64", liquidity)
	}
	return liquidity.Uint64(), nil
}

// WithdrawCollateralForRedeemableLiquidityRaw mirrors
// calculateKaminoCollateralAmountForRedeemableLiquidityRaw (CL:1332): zero
// liquidity needs no collateral, an unavailable rate prices it 1:1, otherwise
// the collateral CEILs so it redeems at least the liquidity.
func WithdrawCollateralForRedeemableLiquidityRaw(liquidityAmountRaw uint64, snapshot WithdrawReserveSnapshot) (uint64, error) {
	if liquidityAmountRaw == 0 {
		return 0, nil
	}
	if snapshot.CollateralSupplyRaw == 0 || snapshot.TotalLiquidityScaled.Sign() == 0 {
		return liquidityAmountRaw, nil
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(liquidityAmountRaw), new(big.Int).SetUint64(snapshot.CollateralSupplyRaw))
	numerator.Lsh(numerator, kamino.FractionBits)
	numerator.Add(numerator, snapshot.TotalLiquidityScaled)
	numerator.Sub(numerator, big.NewInt(1))
	collateral := numerator.Quo(numerator, snapshot.TotalLiquidityScaled)
	if !collateral.IsUint64() {
		return 0, fmt.Errorf("Kamino required collateral %s exceeds u64", collateral)
	}
	return collateral.Uint64(), nil
}

func withdrawU128(data []byte, offset int) *big.Int {
	value := new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[offset+8 : offset+16]))
	return value.Lsh(value, 64).Or(value, new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[offset:offset+8])))
}

func withdrawKey(data []byte, offset int) solana.PublicKey {
	return solana.PublicKeyFromBytes(data[offset : offset+32])
}
