package backyard

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const (
	kaminoRequiredPriceStatus = 0x3f
	kaminoReserveConfigOffset = 4856

	kaminoOracleMaxAgeSecs = int64(300)
)

var (
	errKaminoReserveStale    = errors.New("kamino_stale")
	errKaminoReservePaused   = errors.New("reserve_paused")
	errKaminoOracleStale     = errors.New("kamino_oracle_stale")
	errKaminoMarketEmergency = errors.New("kamino_market_emergency")
)

func kaminoHealthReason(err error) (string, bool) {
	switch {
	case errors.Is(err, errKaminoReserveStale), errors.Is(err, errKaminoOracleStale):
		return "kamino_stale", true
	case errors.Is(err, errKaminoReservePaused), errors.Is(err, errKaminoMarketEmergency):
		return "reserve_paused", true
	}
	return "", false
}

func KaminoHealthHoldObservation(err error, slot int64, now time.Time) (Observation, bool) {
	reason, known := kaminoHealthReason(err)
	if !known {
		return Observation{}, false
	}
	return Observation{ObservedAt: now, Snapshot: Snapshot{
		ObservationID: "kamino-health:" + reason, Slot: slot, RouteKind: RouteKind,
		ManualReason: reason,
	}}, true
}

type KaminoObservationConfig struct {
	Program, Market, Obligation, CollateralReserve, DebtReserve string
	Vault, MarketAuthority, CollateralMint, DebtMint            string
}

type KaminoPosition struct {
	Slot, RefreshedSlot     int64
	HasPosition             bool
	Obligation              obligationPresence
	CollateralDepositedRaw  uint64
	DebtRaw                 uint64
	RedeemablePrimeRaw      uint64
	CollateralPriceSF       [16]byte
	DebtPriceSF             [16]byte
	CollateralDecimals      uint8
	DebtDecimals            uint8
	LiquidationThresholdBPS int64
	// MaxLTVBPS is the obligation's borrow limit (kaminoBorrowLimitBPS):
	// the most KLend lets it borrow, as an LTV. Before a deposit it is the
	// collateral reserve's LTV, the limit a cross-mode obligation's first
	// deposit gets.
	MaxLTVBPS                int64
	EntryCapacityRaw         uint64
	BorrowUtilizationBlocked bool
	Oracles                  []string
}

func (p KaminoPosition) priced() bool {
	return p.LiquidationThresholdBPS > 0 && littleInt(p.CollateralPriceSF[:]).Sign() > 0
}

type decodedKaminoObligation struct {
	refreshedSlot          int64
	hasPosition            bool
	collateralDepositedRaw uint64
	debtRaw                uint64
	debtAmountSF           [16]byte
	cumulativeBorrowRate   [32]byte
	debtBorrow             kamino.ObligationLiquidity
	// borrowLimitBPS is kaminoBorrowLimitBPS; zero before a deposit.
	borrowLimitBPS int64
	elevationGroup uint8
}
type decodedKaminoReserve struct {
	refreshedSlot            int64
	status                   byte
	emergencyMode            bool
	marketPriceLastUpdatedTS int64
	marketPriceSF            [16]byte
	mintDecimals             uint8
	oracles                  []string
	totalLiquiditySF         *big.Int
	collateralMintSupply     uint64
	liquidationThresholdPct  byte
	loanToValuePct           byte
	depositLimitRaw          uint64
	borrowLimitRaw           uint64
	borrowedRaw              uint64
	borrowedLiquiditySF      *big.Int
	utilizationLimitPct      byte
	cumulativeBorrowRate     [32]byte
	// terms is the decoded reserve, whose interest terms
	// (SupportedInterestTerms) and debt term (DebtMaturityTimestamp,
	// DebtTermSeconds) kamino_term.go reads.
	terms kamino.Reserve
}

