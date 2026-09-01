package backyardrwa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
)

const strategyReceiptLength = 192

var strategyReceiptDiscriminator = [8]byte{51, 8, 192, 253, 115, 78, 112, 214}

// ConfirmedAccountReader is the complete transport boundary for route NAV.
// Production uses RPCClient; fixtures use an in-memory reader without changing
// valuation or account validation.
type ConfirmedAccountReader interface {
	ConfirmedSlot(context.Context) (int64, error)
	GetMultipleAccounts(context.Context, []string, int64) (int64, []ConfirmedAccount, error)
}

type StrategyReceipt struct {
	PositionValueRaw uint64
	LastUpdatedTS    uint64
}

type RouteNAVCustodies struct {
	VoltrIdleRaw, StrategyUSDCraw, SquadsUSDCraw, SquadsPRIMEraw uint64
}

type RouteNAVSnapshot struct {
	Slot                    int64
	Custodies               RouteNAVCustodies
	VaultIdleRaw            uint64
	StrategyNAVRaw          uint64
	TotalVaultNAVRaw        uint64
	PriorReportedNAVRaw     uint64
	PriorReportUpdatedTS    uint64
	PrimeIdleValueRaw       uint64
	PositionCollateralValue uint64
	PositionDebtValue       uint64
	SnapshotDigest          string
	Report                  BridgeReport
}

func pinnedRouteNAVAddresses() []string {
	return []string{
		bridgeStrategy,
		bridgeStrategyReceipt,
		bridgeIdleATA,
		bridgeStrategyATA,
		bridgeSquadsATA,
		kaminoPrimeCustody,
		kaminoPrimeUSDCObligation,
		kaminoCollateralReserve,
		kaminoDebtReserve,
	}
}

func selectRouteNAVAccounts(accounts []ConfirmedAccount) ([]ConfirmedAccount, error) {
	selected := make([]ConfirmedAccount, 0, len(pinnedRouteNAVAddresses()))
	for _, address := range pinnedRouteNAVAddresses() {
		account := accountAt(accounts, address)
		if account.Address == "" {
			return nil, fmt.Errorf("NAV account %s is absent", address)
		}
		selected = append(selected, account)
	}
	return selected, nil
}

func decodeStrategyReceipt(account ConfirmedAccount) (StrategyReceipt, error) {
	if account.Address != bridgeStrategyReceipt || account.Owner != bridgeVoltrProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) != strategyReceiptLength ||
		!bytes.Equal(account.Data[:8], strategyReceiptDiscriminator[:]) {
		return StrategyReceipt{}, fmt.Errorf("Voltr strategy receipt envelope or layout drifted")
	}
	if !sameKey(account.Data[8:40], bridgeVoltrVault) ||
		!sameKey(account.Data[40:72], bridgeStrategy) ||
		!sameKey(account.Data[72:104], bridgeAdaptorProgram) ||
		account.Data[120] != 1 || !allZero(account.Data[123:]) {
		return StrategyReceipt{}, fmt.Errorf("Voltr strategy receipt binding or reserved bytes drifted")
	}
	return StrategyReceipt{
		PositionValueRaw: binary.LittleEndian.Uint64(account.Data[104:112]),
		LastUpdatedTS:    binary.LittleEndian.Uint64(account.Data[112:120]),
	}, nil
}

func decodeRouteNAVCustodies(accounts []ConfirmedAccount) (RouteNAVCustodies, error) {
	idle, err := decodePinnedUSDC(accountAt(accounts, bridgeIdleATA), bridgeIdleAuthority)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Voltr idle custody: %w", err)
	}
	strategy, err := decodePinnedUSDC(accountAt(accounts, bridgeStrategyATA), bridgeStrategyAuth)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Voltr strategy custody: %w", err)
	}
	squadsUSDC, err := decodePinnedUSDC(accountAt(accounts, bridgeSquadsATA), bridgeVault)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Squads USDC custody: %w", err)
	}
	squadsPRIME, err := decodePinnedPrime(accountAt(accounts, kaminoPrimeCustody))
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Squads PRIME custody: %w", err)
	}
	return RouteNAVCustodies{
		VoltrIdleRaw: idle.Raw, StrategyUSDCraw: strategy.Raw,
		SquadsUSDCraw: squadsUSDC.Raw, SquadsPRIMEraw: squadsPRIME.Raw,
	}, nil
}

