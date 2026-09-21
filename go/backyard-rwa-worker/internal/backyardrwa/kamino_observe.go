package backyardrwa

// This file deliberately decodes only the frozen PRIME/USDC Kamino graph. It
// is not a general KLend decoder: an unfamiliar account layout or topology is
// an observation failure, not an opportunity to guess.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"
)

const (
	kaminoProgram             = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	kaminoMarket              = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	kaminoPrimeUSDCObligation = "9suFBUhW7D7jN141mKR49Hn1WYDHEsRnPiGhxxm7RFkv"
	kaminoCollateralReserve   = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
	kaminoDebtReserve         = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"
	kaminoPrimeMint           = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	kaminoUSDCMint            = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	kaminoObligationLength    = 3344
	kaminoReserveLength       = 8624
	kaminoRequiredPriceStatus = 0x3f
	kaminoReserveConfigOffset = 4856

	// LendingMarket: 8-byte discriminator + size_of::<LendingMarket>() == 4656
	// in the pinned klend-interface 23b9f2b. emergency_mode follows version,
	// bump_seed, the two owner keys, quote_currency, and referral_fee_bps.
	kaminoMarketLength              = 4664
	kaminoMarketEmergencyModeOffset = 122

	// ReserveConfig.status (0 = Active) sits at the config base, and its
	// emergency_mode follows status, asset_tier padding, host_fixed_interest_
	// rate_bps, min_deleveraging_bonus_bps, block_ctoken_usage, and early_
	// repay_remaining_interest_pct. liquidation_threshold_pct at +17 is the
	// already-decoded cross-check for this base.
	kaminoReserveStatusOffset        = kaminoReserveConfigOffset
	kaminoReserveEmergencyModeOffset = kaminoReserveConfigOffset + 8
	// ReserveLiquidity.market_price_last_updated_ts: i64 unix seconds between
	// market_price_sf (248) and mint_decimals (272, already decoded as u64).
	kaminoMarketPriceLastUpdatedTSOffset = 264

	// Audit U5 / monitor M5: a reserve that has not refreshed within the
	// adaptor's report window, a paused reserve, or a stale oracle price keeps
	// reporting the last valuation. Every report-bearing action fails closed.
	kaminoMaxReserveAgeSlots = int64(32)
	kaminoOracleMaxAgeSecs   = int64(300)
)

var (
	kaminoObligationDiscriminator = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
	kaminoReserveDiscriminator    = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}
	// Verified against the live mainnet market account bytes as well as the
	// pinned klend-interface layout.
	kaminoMarketDiscriminator = [8]byte{246, 114, 50, 98, 72, 157, 28, 120}
)

// Fail-closed reserve health violations are sentinel errors so the observers
// can turn them into HOLD_MANUAL_RECOVERY decisions with the contract's
// reasons instead of a failed observation tick.
var (
	errKaminoReserveStale    = errors.New("kamino_stale")
	errKaminoReservePaused   = errors.New("reserve_paused")
	errKaminoOracleStale     = errors.New("kamino_oracle_stale")
	errKaminoMarketEmergency = errors.New("kamino_market_emergency")
)

// kaminoOracleSentinelPubkey is the pinned klend-sdk's documented NULL_PUBKEY
// (tools/backyard-voltr/node_modules/@kamino-finance/klend-sdk/src/utils/
// pubkey.ts): the value installed reserves write into UNUSED oracle config
// slots. keyString already maps all-zero keys to "", so the SDK's
// DEFAULT_PUBLIC_KEY never surfaces here; this sentinel is a real non-zero
// pubkey and did reach the oracle account fetch as a required account. It is
// filtered ONLY at the reserve oracle-key extraction below — no other
// account selection changes, and a reserve with no remaining configured
// oracle still fails "Kamino reserve has no configured oracle".
const kaminoOracleSentinelPubkey = "nu11111111111111111111111111111111111111111"

