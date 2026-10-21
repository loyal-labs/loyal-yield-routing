package kamino

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
)

var (
	ReserveDiscriminator       = anchor.Account("Reserve")
	ObligationDiscriminator    = anchor.Account("Obligation")
	LendingMarketDiscriminator = anchor.Account("LendingMarket")
	UserMetadataDiscriminator  = anchor.Account("UserMetadata")
	FarmUserStateDiscriminator = anchor.Account("UserState")
	FarmStateDiscriminator     = anchor.Account("FarmState")
)

const reserveVersion = 1

const FractionBits = 60

// ScaledFraction is a U68F60 on-chain value, kept as its raw little-endian
// bytes.
type ScaledFraction [16]byte

// BigScaledFraction is a cumulative borrow rate: four little-endian u64 limbs.
type BigScaledFraction [32]byte

func (f ScaledFraction) Int() *big.Int {
	high := new(big.Int).SetUint64(binary.LittleEndian.Uint64(f[8:]))
	return high.Lsh(high, 64).Or(high, new(big.Int).SetUint64(binary.LittleEndian.Uint64(f[:8])))
}

type CurvePoint struct {
	UtilizationRateBPS, BorrowRateBPS uint32
}

type WithdrawalCap struct {
	ConfigCapacity, CurrentTotal                      int64
	LastIntervalStartTimestamp, IntervalLengthSeconds uint64
}

type Reserve struct {
	LastUpdateSlot      uint64
	LastUpdateStale     bool
	PriceStatus         uint8
	LastUpdateTimestamp uint32
	LendingMarket       solana.PublicKey
	FarmCollateral      solana.PublicKey
	FarmDebt            solana.PublicKey

	LiquidityMint, LiquiditySupply, LiquidityTokenProgram   solana.PublicKey
	FeeReceiver                                             solana.PublicKey
	AvailableAmount, MarketPriceLastUpdatedTS, MintDecimals uint64
	BorrowedAmountSF, MarketPriceSF                         ScaledFraction
	AccumulatedProtocolFeesSF, AccumulatedReferrerFeesSF    ScaledFraction
	PendingReferrerFeesSF                                   ScaledFraction
	CumulativeBorrowRateBSF                                 BigScaledFraction
	BorrowedAmountOutsideElevationGroup                     uint64
	QueuedCollateral                                        uint64

	CollateralMint, CollateralSupply solana.PublicKey
	CollateralMintTotalSupply        uint64

	Status, ProtocolTakeRatePct, LoanToValuePct, LiquidationThresholdPct uint8
	UtilizationLimitBlockBorrowingAbovePct                               uint8
	HostFixedInterestRateBPS                                             uint16
	InterestRateBasis                                                    RateBasis
	EarlyRepayRemainingInterestPct                                       uint8
	// Term debt: a borrow must be repaid (or rolled over) within
	// DebtTermSeconds of its slot's last borrow, and every borrow by
	// DebtMaturityTimestamp, market-wide; zero is none. Past its end a borrow
	// can borrow no more and becomes liquidatable.
	DebtMaturityTimestamp, DebtTermSeconds                      uint64
	EmergencyMode, DisableUsageAsCollOutsideEmode               bool
	BorrowFeeSF, FlashLoanFeeSF, BorrowFactorPct                uint64
	DepositLimit, BorrowLimit, BorrowLimitOutsideElevationGroup uint64
	BorrowRateCurve                                             [11]CurvePoint
	Name                                                        [32]byte
	ScopePriceFeed, SwitchboardPriceAggregator                  solana.PublicKey
	SwitchboardTWAPAggregator, PythPrice                        solana.PublicKey
	DepositWithdrawalCap, DebtWithdrawalCap                     WithdrawalCap
}