// valueInDebtRaw conservatively converts equal-decimal token raw units into
// debt-token raw units. Assets floor; liabilities ceil. big.Int keeps hostile
// reserve prices and balances from wrapping intermediate arithmetic.
func valueInDebtRaw(raw uint64, tokenPriceSF, debtPriceSF [16]byte, liability bool) (uint64, error) {
	tokenPrice, debtPrice := littleInt(tokenPriceSF[:]), littleInt(debtPriceSF[:])
	if tokenPrice.Sign() <= 0 || debtPrice.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino reserve market price is zero")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(raw), tokenPrice)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, debtPrice, remainder)
	if liability && remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() {
		return 0, fmt.Errorf("Kamino valuation exceeds u64")
	}
	return quotient.Uint64(), nil
}

func navInputFingerprint(slot int64, accounts []ConfirmedAccount, custodies RouteNAVCustodies) (string, error) {
	if slot <= 0 {
		return "", fmt.Errorf("NAV slot is invalid")
	}
	if len(accounts) != len(pinnedRouteNAVAddresses()) {
		return "", fmt.Errorf("NAV account namespace contains unsupported custody")
	}
	byAddress := make(map[string]ConfirmedAccount, len(accounts))
	for _, account := range accounts {
		if account.Address == "" {
			return "", fmt.Errorf("NAV contains an unidentified account")
		}
		if _, duplicate := byAddress[account.Address]; duplicate {
			return "", fmt.Errorf("NAV contains a duplicate account")
		}
		byAddress[account.Address] = account
	}
	parts := make([]string, 0, len(pinnedRouteNAVAddresses())+1)
	for _, address := range pinnedRouteNAVAddresses() {
		account, ok := byAddress[address]
		if !ok {
			return "", fmt.Errorf("NAV account %s is absent", address)
		}
		hash := sha256.Sum256(account.Data)
		parts = append(parts, fmt.Sprintf("%s:%s:%d:%t:%x", address, account.Owner, account.Lamports, account.Executable, hash[:]))
	}
	// For bridge construction, custody overrides describe the exact expected
	// poststate while reserve/obligation/config bytes remain the confirmed input.
	parts = append(parts, fmt.Sprintf("post:%d:%d:%d:%d", custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw, custodies.SquadsPRIMEraw))
	sort.Strings(parts)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", slot, strings.Join(parts, "|"))))
	return hex.EncodeToString(hash[:]), nil
}

