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

const (
	strategyReceiptLength = 192
	// voltrVaultMinimumLength is the prefix of Voltr's Vault account this worker
	// decodes. Offsets below are verified against the deployed 928-byte account
	// (scripts/decode_voltr_repair.py). Only the prefix is required so a
	// same-identity upgrade that appends fields still reaches the M6
	// program-identity monitor instead of dying inside account decoding.
	voltrVaultMinimumLength = 696
	voltrLPMintLength       = 82
)

var (
	strategyReceiptDiscriminator = [8]byte{51, 8, 192, 253, 115, 78, 112, 214}
	voltrVaultDiscriminator      = [8]byte{211, 8, 232, 43, 2, 152, 117, 119}
)

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
	// CustodyTrackedRaw is the strategy custody balance Voltr itself books in
	// the receipt's reserved bytes at offset 128 on the post-upgrade binary.
	// Voltr credits this balance into totalValue by itself, so an independent
	// NAV must never add it again.
	CustodyTrackedRaw uint64
}

// VoltrVaultBook is the vault book decoded independently of the adaptor and of
// this worker's own arithmetic. It is the only admissible comparison input for
// the M1 identity `totalValue == idle + custody + receipt.positionValue`.
type VoltrVaultBook struct {
	TotalValueRaw                  uint64
	LockedProfitDegradationSeconds uint64
	LastUpdatedLockedProfitRaw     uint64
	LastLockedProfitReportUnix     uint64
	ManagerPerformanceFeeBPS       uint64
	AdminPerformanceFeeBPS         uint64
	WithdrawalWaitingPeriodSeconds uint64
	FeeAccumulatorManagerRaw       uint64
	FeeAccumulatorAdminRaw         uint64
	FeeAccumulatorProtocolRaw      uint64
	LPSupplyDeadWeightRaw          uint64
}

// FeeAccumulatorRaw sums the un-harvested LP fee accumulators that
// get_total_lp_supply_incl_fees adds to the mint supply.
func (b VoltrVaultBook) FeeAccumulatorRaw() uint64 {
	return b.FeeAccumulatorManagerRaw + b.FeeAccumulatorAdminRaw + b.FeeAccumulatorProtocolRaw
}

// LPSupplyInclFeesRaw mirrors Voltr's SDK LP supply used to price deposits and
// claims. The dead-weight LP at offset 616 is minted but untracked supply.
func (b VoltrVaultBook) LPSupplyInclFeesRaw(lpSupplyRaw uint64) uint64 {
	return lpSupplyRaw + b.FeeAccumulatorRaw() + b.LPSupplyDeadWeightRaw
}

func decodeVoltrVaultBook(account ConfirmedAccount) (VoltrVaultBook, error) {
	if account.Address != bridgeVoltrVault || account.Owner != bridgeVoltrProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) < voltrVaultMinimumLength ||
		!bytes.Equal(account.Data[:8], voltrVaultDiscriminator[:]) {
		return VoltrVaultBook{}, fmt.Errorf("Voltr vault account envelope or layout drifted")
	}
	if !sameKey(account.Data[104:136], bridgeUSDC) || !sameKey(account.Data[136:168], bridgeIdleATA) ||
		!sameKey(account.Data[272:304], bridgeLPMint) || !sameKey(account.Data[368:400], bridgeVault) ||
		!sameKey(account.Data[400:432], bridgeSettingsSigner) {
		return VoltrVaultBook{}, fmt.Errorf("Voltr vault asset, LP, manager, or admin identity drifted")
	}
	return VoltrVaultBook{
		TotalValueRaw:                  binary.LittleEndian.Uint64(account.Data[168:176]),
		LockedProfitDegradationSeconds: binary.LittleEndian.Uint64(account.Data[448:456]),
		WithdrawalWaitingPeriodSeconds: binary.LittleEndian.Uint64(account.Data[456:464]),
		ManagerPerformanceFeeBPS:       uint64(binary.LittleEndian.Uint16(account.Data[512:514])),
		AdminPerformanceFeeBPS:         uint64(binary.LittleEndian.Uint16(account.Data[514:516])),
		FeeAccumulatorManagerRaw:       binary.LittleEndian.Uint64(account.Data[576:584]),
		FeeAccumulatorAdminRaw:         binary.LittleEndian.Uint64(account.Data[584:592]),
		FeeAccumulatorProtocolRaw:      binary.LittleEndian.Uint64(account.Data[592:600]),
		LPSupplyDeadWeightRaw:          binary.LittleEndian.Uint64(account.Data[616:624]),
		LastUpdatedLockedProfitRaw:     binary.LittleEndian.Uint64(account.Data[672:680]),
		LastLockedProfitReportUnix:     binary.LittleEndian.Uint64(account.Data[680:688]),
	}, nil
}