const (
	reserveLiquidity  = 128
	reserveCollateral = 2560
	reserveConfig     = 4856
)

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
		LastUpdateSlot: u64(d, 16), LastUpdateStale: d[24] != 0, PriceStatus: d[25], LastUpdateTimestamp: binary.LittleEndian.Uint32(d[28:32]),
		LendingMarket: key(d, 32), FarmCollateral: key(d, 64), FarmDebt: key(d, 96),

		LiquidityMint: key(l, 0), LiquiditySupply: key(l, 32), FeeReceiver: key(l, 64),
		AvailableAmount: u64(l, 96), BorrowedAmountSF: ScaledFraction(l[104:120]), MarketPriceSF: ScaledFraction(l[120:136]),
		MarketPriceLastUpdatedTS: u64(l, 136), MintDecimals: u64(l, 144), CumulativeBorrowRateBSF: BigScaledFraction(l[168:200]),
		AccumulatedProtocolFeesSF: ScaledFraction(l[216:232]), AccumulatedReferrerFeesSF: ScaledFraction(l[232:248]),
		PendingReferrerFeesSF: ScaledFraction(l[248:264]), LiquidityTokenProgram: key(l, 280),

		CollateralMint: key(c, 0), CollateralMintTotalSupply: u64(c, 32), CollateralSupply: key(c, 40),

		Status: k[0], HostFixedInterestRateBPS: binary.LittleEndian.Uint16(k[2:4]), EmergencyMode: k[8] != 0, InterestRateBasis: RateBasis(k[9]),
		EarlyRepayRemainingInterestPct: k[7], DebtMaturityTimestamp: u64(k, 920), DebtTermSeconds: u64(k, 928),
		ProtocolTakeRatePct: k[14], LoanToValuePct: k[16], LiquidationThresholdPct: k[17],
		BorrowFeeSF: u64(k, 40), FlashLoanFeeSF: u64(k, 48), BorrowFactorPct: u64(k, 152),
		DepositLimit: u64(k, 160), BorrowLimit: u64(k, 168), Name: [32]byte(k[176:208]),
		ScopePriceFeed: oracle(k, 256), SwitchboardPriceAggregator: oracle(k, 304), SwitchboardTWAPAggregator: oracle(k, 336), PythPrice: oracle(k, 368),
		DepositWithdrawalCap: withdrawalCap(k[560:592]), DebtWithdrawalCap: withdrawalCap(k[592:624]),
		DisableUsageAsCollOutsideEmode: k[644] != 0, UtilizationLimitBlockBorrowingAbovePct: k[645],
		BorrowLimitOutsideElevationGroup: u64(k, 648),

		BorrowedAmountOutsideElevationGroup: u64(d, 6704), QueuedCollateral: u64(d, 6968),
	}
	for i := range r.BorrowRateCurve {
		r.BorrowRateCurve[i] = CurvePoint{binary.LittleEndian.Uint32(k[64+i*8:]), binary.LittleEndian.Uint32(k[68+i*8:])}
	}
	return r, nil
}

func (r Reserve) TotalLiquiditySF() (*big.Int, error) {
	total := new(big.Int).Lsh(new(big.Int).SetUint64(r.AvailableAmount), FractionBits)
	total.Add(total, r.BorrowedAmountSF.Int())
	for _, fee := range []ScaledFraction{r.AccumulatedProtocolFeesSF, r.AccumulatedReferrerFeesSF, r.PendingReferrerFeesSF} {
		value := fee.Int()
		if total.Cmp(value) < 0 {
			return nil, errors.New("Kamino reserve total liquidity underflowed fees")
		}
		total.Sub(total, value)
	}
	return total, nil
}

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

func CollateralToLiquidity(totalLiquiditySF *big.Int, collateralSupply, collateral uint64) (uint64, error) {
	if collateral == 0 {
		return 0, nil
	}
	if totalLiquiditySF == nil || totalLiquiditySF.Sign() <= 0 || collateralSupply == 0 {
		return 0, errors.New("Kamino collateral exchange rate is unavailable")
	}
	liquidity := new(big.Int).Mul(new(big.Int).SetUint64(collateral), totalLiquiditySF)
	liquidity.Quo(liquidity, new(big.Int).Lsh(new(big.Int).SetUint64(collateralSupply), FractionBits))
	if !liquidity.IsUint64() {
		return 0, errors.New("Kamino redeemable collateral exceeds u64")
	}
	return liquidity.Uint64(), nil
}