// kaminoHealthReason maps a reserve-health sentinel to its HOLD reason.
func kaminoHealthReason(err error) (string, bool) {
	switch {
	case errors.Is(err, errKaminoReserveStale), errors.Is(err, errKaminoOracleStale):
		return "kamino_stale", true
	case errors.Is(err, errKaminoReservePaused), errors.Is(err, errKaminoMarketEmergency):
		return "reserve_paused", true
	}
	return "", false
}

// KaminoHealthHoldObservation converts one fail-closed reserve-health
// violation into a HOLD_MANUAL_RECOVERY observation. Any other error is not a
// reserve-health verdict and is returned unchanged.
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

func pinnedKaminoObservationConfig() (KaminoObservationConfig, error) {
	return KaminoObservationConfig{
		Program: kaminoProgram, Market: kaminoMarket, Obligation: kaminoPrimeUSDCObligation,
		CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve,
		Vault: bridgeVault, MarketAuthority: kaminoPrimeMarketAuthority, CollateralMint: kaminoPrimeMint, DebtMint: kaminoUSDCMint,
	}, nil
}

type KaminoPosition struct {
	Slot, RefreshedSlot int64
	HasPosition         bool
	// ObligationPresent is false only when the observer saw the lane's
	// obligation account absent from its confirmed batch. A flat decode of a
	// missing account must never be mistaken for an observed flat position.
	ObligationPresent        bool
	CollateralDepositedRaw   uint64
	DebtRaw                  uint64
	RedeemablePrimeRaw       uint64
	CollateralPriceSF        [16]byte
	DebtPriceSF              [16]byte
	CollateralDecimals       uint8
	DebtDecimals             uint8
	LiquidationThresholdBPS  int64
	EntryCapacityRaw         uint64
	BorrowUtilizationBlocked bool
	Oracles                  []string
}

// ObserveKaminoPrimeUSDC reads the obligation, both reserves, and every
// configured oracle at confirmed commitment. Two RPC batches are necessary
// because oracle identities are encoded in the reserve. Their context slots
// must therefore match exactly; retries advance minContextSlot rather than
// mixing a newer oracle with older reserve bytes.
func (c *RPCClient) ObserveKaminoPrimeUSDC(ctx context.Context) (KaminoPosition, error) {
	if c == nil {
		return KaminoPosition{}, fmt.Errorf("RPC client is required")
	}
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		return KaminoPosition{}, err
	}
	return c.observeKaminoPrimeUSDC(ctx, config)
}