// decodeVoltrLPSupply reads only the mint supply of the vault LP asset. Fee
// LP that Voltr has accrued but not yet minted is added by VoltrVaultBook.
func decodeVoltrLPSupply(account ConfirmedAccount) (uint64, error) {
	if account.Address != bridgeLPMint || account.Owner != bridgeTokenProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) < voltrLPMintLength {
		return 0, fmt.Errorf("Voltr LP mint account envelope drifted")
	}
	return binary.LittleEndian.Uint64(account.Data[36:44]), nil
}

type RouteNAVCustodies struct {
	VoltrIdleRaw, StrategyUSDCraw, SquadsUSDCraw, SquadsPRIMEraw uint64
	// Separate from USDC; zero on USDC-debt lanes to avoid double counting.
	SquadsDebtRaw uint64
}

type RouteNAVSnapshot struct {
	Slot int64
	// Custodies reports every observed custody balance, including the strategy
	// custody ATA. StrategyCustody is deliberately not a NAV component: Voltr
	// books the strategy custody balance itself, so adding it here would count
	// it twice (the Sep 4 incident reported exactly that double count).
	Custodies               RouteNAVCustodies
	VaultIdleRaw            uint64
	StrategyNAVRaw          uint64
	TotalVaultNAVRaw        uint64
	PriorReportedNAVRaw     uint64
	PriorReportUpdatedTS    uint64
	PrimeIdleValueRaw       uint64
	DebtIdleValueRaw        uint64
	PositionCollateralValue uint64
	PositionDebtValue       uint64
	// ObligationPresent reports whether the obligation account existed in this
	// confirmed batch. Zero position values from a missing account are an
	// observed absence, never a silently decoded flat position.
	ObligationPresent bool
	Receipt           StrategyReceipt
	Voltr             VoltrVaultBook
	LPSupplyRaw       uint64
	SnapshotDigest    string
	Report            BridgeReport
}

func pinnedRouteNAVAddresses() []string {
	return pinnedRouteNAVAddressesForRoute(RuntimeRoute{Lane: RouteID, Kamino: KaminoObservationConfig{Obligation: kaminoPrimeUSDCObligation, CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve, Market: kaminoMarket, Program: kaminoProgram}, CollateralCustody: kaminoPrimeCustody})
}

func pinnedRouteNAVAddressesForRoute(route RuntimeRoute) []string {
	addresses := []string{
		bridgeStrategy,
		bridgeStrategyReceipt,
		bridgeIdleATA,
		bridgeStrategyATA,
		bridgeSquadsATA,
		route.CollateralCustody,
		route.Kamino.Obligation,
		route.Kamino.CollateralReserve,
		route.Kamino.DebtReserve,
		// The lending market is part of the valuation input: its emergency
		// mode pauses the whole position, so it is pinned, hashed into the
		// NAV fingerprint, and required in every batch.
		route.Kamino.Market,
		// The Voltr book and the LP mint are read in the same coherent batch so
		// the fail-closed monitors never compare values from different slots.
		bridgeVoltrVault,
		bridgeLPMint,
	}
	if route.Kamino.DebtMint != "" && route.Kamino.DebtMint != bridgeUSDC {
		addresses = append(addresses, route.DebtCustody, kaminoDebtReserve)
	}
	return addresses
}

