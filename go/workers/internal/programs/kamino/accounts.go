package kamino

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Anchor account discriminators: sha256("account:<name>")[:8].
var (
	ReserveDiscriminator       = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}
	ObligationDiscriminator    = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
	LendingMarketDiscriminator = [8]byte{246, 114, 50, 98, 72, 157, 28, 120}
	UserMetadataDiscriminator  = [8]byte{157, 214, 220, 235, 98, 135, 171, 28}
	// Farms UserState.
	FarmUserStateDiscriminator = [8]byte{72, 177, 85, 249, 76, 167, 186, 126}
)

// The reserve version every decoded reserve must carry.
const reserveVersion = 1

// Scaled fractions (U68F60 "_sf" values) are kept as their raw little-endian
// on-chain bytes; BigFractionBytes values are their four u64 limbs.

// CurvePoint is one point of a reserve's borrow rate curve.
type CurvePoint struct {
	UtilizationRateBPS, BorrowRateBPS uint32
}

// WithdrawalCap is a reserve's deposit or debt withdrawal cap.
type WithdrawalCap struct {
	ConfigCapacity, CurrentTotal                      int64
	LastIntervalStartTimestamp, IntervalLengthSeconds uint64
}

type Reserve struct {
	LastUpdateSlot  uint64
	LastUpdateStale bool
	PriceStatus     uint8
	LendingMarket   solana.PublicKey
	FarmCollateral  solana.PublicKey // zero when absent

	LiquidityMint, LiquiditySupply, LiquidityTokenProgram   solana.PublicKey
	AvailableAmount, MarketPriceLastUpdatedTS, MintDecimals uint64
	BorrowedAmountSF, MarketPriceSF                         [16]byte
	AccumulatedProtocolFeesSF, AccumulatedReferrerFeesSF    [16]byte
	PendingReferrerFeesSF                                   [16]byte
	CumulativeBorrowRateBSF                                 [32]byte
	BorrowedAmountOutsideElevationGroup                     uint64

	CollateralMint, CollateralSupply solana.PublicKey
	CollateralMintTotalSupply        uint64

	// ReserveConfig.
	Status, ProtocolTakeRatePct, LoanToValuePct, LiquidationThresholdPct uint8
	UtilizationLimitBlockBorrowingAbovePct                               uint8
	HostFixedInterestRateBPS                                             uint16
	EmergencyMode, DisableUsageAsCollOutsideEmode                        bool
	BorrowFeeSF, FlashLoanFeeSF, BorrowFactorPct                         uint64
	DepositLimit, BorrowLimit, BorrowLimitOutsideElevationGroup          uint64
	BorrowRateCurve                                                      [11]CurvePoint
	Name                                                                 [32]byte
	ScopePriceFeed, SwitchboardPriceAggregator                           solana.PublicKey
	SwitchboardTWAPAggregator, PythPrice                                 solana.PublicKey
	DepositWithdrawalCap, DebtWithdrawalCap                              WithdrawalCap
}

// Reserve is the subset of a KLend Reserve the workers read.

// Reserve byte offsets, discriminator included.
const (
	reserveLiquidity  = 128
	reserveCollateral = 2560
	reserveConfig     = 4856
)