// FloorCollateralMinted is the collateral a deposit of liquidity mints at the
// reserve's current rate, floored. The rate only grows, so against a later
// read it never exceeds what was minted.
func (r Reserve) FloorCollateralMinted(liquidity uint64) (uint64, error) {
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
	collateral := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), new(big.Int).Lsh(new(big.Int).SetUint64(r.CollateralMintTotalSupply), FractionBits))
	collateral.Quo(collateral, total)
	if !collateral.IsUint64() {
		return 0, errors.New("Kamino minted collateral exceeds u64")
	}
	return collateral.Uint64(), nil
}

func (r Reserve) MinimumDeposit() (uint64, error) {
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return 0, err
	}
	if total.Sign() == 0 || r.CollateralMintTotalSupply == 0 {
		return 1, nil
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(r.CollateralMintTotalSupply), FractionBits)
	amount := new(big.Int).Add(total, denominator)
	amount.Sub(amount, big.NewInt(1)).Quo(amount, denominator)
	if !amount.IsUint64() || amount.Sign() <= 0 {
		return 0, errors.New("minimum Kamino deposit exceeds positive u64")
	}
	return amount.Uint64(), nil
}

type ObligationCollateral struct {
	Reserve         solana.PublicKey
	DepositedAmount uint64
}

// ObligationLiquidity is one borrow slot; an empty slot has a zero reserve.
// LastBorrowedAtTimestamp is the unix time of the slot's last borrow (or
// same-reserve rollover), which starts its debt term again for the whole
// slot; zero on a slot KLend has not tracked. BorrowedAmountAtExpiration is
// the debt KLend captured once the term ended, zero before.
type ObligationLiquidity struct {
	Reserve                    solana.PublicKey
	CumulativeBorrowRateBSF    BigScaledFraction
	LastBorrowedAtTimestamp    uint64
	BorrowedAmountSF           ScaledFraction
	BorrowedAmountAtExpiration uint64
}

func (b ObligationLiquidity) DebtTermEnd(reserve Reserve) (uint64, bool) {
	if reserve.DebtTermSeconds == 0 || b.LastBorrowedAtTimestamp == 0 {
		return 0, false
	}
	return b.LastBorrowedAtTimestamp + reserve.DebtTermSeconds, true
}

type Obligation struct {
	LastUpdateSlot         uint64
	LendingMarket, Owner   solana.PublicKey
	Deposits               [8]ObligationCollateral
	Borrows                [5]ObligationLiquidity
	UnhealthyBorrowValueSF ScaledFraction
	// The values of its last refresh: its deposits, its debt adjusted by the
	// borrow factors and not, and what it may borrow against its deposits
	// (each deposit's LTV, or its elevation group's).
	DepositedValueSF, BorrowFactorAdjustedDebtValueSF, BorrowedAssetsMarketValueSF, AllowedBorrowValueSF ScaledFraction
	ElevationGroup                                                                                       uint8
}

const obligationOwnerOffset = 64