func selectRouteNAVAccounts(accounts []ConfirmedAccount) ([]ConfirmedAccount, error) {
	return selectRouteNAVAccountsForRoute(accounts, RuntimeRoute{Lane: RouteID, Kamino: KaminoObservationConfig{Obligation: kaminoPrimeUSDCObligation, CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve, Market: kaminoMarket, Program: kaminoProgram}, CollateralCustody: kaminoPrimeCustody})
}

func selectRouteNAVAccountsForRoute(accounts []ConfirmedAccount, route RuntimeRoute) ([]ConfirmedAccount, error) {
	selected := make([]ConfirmedAccount, 0, len(pinnedRouteNAVAddressesForRoute(route)))
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
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
		account.Data[120] != 2 || !allZero(account.Data[123:128]) || !allZero(account.Data[136:]) {
		return StrategyReceipt{}, fmt.Errorf("Voltr strategy receipt binding or reserved bytes drifted")
	}
	// Fresh strategy-two receipts are version 2 on the pinned Voltr binary,
	// proven by the deployed-binary bootstrap in voltr_reset_sequence.
	// Offset 128 is reserved on the pre-upgrade binary and holds the custody
	// balance Voltr books for this strategy on the current one. It is decoded
	// and reported rather than required zero: a nonzero value must surface as a
	// custody monitor HOLD, not as an undecodable account.
	return StrategyReceipt{
		PositionValueRaw:  binary.LittleEndian.Uint64(account.Data[104:112]),
		LastUpdatedTS:     binary.LittleEndian.Uint64(account.Data[112:120]),
		CustodyTrackedRaw: binary.LittleEndian.Uint64(account.Data[128:136]),
	}, nil
}

// strategyReceiptIntegrityFault classifies a confirmed strategy receipt that
// cannot be the reviewed Voltr account at all: it is absent from the batch,
// owned by another program, or the wrong length. Those are observed integrity
// failures and become the durable strategy_receipt_integrity hold; malformed
// fields inside a correctly enveloped receipt stay decode errors, and
// transport failures never reach this classifier.
func strategyReceiptIntegrityFault(account ConfirmedAccount) bool {
	return account.Address == "" || account.Owner != bridgeVoltrProgram || len(account.Data) != strategyReceiptLength
}

// strategyReceiptAbsent separates a null account in the batch — which may be a
// replication artifact until the ledger finalizes past it — from a present
// account with a broken envelope. Only the finalized re-read may promote
// absence to an integrity fault.
func strategyReceiptAbsent(account ConfirmedAccount) bool {
	return account.Owner == "" && len(account.Data) == 0
}

func decodeRouteNAVCustodies(accounts []ConfirmedAccount) (RouteNAVCustodies, error) {
	route, err := runtimeRoute(RouteID)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("load pinned PRIME route: %w", err)
	}
	return decodeRouteNAVCustodiesForRoute(accounts, route)
}

func decodeRouteNAVCustodiesForRoute(accounts []ConfirmedAccount, route RuntimeRoute) (RouteNAVCustodies, error) {
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
	collateralMint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
	if err != nil {
		return RouteNAVCustodies{}, err
	}
	authority, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return RouteNAVCustodies{}, err
	}
	squadsPRIME, err := DecodeTokenCustody(accountAt(accounts, route.CollateralCustody).Owner, accountAt(accounts, route.CollateralCustody).Data, collateralMint, authority)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode route collateral custody: %w", err)
	}
	result := RouteNAVCustodies{
		VoltrIdleRaw: idle.Raw, StrategyUSDCraw: strategy.Raw,
		SquadsUSDCraw: squadsUSDC.Raw, SquadsPRIMEraw: squadsPRIME.Raw,
	}
	if route.Kamino.DebtMint != bridgeUSDC {
		if route.DebtCustody == "" || route.DebtCustody == bridgeSquadsATA || route.DebtCustody == route.CollateralCustody {
			return RouteNAVCustodies{}, fmt.Errorf("non-USDC debt custody is missing or aliased")
		}
		mint, err := decodeBase58PublicKey(route.Kamino.DebtMint)
		if err != nil {
			return RouteNAVCustodies{}, err
		}
		account := accountAt(accounts, route.DebtCustody)
		if account.Owner != route.DebtTokenProgram || account.Executable || account.Lamports == 0 {
			return RouteNAVCustodies{}, fmt.Errorf("debt custody token program or account envelope drifted")
		}
		debt, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
		if err != nil {
			return RouteNAVCustodies{}, fmt.Errorf("decode debt custody: %w", err)
		}
		result.SquadsDebtRaw = debt.Raw
	}
	return result, nil
}