// ComputeRouteNAV is a pure, fixed-route calculator over one coherent account
// batch. The optional custody override is used only for a transaction's exact
// expected poststate; all identity, receipt, reserve, and obligation bytes are
// still independently decoded from the confirmed batch.
func ComputeRouteNAV(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies) (RouteNAVSnapshot, error) {
	if slot <= 0 || !sha256Pattern.MatchString(manifest.SHA256) || manifest.PolicyCatalog.SHA256 == nil ||
		!sha256Pattern.MatchString(*manifest.PolicyCatalog.SHA256) {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV manifest or slot is invalid")
	}
	if len(accounts) != len(pinnedRouteNAVAddresses()) {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV account namespace contains unsupported custody")
	}
	if _, err := decodeObservedAdaptorConfig(accountAt(accounts, bridgeStrategy)); err != nil {
		return RouteNAVSnapshot{}, err
	}
	receipt, err := decodeStrategyReceipt(accountAt(accounts, bridgeStrategyReceipt))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if receipt.PositionValueRaw > bridgeMaxNAV {
		return RouteNAVSnapshot{}, fmt.Errorf("prior Voltr NAV state is incoherent")
	}
	custodies, err := decodeRouteNAVCustodies(accounts)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if override != nil {
		custodies = *override
	}

	kaminoConfig, err := pinnedKaminoObservationConfig()
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, kaminoConfig.Obligation), kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	collateralReserve, err := decodeKaminoReserve(accountAt(accounts, kaminoConfig.CollateralReserve), kaminoConfig.CollateralMint, kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	debtReserve, err := decodeKaminoReserve(accountAt(accounts, kaminoConfig.DebtReserve), kaminoConfig.DebtMint, kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if err := validateKaminoRefresh(obligation, collateralReserve, debtReserve); err != nil {
		return RouteNAVSnapshot{}, err
	}
	if collateralReserve.refreshedSlot > slot || debtReserve.refreshedSlot > slot || obligation.refreshedSlot > slot {
		return RouteNAVSnapshot{}, fmt.Errorf("Kamino valuation claims a future refresh slot")
	}
	redeemablePRIME, err := collateralReserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	primeIdleValue, err := valueInDebtRaw(custodies.SquadsPRIMEraw, collateralReserve.marketPriceSF, debtReserve.marketPriceSF, false)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	collateralValue, err := valueInDebtRaw(redeemablePRIME, collateralReserve.marketPriceSF, debtReserve.marketPriceSF, false)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	debtValue, err := valueInDebtRaw(obligation.debtRaw, debtReserve.marketPriceSF, debtReserve.marketPriceSF, true)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	values := []uint64{custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw, primeIdleValue, collateralValue, debtValue}
	for _, value := range values {
		if value > math.MaxInt64 {
			return RouteNAVSnapshot{}, fmt.Errorf("NAV component exceeds signed range")
		}
	}
	fingerprint, err := navInputFingerprint(slot, accounts, custodies)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	nav, err := ComputeNAV(NAVSnapshotContext{
		Slot: slot, ReceiptFingerprint: fingerprint,
		ManifestSHA256: manifest.SHA256, PolicyCatalogSHA256: *manifest.PolicyCatalog.SHA256,
	}, []NAVComponent{
		{Account: bridgeStrategyATA, Owner: bridgeStrategyAuth, Raw: int64(custodies.StrategyUSDCraw), Slot: slot, Known: true},
		{Account: bridgeSquadsATA, Owner: bridgeVault, Raw: int64(custodies.SquadsUSDCraw), Slot: slot, Known: true},
		{Account: kaminoPrimeCustody, Owner: bridgeVault, Raw: int64(primeIdleValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":collateral", Owner: kaminoProgram, Raw: int64(collateralValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":debt", Owner: kaminoProgram, Raw: int64(debtValue), Slot: slot, Known: true, Liability: true},
	})
	if err != nil || nav.Raw < 0 {
		return RouteNAVSnapshot{}, fmt.Errorf("compute route NAV: %w", err)
	}
	if uint64(nav.Raw) > bridgeMaxNAV {
		return RouteNAVSnapshot{}, fmt.Errorf("route NAV exceeds adaptor bounds")
	}
	if custodies.VoltrIdleRaw > math.MaxUint64-uint64(nav.Raw) {
		return RouteNAVSnapshot{}, fmt.Errorf("total vault NAV overflows")
	}
	return RouteNAVSnapshot{
		Slot: slot, Custodies: custodies, VaultIdleRaw: custodies.VoltrIdleRaw, StrategyNAVRaw: uint64(nav.Raw),
		TotalVaultNAVRaw: custodies.VoltrIdleRaw + uint64(nav.Raw), PriorReportedNAVRaw: receipt.PositionValueRaw,
		PriorReportUpdatedTS: receipt.LastUpdatedTS,
		PrimeIdleValueRaw:    primeIdleValue, PositionCollateralValue: collateralValue, PositionDebtValue: debtValue,
		SnapshotDigest: nav.SnapshotDigest,
		Report:         BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: uint64(nav.Raw), SnapshotDigest: nav.SnapshotDigest},
	}, nil
}

// ObserveConfirmedRouteNAV obtains every known custody, receipt, adaptor, and
// PRIME/USDC account in one confirmed getMultipleAccounts context. It never
// merges values from independently observed slots.
func ObserveConfirmedRouteNAV(ctx context.Context, reader ConfirmedAccountReader, manifest RouteManifest) (RouteNAVSnapshot, error) {
	if reader == nil {
		return RouteNAVSnapshot{}, fmt.Errorf("confirmed account reader is required")
	}
	minimumSlot, err := reader.ConfirmedSlot(ctx)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	addresses := pinnedRouteNAVAddresses()
	slot, accounts, err := reader.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if slot < minimumSlot {
		return RouteNAVSnapshot{}, fmt.Errorf("confirmed NAV response regressed below its minimum slot")
	}
	return ComputeRouteNAV(slot, accounts, manifest, nil)
}