func (c *RPCClient) observeKaminoPrimeUSDC(ctx context.Context, config KaminoObservationConfig) (KaminoPosition, error) {
	minSlot, err := c.ConfirmedSlot(ctx)
	if err != nil {
		return KaminoPosition{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		baseSlot, accounts, err := c.GetMultipleAccounts(ctx, []string{config.Obligation, config.CollateralReserve, config.DebtReserve, config.Market}, minSlot)
		if err != nil {
			return KaminoPosition{}, err
		}
		obligation, err := decodeKaminoObligation(accountAt(accounts, config.Obligation), config)
		if err != nil {
			return KaminoPosition{}, err
		}
		collateral, err := decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
		if err != nil {
			return KaminoPosition{}, err
		}
		debt, err := decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
		if err != nil {
			return KaminoPosition{}, err
		}
		marketEmergency, err := decodeKaminoMarketEmergency(accountAt(accounts, config.Market), config)
		if err != nil {
			return KaminoPosition{}, err
		}
		if err := validateKaminoReserveHealth(baseSlot, marketEmergency, obligation, collateral, debt); err != nil {
			return KaminoPosition{}, err
		}
		oracles := uniqueNonzero(append(collateral.oracles, debt.oracles...))
		if len(oracles) == 0 {
			return KaminoPosition{}, fmt.Errorf("Kamino reserve has no configured oracle")
		}
		oracleSlot, oracleAccounts, err := c.GetMultipleAccounts(ctx, oracles, baseSlot)
		if err != nil {
			return KaminoPosition{}, err
		}
		if oracleSlot != baseSlot {
			minSlot = maxSlot(baseSlot, oracleSlot)
			continue
		}
		for _, oracle := range oracleAccounts {
			if oracle.Executable || oracle.Lamports == 0 || len(oracle.Data) == 0 {
				return KaminoPosition{}, fmt.Errorf("invalid configured oracle %s", oracle.Address)
			}
		}
		redeemable, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
		if err != nil {
			return KaminoPosition{}, err
		}
		entryCapacityRaw, err := entryCapacityDebtRaw(collateral, debt)
		if err != nil {
			return KaminoPosition{}, err
		}
		borrowUtilizationBlocked, err := borrowingBlockedByUtilization(debt)
		if err != nil {
			return KaminoPosition{}, err
		}
		debtRaw, err := obligation.debtAtReserveRate(debt)
		if err != nil {
			return KaminoPosition{}, err
		}
		blockTime, err := c.ConfirmedBlockTime(ctx, baseSlot)
		if err != nil {
			return KaminoPosition{}, err
		}
		if err := validateKaminoOracleAge(blockTime, collateral, debt); err != nil {
			return KaminoPosition{}, err
		}
		return KaminoPosition{
			Slot: baseSlot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition,
			// decodeKaminoObligation refuses an absent obligation envelope, so a
			// returned position always observed the account on chain.
			ObligationPresent:      true,
			CollateralDepositedRaw: obligation.collateralDepositedRaw, DebtRaw: debtRaw,
			RedeemablePrimeRaw: redeemable, CollateralPriceSF: collateral.marketPriceSF,
			DebtPriceSF: debt.marketPriceSF, Oracles: oracles,
			CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals,
			LiquidationThresholdBPS:  int64(collateral.liquidationThresholdPct) * 100,
			EntryCapacityRaw:         entryCapacityRaw,
			BorrowUtilizationBlocked: borrowUtilizationBlocked,
		}, nil
	}
	return KaminoPosition{}, confirmedObservationUnavailable(fmt.Errorf("confirmed Kamino reserve and oracle reads did not align after %d attempts", maxConfirmedObservationAttempts))
}

type decodedKaminoObligation struct {
	refreshedSlot          int64
	stale, priceStatus     byte
	hasPosition            bool
	collateralDepositedRaw uint64
	debtRaw                uint64
	debtAmountSF           [16]byte
	cumulativeBorrowRate   [32]byte
}
type decodedKaminoReserve struct {
	refreshedSlot            int64
	stale, priceStatus       byte
	status, emergencyMode    byte
	marketPriceLastUpdatedTS int64
	marketPriceSF            [16]byte
	mintDecimals             uint8
	oracles                  []string
	totalLiquiditySF         *big.Int
	collateralMintSupply     uint64
	liquidationThresholdPct  byte
	depositLimitRaw          uint64
	borrowLimitRaw           uint64
	borrowedRaw              uint64
	borrowedLiquiditySF      *big.Int
	utilizationLimitPct      byte
	cumulativeBorrowRate     [32]byte
}

func decodeKaminoObligation(account ConfirmedAccount, c KaminoObservationConfig) (decodedKaminoObligation, error) {
	if err := kaminoEnvelope(account, c.Obligation, kaminoObligationLength, kaminoObligationDiscriminator, c.Program); err != nil {
		return decodedKaminoObligation{}, err
	}
	if !sameKey(account.Data[32:64], c.Market) || !sameKey(account.Data[64:96], c.Vault) {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation market or owner drifted")
	}
	deposits, borrows := 0, 0
	var collateralRaw, debtRaw uint64
	var debtAmountSF [16]byte
	var cumulativeBorrowRate [32]byte
	for i := 0; i < 8; i++ {
		off := 96 + i*136
		amount := binary.LittleEndian.Uint64(account.Data[off+32 : off+40])
		if !zeroKey(account.Data[off:off+32]) && amount > 0 {
			if !sameKey(account.Data[off:off+32], c.CollateralReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino collateral reserve")
			}
			deposits++
			collateralRaw = amount
		}
	}
	for i := 0; i < 5; i++ {
		off := 1208 + i*200
		if !zeroKey(account.Data[off:off+32]) && !allZero(account.Data[off+88:off+104]) {
			if !sameKey(account.Data[off:off+32], c.DebtReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino debt reserve")
			}
			borrows++
			copy(debtAmountSF[:], account.Data[off+88:off+104])
			copy(cumulativeBorrowRate[:], account.Data[off+32:off+64])
			var err error
			debtRaw, err = ceilScaledFraction(account.Data[off+88 : off+104])
			if err != nil {
				return decodedKaminoObligation{}, err
			}
		}
	}
	if deposits > 1 || borrows > 1 {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation topology is unsupported")
	}
	hasPosition := deposits > 0 || borrows > 0
	return decodedKaminoObligation{
		refreshedSlot: int64(binary.LittleEndian.Uint64(account.Data[16:24])), stale: account.Data[24],
		priceStatus: account.Data[25], hasPosition: hasPosition,
		collateralDepositedRaw: collateralRaw, debtRaw: debtRaw,
		debtAmountSF: debtAmountSF, cumulativeBorrowRate: cumulativeBorrowRate,
	}, nil
}

func decodeKaminoReserve(account ConfirmedAccount, mint string, c KaminoObservationConfig) (decodedKaminoReserve, error) {
	if err := kaminoEnvelope(account, account.Address, kaminoReserveLength, kaminoReserveDiscriminator, c.Program); err != nil {
		return decodedKaminoReserve{}, err
	}
	if binary.LittleEndian.Uint64(account.Data[8:16]) != 1 || !sameKey(account.Data[32:64], c.Market) || !sameKey(account.Data[128:160], mint) {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino reserve identity drifted")
	}
	var price [16]byte
	copy(price[:], account.Data[248:264])
	var cumulativeBorrowRate [32]byte
	// ReserveLiquidity BigFractionBytes starts at 296; its four u64 value
	// limbs precede two padding limbs. Never interpret the padding as value.
	copy(cumulativeBorrowRate[:], account.Data[296:328])
	// ReserveLiquidity.mintDecimals is a u64 after marketPriceLastUpdatedTs.
	decimals := binary.LittleEndian.Uint64(account.Data[272:280])
	if decimals > 18 {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino mint decimals exceed supported scale")
	}
	// These offsets are derived from the pinned KLend 8624-byte Reserve layout:
	// ReserveConfig begins at 4856 and TokenInfo at 5032. Offset 645 is the
	// reviewed u8 utilization borrowing gate; the oracle keys below are the
	// only other config fields read. Curve and padding bytes remain ignored.
	var oracles []string
	for _, offset := range []int{5112, 5160, 5192, 5224} {
		// Drop ONLY the documented klend-sdk NULL_PUBKEY sentinel (see
		// kaminoOracleSentinelPubkey): an unused oracle slot is not a
		// configured oracle, and requesting it as an account can only fail
		// the observation. Every real configured oracle still passes.
		if key := keyString(account.Data[offset : offset+32]); key != kaminoOracleSentinelPubkey {
			oracles = append(oracles, key)
		}
	}
	borrowedLiquiditySF := littleInt(account.Data[232:248])
	totalLiquidity := new(big.Int).Set(borrowedLiquiditySF)
	totalLiquidity.Add(totalLiquidity, new(big.Int).Lsh(new(big.Int).SetUint64(binary.LittleEndian.Uint64(account.Data[224:232])), 60))
	for _, offset := range []int{344, 360, 376} {
		fee := littleInt(account.Data[offset : offset+16])
		if totalLiquidity.Cmp(fee) < 0 {
			return decodedKaminoReserve{}, fmt.Errorf("Kamino reserve total liquidity underflowed fees")
		}
		totalLiquidity.Sub(totalLiquidity, fee)
	}
	borrowedRaw, err := ceilScaledFraction(account.Data[232:248])
	if err != nil {
		return decodedKaminoReserve{}, err
	}
	return decodedKaminoReserve{
		refreshedSlot: int64(binary.LittleEndian.Uint64(account.Data[16:24])), stale: account.Data[24],
		priceStatus: account.Data[25], marketPriceSF: price, oracles: oracles,
		status:                   account.Data[kaminoReserveStatusOffset],
		emergencyMode:            account.Data[kaminoReserveEmergencyModeOffset],
		marketPriceLastUpdatedTS: int64(binary.LittleEndian.Uint64(account.Data[kaminoMarketPriceLastUpdatedTSOffset : kaminoMarketPriceLastUpdatedTSOffset+8])),
		mintDecimals:             uint8(decimals),
		totalLiquiditySF:         totalLiquidity, collateralMintSupply: binary.LittleEndian.Uint64(account.Data[2592:2600]),
		liquidationThresholdPct: account.Data[kaminoReserveConfigOffset+17],
		depositLimitRaw:         binary.LittleEndian.Uint64(account.Data[kaminoReserveConfigOffset+160 : kaminoReserveConfigOffset+168]),
		borrowLimitRaw:          binary.LittleEndian.Uint64(account.Data[kaminoReserveConfigOffset+168 : kaminoReserveConfigOffset+176]),
		borrowedRaw:             borrowedRaw,
		borrowedLiquiditySF:     borrowedLiquiditySF,
		utilizationLimitPct:     account.Data[kaminoReserveConfigOffset+645],
		cumulativeBorrowRate:    cumulativeBorrowRate,
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
	// One 50%-LTV borrow and one redeposit consume 1.5x the initial
	// collateral value. Floor this bound and the debt headroom bound.
	byCollateral := new(big.Int).Mul(new(big.Int).SetUint64(remainingCollateralValue), big.NewInt(2))
	byCollateral.Quo(byCollateral, big.NewInt(3))
	remainingDebt := debt.borrowLimitRaw - debt.borrowedRaw
	utilizationHeadroom, err := utilizationBorrowHeadroomRaw(debt)
	if err != nil {
		return 0, err
	}
	if remainingDebt > utilizationHeadroom {
		remainingDebt = utilizationHeadroom
	}
	byDebt := new(big.Int).Mul(new(big.Int).SetUint64(remainingDebt), big.NewInt(2))
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
	integer := new(big.Int).Rsh(new(big.Int).Set(value), 60)
	if new(big.Int).And(new(big.Int).Set(value), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 60), big.NewInt(1))).Sign() > 0 {
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
	result := high<<4 | low>>60
	if low&((uint64(1)<<60)-1) != 0 {
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
	if collateralRaw == 0 {
		return 0, nil
	}
	if r.totalLiquiditySF == nil || r.totalLiquiditySF.Sign() <= 0 || r.collateralMintSupply == 0 {
		return 0, fmt.Errorf("Kamino collateral exchange rate is unavailable")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(collateralRaw), r.totalLiquiditySF)
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(r.collateralMintSupply), 60)
	result := numerator.Div(numerator, denominator)
	if !result.IsUint64() {
		return 0, fmt.Errorf("Kamino redeemable collateral exceeds u64")
	}
	return result.Uint64(), nil
}

func (p KaminoPosition) targetLTVBorrowRaw() (uint64, error) {
	if p.CollateralDepositedRaw == 0 || p.RedeemablePrimeRaw == 0 {
		return 0, fmt.Errorf("Kamino collateral value is unavailable")
	}
	return targetBorrowForCollateralRaw(p.RedeemablePrimeRaw, p.CollateralDecimals, p.DebtDecimals, p.CollateralPriceSF, p.DebtPriceSF)
}

// Shared integer sizing for an observed position or an explicitly hypothetical
// entry amount. Forecasting does not manufacture a position/account to use it.
func targetBorrowForCollateralRaw(underlying uint64, collateralDecimals, debtDecimals uint8, collateralPriceSF, debtPriceSF [16]byte) (uint64, error) {
	collateralPrice := littleInt(collateralPriceSF[:])
	debtPrice := littleInt(debtPriceSF[:])
	if collateralPrice.Sign() <= 0 || debtPrice.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino market price is zero")
	}
	if collateralDecimals > 18 || debtDecimals > 18 {
		return 0, fmt.Errorf("Kamino mint decimals exceed supported scale")
	}
	// Preserve the existing conservative KLend scaled-fraction rounding at
	// each step, replacing only the former implicit six-decimal scales.
	valueSF := new(big.Int).Mul(new(big.Int).SetUint64(underlying), collateralPrice)
	valueSF.Div(valueSF, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(collateralDecimals)), nil))
	valueSF.Mul(valueSF, big.NewInt(TargetLTVBPS))
	valueSF.Div(valueSF, big.NewInt(10_000))
	valueSF.Mul(valueSF, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(debtDecimals)), nil))
	raw := valueSF.Div(valueSF, debtPrice)
	if !raw.IsUint64() || raw.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino target-LTV borrow is outside u64")
	}
	return raw.Uint64(), nil
}