// valueInDebtRaw conservatively converts equal-decimal token raw units into
// debt-token raw units. Assets floor; liabilities ceil. big.Int keeps hostile
// reserve prices and balances from wrapping intermediate arithmetic.
func valueInDebtRaw(raw uint64, tokenPriceSF, debtPriceSF [16]byte, liability bool) (uint64, error) {
	return valueBetweenTokenRaw(raw, 0, 0, tokenPriceSF, debtPriceSF, liability)
}

// valueBetweenTokenRaw converts raw units between assets without assuming
// matching decimals or a stablecoin peg. The two reserve prices use the same
// scaled-fraction quote denomination, which cancels exactly in their ratio.
func valueBetweenTokenRaw(raw uint64, tokenDecimals, debtDecimals uint8, tokenPriceSF, debtPriceSF [16]byte, liability bool) (uint64, error) {
	if tokenDecimals > 18 || debtDecimals > 18 {
		return 0, fmt.Errorf("unsupported token decimal scale")
	}
	tokenPrice, debtPrice := littleInt(tokenPriceSF[:]), littleInt(debtPriceSF[:])
	if tokenPrice.Sign() <= 0 || debtPrice.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino reserve market price is zero")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(raw), tokenPrice)
	numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(debtDecimals)), nil))
	denominator := new(big.Int).Mul(debtPrice, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(tokenDecimals)), nil))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if liability && remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() {
		return 0, fmt.Errorf("Kamino valuation exceeds u64")
	}
	return quotient.Uint64(), nil
}

func navInputFingerprint(slot int64, accounts []ConfirmedAccount, custodies RouteNAVCustodies) (string, error) {
	route := RuntimeRoute{Lane: RouteID, Kamino: KaminoObservationConfig{Obligation: kaminoPrimeUSDCObligation, CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve, Market: kaminoMarket, Program: kaminoProgram}, CollateralCustody: kaminoPrimeCustody}
	return navInputFingerprintForRoute(slot, accounts, custodies, route)
}