// DecodeReserve decodes a funded, non-executable KLend reserve of the current
// version.
func DecodeReserve(a *chain.Account) (Reserve, error) {
	if err := envelope(a, ProgramID, ReserveSize, ReserveDiscriminator, "reserve"); err != nil {
		return Reserve{}, err
	}
	d := a.Data
	if u64(d, 8) != reserveVersion {
		return Reserve{}, fmt.Errorf("Kamino reserve %s version %d is unsupported", a.Key, u64(d, 8))
	}
	l, c, k := d[reserveLiquidity:], d[reserveCollateral:], d[reserveConfig:]
	r := Reserve{
		LastUpdateSlot: u64(d, 16), LastUpdateStale: d[24] != 0, PriceStatus: d[25],
		LendingMarket: key(d, 32), FarmCollateral: key(d, 64),

		LiquidityMint: key(l, 0), LiquiditySupply: key(l, 32),
		AvailableAmount: u64(l, 96), BorrowedAmountSF: [16]byte(l[104:120]), MarketPriceSF: [16]byte(l[120:136]),
		MarketPriceLastUpdatedTS: u64(l, 136), MintDecimals: u64(l, 144), CumulativeBorrowRateBSF: [32]byte(l[168:200]),
		AccumulatedProtocolFeesSF: [16]byte(l[216:232]), AccumulatedReferrerFeesSF: [16]byte(l[232:248]),
		PendingReferrerFeesSF: [16]byte(l[248:264]), LiquidityTokenProgram: key(l, 280),

		CollateralMint: key(c, 0), CollateralMintTotalSupply: u64(c, 32), CollateralSupply: key(c, 40),

		Status: k[0], HostFixedInterestRateBPS: binary.LittleEndian.Uint16(k[2:4]), EmergencyMode: k[8] != 0,
		ProtocolTakeRatePct: k[14], LoanToValuePct: k[16], LiquidationThresholdPct: k[17],
		BorrowFeeSF: u64(k, 40), FlashLoanFeeSF: u64(k, 48), BorrowFactorPct: u64(k, 152),
		DepositLimit: u64(k, 160), BorrowLimit: u64(k, 168), Name: [32]byte(k[176:208]),
		ScopePriceFeed: key(k, 256), SwitchboardPriceAggregator: key(k, 304), SwitchboardTWAPAggregator: key(k, 336), PythPrice: key(k, 368),
		DepositWithdrawalCap: withdrawalCap(k[560:592]), DebtWithdrawalCap: withdrawalCap(k[592:624]),
		DisableUsageAsCollOutsideEmode: k[644] != 0, UtilizationLimitBlockBorrowingAbovePct: k[645],
		BorrowLimitOutsideElevationGroup: u64(k, 648),

		BorrowedAmountOutsideElevationGroup: u64(d, 6704),
	}
	for i := range r.BorrowRateCurve {
		r.BorrowRateCurve[i] = CurvePoint{binary.LittleEndian.Uint32(k[64+i*8:]), binary.LittleEndian.Uint32(k[68+i*8:])}
	}
	return r, nil
}

// TotalLiquiditySF is ReserveLiquidity::total_supply as a scaled fraction:
// available plus borrowed liquidity minus all three fee pools.
func (r Reserve) TotalLiquiditySF() (*big.Int, error) {
	total := new(big.Int).Lsh(new(big.Int).SetUint64(r.AvailableAmount), 60)
	total.Add(total, U128(r.BorrowedAmountSF))
	for _, fee := range [][16]byte{r.AccumulatedProtocolFeesSF, r.AccumulatedReferrerFeesSF, r.PendingReferrerFeesSF} {
		value := U128(fee)
		if total.Cmp(value) < 0 {
			return nil, errors.New("Kamino reserve total liquidity underflowed fees")
		}
		total.Sub(total, value)
	}
	return total, nil
}

// CollateralToLiquidity is what collateral redeems from the reserve.
func (r Reserve) CollateralToLiquidity(collateral uint64) (uint64, error) {
	if collateral == 0 {
		return 0, nil
	}
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return 0, err
	}
	return CollateralToLiquidity(total, r.CollateralMintTotalSupply, collateral)
}