func decodeKaminoObligation(account ConfirmedAccount, c KaminoObservationConfig) (decodedKaminoObligation, error) {
	obligation, err := kamino.DecodeObligation(chainAccount(account, c.Obligation))
	if err != nil {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino account envelope or layout drifted")
	}
	if !sameKey(obligation.LendingMarket[:], c.Market) || !sameKey(obligation.Owner[:], c.Vault) {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation market or owner drifted")
	}
	deposits, borrows := 0, 0
	var collateralRaw, debtRaw uint64
	var debtAmountSF [16]byte
	var cumulativeBorrowRate [32]byte
	var debtBorrow kamino.ObligationLiquidity
	for _, deposit := range obligation.Deposits {
		if !deposit.Reserve.IsZero() && deposit.DepositedAmount > 0 {
			if !sameKey(deposit.Reserve[:], c.CollateralReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino collateral reserve")
			}
			deposits++
			collateralRaw = deposit.DepositedAmount
		}
	}
	for _, borrow := range obligation.Borrows {
		if !borrow.Reserve.IsZero() && !allZero(borrow.BorrowedAmountSF[:]) {
			if !sameKey(borrow.Reserve[:], c.DebtReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino debt reserve")
			}
			borrows++
			debtAmountSF, cumulativeBorrowRate, debtBorrow = borrow.BorrowedAmountSF, borrow.CumulativeBorrowRateBSF, borrow
			if debtRaw, err = ceilScaledFraction(borrow.BorrowedAmountSF[:]); err != nil {
				return decodedKaminoObligation{}, err
			}
		}
	}
	if deposits > 1 || borrows > 1 {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation topology is unsupported")
	}
	limit, _ := kaminoBorrowLimitBPS(obligation)
	return decodedKaminoObligation{
		refreshedSlot: int64(obligation.LastUpdateSlot), hasPosition: deposits > 0 || borrows > 0,
		collateralDepositedRaw: collateralRaw, debtRaw: debtRaw,
		debtAmountSF: debtAmountSF, cumulativeBorrowRate: cumulativeBorrowRate, debtBorrow: debtBorrow,
		borrowLimitBPS: limit, elevationGroup: obligation.ElevationGroup,
	}, nil
}

// kaminoBorrowLimitBPS is the obligation's borrow limit as an LTV of plain
// debt, the number KLend enforces on a borrow and a withdrawal: its allowed
// borrow value (each deposit at its reserve's LTV, or its elevation group's)
// over its deposited value, read back to plain debt through its borrow
// factors (100% without debt). False before a deposit.
func kaminoBorrowLimitBPS(o kamino.Obligation) (int64, bool) {
	deposited, allowed := littleInt(o.DepositedValueSF[:]), littleInt(o.AllowedBorrowValueSF[:])
	adjusted, plain := littleInt(o.BorrowFactorAdjustedDebtValueSF[:]), littleInt(o.BorrowedAssetsMarketValueSF[:])
	if deposited.Sign() <= 0 || allowed.Sign() <= 0 {
		return 0, false
	}
	limit := new(big.Int).Mul(allowed, big.NewInt(10_000))
	if adjusted.Sign() > 0 && plain.Sign() > 0 {
		limit.Mul(limit, plain).Quo(limit, adjusted)
	}
	limit.Quo(limit, deposited)
	if !limit.IsInt64() {
		return 0, false
	}
	return limit.Int64(), true
}

func decodeKaminoReserve(account ConfirmedAccount, mint string, c KaminoObservationConfig) (decodedKaminoReserve, error) {
	reserve, err := kamino.DecodeReserve(chainAccount(account, account.Address))
	if err != nil {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino account envelope or layout drifted")
	}
	if !sameKey(reserve.LendingMarket[:], c.Market) || !sameKey(reserve.LiquidityMint[:], mint) {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino reserve identity drifted")
	}
	if reserve.MintDecimals > 18 {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino mint decimals exceed supported scale")
	}
	var oracles []string
	for _, oracle := range []solana.PublicKey{reserve.ScopePriceFeed, reserve.SwitchboardPriceAggregator, reserve.SwitchboardTWAPAggregator, reserve.PythPrice} {
		// kamino.DecodeReserve reads an unused slot (KLend's NULL_PUBKEY or
		// the default key) as the zero key, which keyString maps to "": an
		// unused slot is no configured oracle.
		oracles = append(oracles, keyString(oracle[:]))
	}
	totalLiquidity, err := reserve.TotalLiquiditySF()
	if err != nil {
		return decodedKaminoReserve{}, err
	}
	borrowedRaw, err := ceilScaledFraction(reserve.BorrowedAmountSF[:])
	if err != nil {
		return decodedKaminoReserve{}, err
	}
	return decodedKaminoReserve{
		refreshedSlot: int64(reserve.LastUpdateSlot), marketPriceSF: reserve.MarketPriceSF, oracles: oracles,
		status: reserve.Status, emergencyMode: reserve.EmergencyMode,
		marketPriceLastUpdatedTS: int64(reserve.MarketPriceLastUpdatedTS),
		mintDecimals:             uint8(reserve.MintDecimals),
		totalLiquiditySF:         totalLiquidity, collateralMintSupply: reserve.CollateralMintTotalSupply,
		liquidationThresholdPct: reserve.LiquidationThresholdPct,
		loanToValuePct:          reserve.LoanToValuePct,
		depositLimitRaw:         reserve.DepositLimit,
		borrowLimitRaw:          reserve.BorrowLimit,
		borrowedRaw:             borrowedRaw,
		borrowedLiquiditySF:     kamino.U128(reserve.BorrowedAmountSF),
		utilizationLimitPct:     reserve.UtilizationLimitBlockBorrowingAbovePct,
		cumulativeBorrowRate:    reserve.CumulativeBorrowRateBSF,
		terms:                   reserve,
	}, nil
}