func navInputFingerprintForRoute(slot int64, accounts []ConfirmedAccount, custodies RouteNAVCustodies, route RuntimeRoute) (string, error) {
	if slot <= 0 {
		return "", fmt.Errorf("NAV slot is invalid")
	}
	if len(accounts) != len(pinnedRouteNAVAddressesForRoute(route)) {
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
	parts := make([]string, 0, len(pinnedRouteNAVAddressesForRoute(route))+1)
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
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
	if route.Kamino.DebtMint != "" && route.Kamino.DebtMint != bridgeUSDC {
		parts = append(parts, fmt.Sprintf("post-debt:%d", custodies.SquadsDebtRaw))
	}
	sort.Strings(parts)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", slot, strings.Join(parts, "|"))))
	return hex.EncodeToString(hash[:]), nil
}

// ComputeRouteNAV is a pure, fixed-route calculator over one coherent account
// batch. The optional custody override is used only for a transaction's exact
// expected poststate; all identity, receipt, reserve, and obligation bytes are
// still independently decoded from the confirmed batch.
func ComputeRouteNAV(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies) (RouteNAVSnapshot, error) {
	route := RuntimeRoute{Lane: RouteID, Kamino: KaminoObservationConfig{Obligation: kaminoPrimeUSDCObligation, CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve, CollateralMint: kaminoPrimeMint, DebtMint: kaminoUSDCMint, Market: kaminoMarket, Program: kaminoProgram, Vault: bridgeVault}, CollateralCustody: kaminoPrimeCustody}
	return computeRouteNAVForRoute(slot, accounts, manifest, override, route)
}

func ComputeRouteNAVForRoute(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, route RuntimeRoute) (RouteNAVSnapshot, error) {
	return computeRouteNAVForRoute(slot, accounts, manifest, override, route)
}

func computeRouteNAVForRoute(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, route RuntimeRoute) (RouteNAVSnapshot, error) {
	if slot <= 0 || !sha256Pattern.MatchString(manifest.SHA256) || manifest.PolicyCatalog.SHA256 == nil ||
		!sha256Pattern.MatchString(*manifest.PolicyCatalog.SHA256) {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV manifest or slot is invalid")
	}
	if len(accounts) != len(pinnedRouteNAVAddressesForRoute(route)) {
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
	custodies, err := decodeRouteNAVCustodiesForRoute(accounts, route)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	vault, err := decodeVoltrVaultBook(accountAt(accounts, bridgeVoltrVault))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	lpSupply, err := decodeVoltrLPSupply(accountAt(accounts, bridgeLPMint))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	observedCashOnly := custodies.SquadsPRIMEraw == 0 && custodies.SquadsDebtRaw == 0
	if override != nil {
		custodies = *override
	}
	if route.Kamino.DebtMint == bridgeUSDC && custodies.SquadsDebtRaw != 0 {
		return RouteNAVSnapshot{}, fmt.Errorf("USDC debt custody would be counted twice")
	}

	kaminoConfig := route.Kamino
	obligationAccount := accountAt(accounts, kaminoConfig.Obligation)
	obligation := decodedKaminoObligation{}
	if obligationAccount.Lamports != 0 {
		obligation, err = decodeKaminoObligation(obligationAccount, kaminoConfig)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
	}
	collateralReserve, err := decodeKaminoReserve(accountAt(accounts, kaminoConfig.CollateralReserve), kaminoConfig.CollateralMint, kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	debtReserve, err := decodeKaminoReserve(accountAt(accounts, kaminoConfig.DebtReserve), kaminoConfig.DebtMint, kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	// Empty positions and empty non-USDC custodies need no market valuation.
	// Both the observed and hypothetical poststate must be cash-only; an
	// override cannot hide exposure to evade the reserve health gate.
	cashOnly := observedCashOnly && !obligation.hasPosition && custodies.SquadsPRIMEraw == 0 && custodies.SquadsDebtRaw == 0
	marketEmergency, err := decodeKaminoMarketEmergency(accountAt(accounts, kaminoConfig.Market), kaminoConfig)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if err := validateKaminoReserveHealth(slot, marketEmergency, obligation, collateralReserve, debtReserve); err != nil && !cashOnly {
		return RouteNAVSnapshot{}, err
	}
	if collateralReserve.refreshedSlot > slot || debtReserve.refreshedSlot > slot || obligation.refreshedSlot > slot {
		return RouteNAVSnapshot{}, fmt.Errorf("Kamino valuation claims a future refresh slot")
	}
	// Voltr reports USDC raw units, never raw units of the selected debt
	// asset. A separate USDC reference from the same batch is required for
	// non-USDC lanes; neither a ticker nor equal decimals establishes a peg.
	usdcReserve := debtReserve
	if kaminoConfig.DebtMint != bridgeUSDC {
		reference, err := pinnedKaminoObservationConfig()
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		usdcReserve, err = decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), bridgeUSDC, reference)
		if err != nil {
			return RouteNAVSnapshot{}, fmt.Errorf("decode NAV USDC reference: %w", err)
		}
		if err := validateKaminoReserveHealth(slot, marketEmergency, obligation, usdcReserve); err != nil && !cashOnly {
			return RouteNAVSnapshot{}, err
		}
		if usdcReserve.refreshedSlot > slot {
			return RouteNAVSnapshot{}, fmt.Errorf("NAV USDC reference claims a future slot")
		}
	}
	if usdcReserve.mintDecimals != 6 {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV USDC reference decimals drifted")
	}
	var primeIdleValue, collateralValue, debtValue, debtIdleValue uint64
	if !cashOnly {
		redeemablePRIME, err := collateralReserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		primeIdleValue, err = valueBetweenTokenRaw(custodies.SquadsPRIMEraw, collateralReserve.mintDecimals, 6, collateralReserve.marketPriceSF, usdcReserve.marketPriceSF, false)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		collateralValue, err = valueBetweenTokenRaw(redeemablePRIME, collateralReserve.mintDecimals, 6, collateralReserve.marketPriceSF, usdcReserve.marketPriceSF, false)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		debtRaw, err := obligation.debtAtReserveRate(debtReserve)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		debtValue, err = valueBetweenTokenRaw(debtRaw, debtReserve.mintDecimals, 6, debtReserve.marketPriceSF, usdcReserve.marketPriceSF, true)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
		debtIdleValue, err = valueBetweenTokenRaw(custodies.SquadsDebtRaw, debtReserve.mintDecimals, 6, debtReserve.marketPriceSF, usdcReserve.marketPriceSF, false)
		if err != nil {
			return RouteNAVSnapshot{}, err
		}
	}
	values := []uint64{custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw, primeIdleValue, collateralValue, debtValue, debtIdleValue}
	for _, value := range values {
		if value > math.MaxInt64 {
			return RouteNAVSnapshot{}, fmt.Errorf("NAV component exceeds signed range")
		}
	}
	fingerprint, err := navInputFingerprintForRoute(slot, accounts, custodies, route)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	// The strategy custody ATA is deliberately absent from this list. On the
	// current Voltr binary the vault books that balance itself (receipt offset
	// 128) and credits it into totalValue when it is swept, so counting it here
	// double counts it. Every remaining component is value held outside Voltr
	// custody: Squads cash, idle collateral, and the Kamino net position.
	components := []NAVComponent{
		{Account: bridgeSquadsATA, Owner: bridgeVault, Raw: int64(custodies.SquadsUSDCraw), Slot: slot, Known: true},
		{Account: route.CollateralCustody, Owner: bridgeVault, Raw: int64(primeIdleValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":collateral", Owner: kaminoProgram, Raw: int64(collateralValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":debt", Owner: kaminoProgram, Raw: int64(debtValue), Slot: slot, Known: true, Liability: true},
	}
	if kaminoConfig.DebtMint != bridgeUSDC {
		components = append(components, NAVComponent{Account: route.DebtCustody, Owner: bridgeVault, Raw: int64(debtIdleValue), Slot: slot, Known: true})
	}
	nav, err := ComputeNAV(NAVSnapshotContext{
		Slot: slot, ReceiptFingerprint: fingerprint,
		ManifestSHA256: manifest.SHA256, PolicyCatalogSHA256: *manifest.PolicyCatalog.SHA256,
	}, components)
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
		DebtIdleValueRaw:  debtIdleValue,
		ObligationPresent: obligationAccount.Lamports != 0,
		Receipt:           receipt, Voltr: vault, LPSupplyRaw: lpSupply,
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
	// This public Phase 1 helper remains pinned to the legacy PRIME fixture.
	// The serialized worker uses ObserveConfirmedRouteSnapshot, which resolves
	// its manifest-frozen lane through the route-aware path.
	route := RuntimeRoute{Lane: RouteID, Kamino: KaminoObservationConfig{Obligation: kaminoPrimeUSDCObligation, CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve, CollateralMint: kaminoPrimeMint, DebtMint: kaminoUSDCMint, Market: kaminoMarket, Program: kaminoProgram, Vault: bridgeVault}, CollateralCustody: kaminoPrimeCustody}
	addresses := pinnedRouteNAVAddresses()
	slot, accounts, err := reader.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if slot < minimumSlot {
		return RouteNAVSnapshot{}, fmt.Errorf("confirmed NAV response regressed below its minimum slot")
	}
	return ComputeRouteNAVForRoute(slot, accounts, manifest, nil, route)
}