// CollateralToLiquidity floors collateral times the exchange value: total
// liquidity (a scaled fraction) per collateral mint unit. A positive amount
// needs a positive total liquidity and collateral supply.
func CollateralToLiquidity(totalLiquiditySF *big.Int, collateralSupply, collateral uint64) (uint64, error) {
	if collateral == 0 {
		return 0, nil
	}
	if totalLiquiditySF == nil || totalLiquiditySF.Sign() <= 0 || collateralSupply == 0 {
		return 0, errors.New("Kamino collateral exchange rate is unavailable")
	}
	liquidity := new(big.Int).Mul(new(big.Int).SetUint64(collateral), totalLiquiditySF)
	liquidity.Quo(liquidity, new(big.Int).Lsh(new(big.Int).SetUint64(collateralSupply), 60))
	if !liquidity.IsUint64() {
		return 0, errors.New("Kamino redeemable collateral exceeds u64")
	}
	return liquidity.Uint64(), nil
}

// LiquidityToCollateral floors liquidity over the exchange value: the
// collateral a deposit of liquidity mints at the reserve's current rate. The
// rate only grows, so against a later read it never exceeds what was minted.
func (r Reserve) LiquidityToCollateral(liquidity uint64) (uint64, error) {
	if liquidity == 0 {
		return 0, nil
	}
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return 0, err
	}
	if total.Sign() <= 0 || r.CollateralMintTotalSupply == 0 {
		return 0, errors.New("Kamino collateral exchange rate is unavailable")
	}
	collateral := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), new(big.Int).Lsh(new(big.Int).SetUint64(r.CollateralMintTotalSupply), 60))
	collateral.Quo(collateral, total)
	if !collateral.IsUint64() {
		return 0, errors.New("Kamino minted collateral exceeds u64")
	}
	return collateral.Uint64(), nil
}

// MinimumDeposit is the smallest liquidity amount that mints one collateral
// unit: the ceiling of one unit's exchange value, or 1 at the initial rate.
func (r Reserve) MinimumDeposit() (uint64, error) {
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return 0, err
	}
	if total.Sign() == 0 || r.CollateralMintTotalSupply == 0 {
		return 1, nil
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(r.CollateralMintTotalSupply), 60)
	amount := new(big.Int).Add(total, denominator)
	amount.Sub(amount, big.NewInt(1)).Quo(amount, denominator)
	if !amount.IsUint64() || amount.Sign() <= 0 {
		return 0, errors.New("minimum Kamino deposit exceeds positive u64")
	}
	return amount.Uint64(), nil
}

// ObligationCollateral is one deposit slot; an empty slot has a zero reserve.
type ObligationCollateral struct {
	Reserve         solana.PublicKey
	DepositedAmount uint64
}

// ObligationLiquidity is one borrow slot; an empty slot has a zero reserve.
type ObligationLiquidity struct {
	Reserve                 solana.PublicKey
	CumulativeBorrowRateBSF [32]byte
	BorrowedAmountSF        [16]byte
}

// Obligation is the subset of a KLend Obligation the workers read.
type Obligation struct {
	LastUpdateSlot         uint64
	LendingMarket, Owner   solana.PublicKey
	Deposits               [8]ObligationCollateral
	Borrows                [5]ObligationLiquidity
	UnhealthyBorrowValueSF [16]byte
	ElevationGroup         uint8
}

// obligationOwnerOffset is where an obligation account holds its owner.
const obligationOwnerOffset = 64

// DecodeObligation decodes a funded, non-executable KLend obligation.
func DecodeObligation(a *chain.Account) (Obligation, error) {
	if err := envelope(a, ProgramID, ObligationSize, ObligationDiscriminator, "obligation"); err != nil {
		return Obligation{}, err
	}
	d := a.Data
	o := Obligation{
		LastUpdateSlot: u64(d, 16), LendingMarket: key(d, 32), Owner: key(d, obligationOwnerOffset),
		UnhealthyBorrowValueSF: [16]byte(d[2256:2272]), ElevationGroup: d[2285],
	}
	for i := range o.Deposits {
		offset := 96 + i*136
		o.Deposits[i] = ObligationCollateral{key(d, offset), u64(d, offset+32)}
	}
	for i := range o.Borrows {
		offset := 1208 + i*200
		o.Borrows[i] = ObligationLiquidity{key(d, offset), [32]byte(d[offset+32 : offset+64]), [16]byte(d[offset+88 : offset+104])}
	}
	return o, nil
}