// Match KLend ObligationLiquidity::accrue_interest: multiply the unrounded
// stored Fraction by the reserve/obligation U256 cumulative-rate ratio, floor
// at the scaled-fraction boundary, then ceil once into executable raw debt.
// This is debt at the captured reserve refresh, NOT a future interest bound.
// Admission must separately reserve interest through its execution horizon.
func (o decodedKaminoObligation) debtAtReserveRate(r decodedKaminoReserve) (uint64, error) {
	amount := littleInt(o.debtAmountSF[:])
	if amount.Sign() == 0 {
		if o.debtRaw != 0 {
			return 0, fmt.Errorf("Kamino stored debt fraction is missing")
		}
		return 0, nil
	}
	former, current := littleInt(o.cumulativeBorrowRate[:]), littleInt(r.cumulativeBorrowRate[:])
	if former.Sign() <= 0 || current.Cmp(former) < 0 {
		return 0, fmt.Errorf("Kamino cumulative borrow rate is zero or regressed")
	}
	if current.Cmp(former) == 0 {
		return ceilScaledBigFraction(amount)
	}
	product := new(big.Int).Mul(amount, current)
	if product.BitLen() > 256 {
		return 0, fmt.Errorf("Kamino interest multiplication exceeds u256")
	}
	accrued := product.Quo(product, former)
	return ceilScaledBigFraction(accrued)
}

const entryLoopLTVPercent = 50

func entryCapacityDebtRaw(collateral, debt decodedKaminoReserve) (uint64, error) {
	if collateral.totalLiquiditySF == nil || collateral.depositLimitRaw == 0 || debt.borrowLimitRaw == 0 {
		return 0, nil
	}
	depositedRaw, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil {
		return 0, err
	}
	if depositedRaw >= collateral.depositLimitRaw || debt.borrowedRaw >= debt.borrowLimitRaw {
		return 0, nil
	}
	remainingCollateral := collateral.depositLimitRaw - depositedRaw
	remainingCollateralValue, err := valueBetweenTokenRaw(remainingCollateral, collateral.mintDecimals, debt.mintDecimals, collateral.marketPriceSF, debt.marketPriceSF, false)
	if err != nil {
		return 0, err
	}
	byCollateral := new(big.Int).Mul(new(big.Int).SetUint64(remainingCollateralValue), big.NewInt(100))
	byCollateral.Quo(byCollateral, big.NewInt(100+entryLoopLTVPercent))
	remainingDebt := debt.borrowLimitRaw - debt.borrowedRaw
	utilizationHeadroom, err := utilizationBorrowHeadroomRaw(debt)
	if err != nil {
		return 0, err
	}
	if remainingDebt > utilizationHeadroom {
		remainingDebt = utilizationHeadroom
	}
	byDebt := new(big.Int).Mul(new(big.Int).SetUint64(remainingDebt), big.NewInt(100))
	byDebt.Quo(byDebt, big.NewInt(entryLoopLTVPercent))
	if byCollateral.Cmp(byDebt) > 0 {
		byCollateral.Set(byDebt)
	}
	if !byCollateral.IsUint64() {
		return 0, fmt.Errorf("Kamino entry capacity exceeds u64")
	}
	return byCollateral.Uint64(), nil
}