// decodeKaminoMarketEmergency reads the market's global emergency-mode flag.
// The account bytes are otherwise ignored: an unexpected length, owner, or
// discriminator is an observation failure, never an assumed healthy market.
func decodeKaminoMarketEmergency(account ConfirmedAccount, c KaminoObservationConfig) (bool, error) {
	if err := kaminoEnvelope(account, c.Market, kaminoMarketLength, kaminoMarketDiscriminator, c.Program); err != nil {
		return false, err
	}
	return account.Data[kaminoMarketEmergencyModeOffset] != 0, nil
}

// validateKaminoReserveHealth is the fail-closed gate behind monitor M5. On
// top of validateKaminoRefresh's relative freshness it requires, at the
// observation slot: every reserve refreshed within kaminoMaxReserveAgeSlots,
// ReserveConfig.status 0, and no reserve- or market-level emergency mode.
func validateKaminoReserveHealth(observedSlot int64, marketEmergencyMode bool, obligation decodedKaminoObligation, reserves ...decodedKaminoReserve) error {
	if marketEmergencyMode {
		return fmt.Errorf("Kamino market is in emergency mode: %w", errKaminoMarketEmergency)
	}
	for _, reserve := range reserves {
		if reserve.status != 0 || reserve.emergencyMode != 0 {
			return fmt.Errorf("Kamino reserve %d/%d is not active: %w",
				reserve.status, reserve.emergencyMode, errKaminoReservePaused)
		}
		if reserve.refreshedSlot > observedSlot || observedSlot-reserve.refreshedSlot > kaminoMaxReserveAgeSlots {
			return fmt.Errorf("Kamino reserve last refreshed at %d, observation slot %d: %w",
				reserve.refreshedSlot, observedSlot, errKaminoReserveStale)
		}
	}
	// The pre-existing relative coherency check stays subordinate: an
	// absolutely stale reserve is reported as kamino_stale even when it is also
	// older than its obligation.
	return validateKaminoRefresh(obligation, reserves...)
}

// validateKaminoOracleAge requires every reserve's market price to have been
// published within kaminoOracleMaxAgeSecs of the observed block time.
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
				return fmt.Errorf("Kamino reserve valuation predates the obligation")
			}
		}
	}
	return nil
}

// clockUnixTimestamp reads the Clock sysvar carried in the same confirmed
// batch as the reserves, so oracle freshness is judged on chain time from the
// observed context rather than the worker's wall clock.
func clockUnixTimestamp(accounts []ConfirmedAccount) int64 {
	clock := accountAt(accounts, budgetClockAddress)
	if len(clock.Data) < 40 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
}

func kaminoEnvelope(a ConfirmedAccount, address string, length int, discriminator [8]byte, program string) error {
	if a.Address != address || a.Owner != program || a.Executable || a.Lamports == 0 || len(a.Data) != length || !bytes.Equal(a.Data[:8], discriminator[:]) {
		return fmt.Errorf("Kamino account envelope or layout drifted")
	}
	return nil
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
func maxSlot(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