// Collateral is the amount deposited from reserve; zero when it is absent.
func (o Obligation) Collateral(reserve solana.PublicKey) uint64 {
	for _, deposit := range o.Deposits {
		if deposit.Reserve == reserve {
			return deposit.DepositedAmount
		}
	}
	return 0
}

// DepositReserves and BorrowReserves list the occupied slots in slot order.
func (o Obligation) DepositReserves() []solana.PublicKey {
	var out []solana.PublicKey
	for _, deposit := range o.Deposits {
		if !deposit.Reserve.IsZero() {
			out = append(out, deposit.Reserve)
		}
	}
	return out
}

func (o Obligation) BorrowReserves() []solana.PublicKey {
	var out []solana.PublicKey
	for _, borrow := range o.Borrows {
		if !borrow.Reserve.IsZero() {
			out = append(out, borrow.Reserve)
		}
	}
	return out
}

// DecodeLendingMarketEmergencyMode reads a funded, non-executable lending
// market's global emergency_mode flag.
func DecodeLendingMarketEmergencyMode(a *chain.Account) (bool, error) {
	if err := envelope(a, ProgramID, LendingMarketSize, LendingMarketDiscriminator, "lending market"); err != nil {
		return false, err
	}
	return a.Data[122] != 0, nil
}

// UserMetadata is the subset of a KLend UserMetadata the workers read.
type UserMetadata struct {
	Referrer, Owner solana.PublicKey
}

// DecodeUserMetadata decodes a funded, non-executable KLend UserMetadata.
func DecodeUserMetadata(a *chain.Account) (UserMetadata, error) {
	if err := envelope(a, ProgramID, UserMetadataSize, UserMetadataDiscriminator, "user metadata"); err != nil {
		return UserMetadata{}, err
	}
	return UserMetadata{Referrer: key(a.Data, 8), Owner: key(a.Data, 80)}, nil
}

// FarmUserState is the subset of a Farms UserState the workers read.
type FarmUserState struct {
	FarmState, Owner, Delegatee solana.PublicKey
	IsFarmDelegated             bool
}

// DecodeFarmUserState decodes a funded, non-executable Farms UserState.
func DecodeFarmUserState(a *chain.Account) (FarmUserState, error) {
	if err := envelope(a, FarmsProgramID, FarmUserStateSize, FarmUserStateDiscriminator, "farm user state"); err != nil {
		return FarmUserState{}, err
	}
	d := a.Data
	return FarmUserState{FarmState: key(d, 16), Owner: key(d, 48), IsFarmDelegated: d[80] == 1, Delegatee: key(d, 480)}, nil
}

// U128 reads a little-endian scaled fraction.
func U128(value [16]byte) *big.Int {
	high := new(big.Int).SetUint64(binary.LittleEndian.Uint64(value[8:]))
	return high.Lsh(high, 64).Or(high, new(big.Int).SetUint64(binary.LittleEndian.Uint64(value[:8])))
}

func envelope(a *chain.Account, owner solana.PublicKey, size int, discriminator [8]byte, name string) error {
	if a == nil {
		return fmt.Errorf("Kamino %s is absent", name)
	}
	if a.Owner != owner || a.Executable || a.Lamports == 0 || len(a.Data) != size || [8]byte(a.Data[:8]) != discriminator {
		return fmt.Errorf("Kamino %s %s envelope or layout drifted", name, a.Key)
	}
	return nil
}

func u64(data []byte, offset int) uint64 { return binary.LittleEndian.Uint64(data[offset : offset+8]) }

func key(data []byte, offset int) solana.PublicKey {
	return solana.PublicKeyFromBytes(data[offset : offset+32])
}

func withdrawalCap(d []byte) WithdrawalCap {
	return WithdrawalCap{int64(u64(d, 0)), int64(u64(d, 8)), u64(d, 16), u64(d, 24)}
}