func borrowingBlockedByUtilization(debt decodedKaminoReserve) (bool, error) {
	if debt.utilizationLimitPct == 0 {
		return false, nil
	}
	headroom, err := utilizationBorrowHeadroomRaw(debt)
	return headroom == 0, err
}

// utilizationBorrowHeadroomRaw mirrors KLend's configured utilization gate:
// floor(total_supply * pct / 100 - total_borrow - one fixed-point ulp).
// A zero pct disables this gate. Returning zero deliberately defers every new
// positive borrow at or beyond the boundary while leaving repayments intact.
func utilizationBorrowHeadroomRaw(debt decodedKaminoReserve) (uint64, error) {
	if debt.utilizationLimitPct == 0 {
		return math.MaxUint64, nil
	}
	if debt.utilizationLimitPct > 100 || debt.totalLiquiditySF == nil || debt.borrowedLiquiditySF == nil ||
		debt.totalLiquiditySF.Sign() < 0 || debt.borrowedLiquiditySF.Sign() < 0 {
		return 0, fmt.Errorf("invalid Kamino utilization boundary")
	}
	limitSF := new(big.Int).Mul(new(big.Int).Set(debt.totalLiquiditySF),
		new(big.Int).SetUint64(uint64(debt.utilizationLimitPct)))
	limitSF.Quo(limitSF, big.NewInt(100))
	if debt.borrowedLiquiditySF.Cmp(limitSF) >= 0 {
		return 0, nil
	}
	headroomSF := new(big.Int).Sub(limitSF, debt.borrowedLiquiditySF)
	headroomSF.Sub(headroomSF, big.NewInt(1))
	if headroomSF.Sign() <= 0 {
		return 0, nil
	}
	headroomRaw := headroomSF.Rsh(headroomSF, 60)
	if !headroomRaw.IsUint64() {
		return 0, fmt.Errorf("Kamino utilization headroom exceeds u64")
	}
	return headroomRaw.Uint64(), nil
}

func ceilScaledBigFraction(value *big.Int) (uint64, error) {
	if value == nil || value.Sign() < 0 || value.BitLen() > 128 {
		return 0, fmt.Errorf("Kamino scaled fraction exceeds u128")
	}
	integer := new(big.Int).Rsh(new(big.Int).Set(value), kamino.FractionBits)
	if new(big.Int).And(new(big.Int).Set(value), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), kamino.FractionBits), big.NewInt(1))).Sign() > 0 {
		integer.Add(integer, big.NewInt(1))
	}
	if !integer.IsUint64() {
		return 0, fmt.Errorf("Kamino scaled fraction exceeds u64")
	}
	return integer.Uint64(), nil
}

func ceilScaledFraction(value []byte) (uint64, error) {
	if len(value) != 16 {
		return 0, fmt.Errorf("invalid Kamino scaled fraction")
	}
	low, high := binary.LittleEndian.Uint64(value[:8]), binary.LittleEndian.Uint64(value[8:])
	if high > math.MaxUint64>>4 {
		return 0, fmt.Errorf("Kamino debt exceeds u64")
	}
	result := high<<4 | low>>kamino.FractionBits
	if low&((uint64(1)<<kamino.FractionBits)-1) != 0 {
		if result == math.MaxUint64 {
			return 0, fmt.Errorf("Kamino debt exceeds u64")
		}
		result++
	}
	return result, nil
}