func DecodeObligation(a *chain.Account) (Obligation, error) {
	if err := envelope(a, ProgramID, ObligationSize, ObligationDiscriminator, "obligation"); err != nil {
		return Obligation{}, err
	}
	d := a.Data
	o := Obligation{
		LastUpdateSlot: u64(d, 16), LendingMarket: key(d, 32), Owner: key(d, obligationOwnerOffset),
		UnhealthyBorrowValueSF: ScaledFraction(d[2256:2272]), ElevationGroup: d[2285], DepositedValueSF: ScaledFraction(d[1192:1208]),
		BorrowFactorAdjustedDebtValueSF: ScaledFraction(d[2208:2224]), BorrowedAssetsMarketValueSF: ScaledFraction(d[2224:2240]), AllowedBorrowValueSF: ScaledFraction(d[2240:2256]),
	}
	for i := range o.Deposits {
		offset := 96 + i*136
		o.Deposits[i] = ObligationCollateral{key(d, offset), u64(d, offset+32)}
	}
	for i := range o.Borrows {
		offset := 1208 + i*200
		o.Borrows[i] = ObligationLiquidity{Reserve: key(d, offset), CumulativeBorrowRateBSF: BigScaledFraction(d[offset+32 : offset+64]),
			LastBorrowedAtTimestamp: u64(d, offset+80), BorrowedAmountSF: ScaledFraction(d[offset+88 : offset+104]),
			BorrowedAmountAtExpiration: u64(d, offset+160)}
	}
	return o, nil
}

func (o Obligation) Collateral(reserve solana.PublicKey) uint64 {
	for _, deposit := range o.Deposits {
		if deposit.Reserve == reserve {
			return deposit.DepositedAmount
		}
	}
	return 0
}

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

func DecodeLendingMarketEmergencyMode(a *chain.Account) (bool, error) {
	if err := envelope(a, ProgramID, LendingMarketSize, LendingMarketDiscriminator, "lending market"); err != nil {
		return false, err
	}
	return a.Data[122] != 0, nil
}

type UserMetadata struct {
	Referrer, Owner solana.PublicKey
}

func DecodeUserMetadata(a *chain.Account) (UserMetadata, error) {
	if err := envelope(a, ProgramID, UserMetadataSize, UserMetadataDiscriminator, "user metadata"); err != nil {
		return UserMetadata{}, err
	}
	return UserMetadata{Referrer: key(a.Data, 8), Owner: key(a.Data, 80)}, nil
}

type FarmUserState struct {
	FarmState, Owner, Delegatee solana.PublicKey
	IsFarmDelegated             bool
}

func DecodeFarmUserState(a *chain.Account) (FarmUserState, error) {
	if err := envelope(a, FarmsProgramID, FarmUserStateSize, FarmUserStateDiscriminator, "farm user state"); err != nil {
		return FarmUserState{}, err
	}
	d := a.Data
	return FarmUserState{FarmState: key(d, 16), Owner: key(d, 48), IsFarmDelegated: d[80] == 1, Delegatee: key(d, 480)}, nil
}

type FarmState struct {
	DelegateAuthority             solana.PublicKey
	IsFarmFrozen, IsFarmDelegated bool
}

func DecodeFarmState(a *chain.Account) (FarmState, error) {
	if err := envelope(a, FarmsProgramID, FarmStateSize, FarmStateDiscriminator, "farm state"); err != nil {
		return FarmState{}, err
	}
	d := a.Data
	return FarmState{DelegateAuthority: key(d, 7328), IsFarmFrozen: d[7361] != 0, IsFarmDelegated: d[7362] == 1}, nil
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

// nullOracle is KLend's NULL_PUBKEY, which a reserve's oracle configuration
// holds, like the default key, for an oracle it does not use.
var nullOracle = solana.MustPublicKeyFromBase58("nu11111111111111111111111111111111111111111")

// oracle is the oracle account configured at offset, the zero key when
// KLend's maybe_null_pk reads none there: refresh_reserve then takes no
// account in its slot, and refuses one (InvalidPythPriceAccount and the like).
func oracle(data []byte, offset int) solana.PublicKey {
	if k := key(data, offset); k != nullOracle {
		return k
	}
	return solana.PublicKey{}
}

func withdrawalCap(d []byte) WithdrawalCap {
	return WithdrawalCap{int64(u64(d, 0)), int64(u64(d, 8)), u64(d, 16), u64(d, 24)}
}