func littleInt(value []byte) *big.Int {
	reversed := append([]byte(nil), value...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return new(big.Int).SetBytes(reversed)
}

func (r decodedKaminoReserve) redeemLiquidityRaw(collateralRaw uint64) (uint64, error) {
	return kamino.CollateralToLiquidity(r.totalLiquiditySF, r.collateralMintSupply, collateralRaw)
}

func (p KaminoPosition) collateralValueDebtRaw() (uint64, error) {
	if p.CollateralDepositedRaw == 0 || p.RedeemablePrimeRaw == 0 {
		return 0, fmt.Errorf("Kamino collateral value is unavailable")
	}
	collateralPrice := littleInt(p.CollateralPriceSF[:])
	debtPrice := littleInt(p.DebtPriceSF[:])
	if collateralPrice.Sign() <= 0 || debtPrice.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino market price is zero")
	}
	if p.CollateralDecimals > 18 || p.DebtDecimals > 18 {
		return 0, fmt.Errorf("Kamino mint decimals exceed supported scale")
	}
	valueSF := new(big.Int).Mul(new(big.Int).SetUint64(p.RedeemablePrimeRaw), collateralPrice)
	valueSF.Div(valueSF, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p.CollateralDecimals)), nil))
	valueSF.Mul(valueSF, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p.DebtDecimals)), nil))
	raw := valueSF.Div(valueSF, debtPrice)
	if !raw.IsUint64() || raw.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino collateral value is outside u64")
	}
	return raw.Uint64(), nil
}

func decodeKaminoMarketEmergency(account ConfirmedAccount, c KaminoObservationConfig) (bool, error) {
	return kamino.DecodeLendingMarketEmergencyMode(chainAccount(account, c.Market))
}

func validateKaminoReserveHealth(observedSlot int64, marketEmergencyMode bool, obligation decodedKaminoObligation, reserves ...decodedKaminoReserve) error {
	if marketEmergencyMode {
		return fmt.Errorf("Kamino market is in emergency mode: %w", errKaminoMarketEmergency)
	}
	for _, reserve := range reserves {
		if reserve.status != 0 || reserve.emergencyMode {
			return fmt.Errorf("Kamino reserve %d/%t is not active: %w",
				reserve.status, reserve.emergencyMode, errKaminoReservePaused)
		}
		if reserve.refreshedSlot > observedSlot || observedSlot-reserve.refreshedSlot > adaptorMaxReportAgeSlots {
			return fmt.Errorf("Kamino reserve last refreshed at %d, observation slot %d: %w",
				reserve.refreshedSlot, observedSlot, errKaminoReserveStale)
		}
	}
	return validateKaminoRefresh(obligation, reserves...)
}

func validateKaminoOracleAge(observedUnix int64, reserves ...decodedKaminoReserve) error {
	for _, reserve := range reserves {
		updated := reserve.marketPriceLastUpdatedTS
		if updated <= 0 || updated > observedUnix || observedUnix-updated > kaminoOracleMaxAgeSecs {
			return fmt.Errorf("Kamino oracle price last updated at %d, observed %d: %w",
				updated, observedUnix, errKaminoOracleStale)
		}
	}
	return nil
}

func validateKaminoRefresh(o decodedKaminoObligation, reserves ...decodedKaminoReserve) error {
	for _, r := range reserves {
		if r.refreshedSlot <= 0 {
			return fmt.Errorf("Kamino reserve valuation is stale, invalid, or incoherent")
		}
	}
	// Reserves are refreshed independently on mainnet, so their LastUpdate
	// slots are not expected to match. LastUpdate.stale means the next
	// transaction must refresh the account; it does not invalidate a read used
	// to build that refresh transaction. price_status is likewise not a
	// universal validity mask (the USDC reserve legitimately reports zero).
	// This mirrors the existing Rust Multiply observer: for a populated
	// obligation, both reserve views must be at least as new as the obligation.
	// Every dispatched Kamino action refreshes and simulates before persistence.
	if o.hasPosition {
		if o.refreshedSlot <= 0 {
			return fmt.Errorf("Kamino obligation valuation is stale, invalid, or incoherent")
		}
		for _, r := range reserves {
			if r.refreshedSlot < o.refreshedSlot {
				return fmt.Errorf("Kamino reserve valuation predates the obligation: %w", errKaminoReserveStale)
			}
		}
	}
	return nil
}

func clockUnixTimestamp(accounts []ConfirmedAccount) int64 {
	clock := accountAt(accounts, budgetClockAddress)
	if len(clock.Data) < 40 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
}

func sameKey(data []byte, address string) bool {
	want, err := decodeBase58PublicKey(address)
	return err == nil && bytes.Equal(data, want[:])
}
func zeroKey(data []byte) bool { return allZero(data) }
func keyString(data []byte) string {
	if zeroKey(data) {
		return ""
	}
	return encodeBase58(data)
}
func uniqueNonzero(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
