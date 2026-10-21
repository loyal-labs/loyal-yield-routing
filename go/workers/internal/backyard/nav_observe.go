package backyard

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const voltrLPMintLength = 82

// VoltrVaultBook is the vault book decoded independently of the adaptor and of
// this worker's own arithmetic.
type VoltrVaultBook struct {
	voltr.Vault
}

// LPTotalsRaw includes all unharvested fee LP and dead weight in the supply
// Voltr prices against. Check each addition before it can wrap, then enforce
// the signed range used by decisions, for live observations and poststates.
func (b VoltrVaultBook) LPTotalsRaw(lpSupplyRaw uint64) (fees, supply uint64, err error) {
	for _, value := range []uint64{b.FeeAccumulatorManagerRaw, b.FeeAccumulatorAdminRaw, b.FeeAccumulatorProtocolRaw} {
		if value > math.MaxUint64-fees {
			return 0, 0, fmt.Errorf("Voltr LP fee accumulator overflows u64")
		}
		fees += value
	}
	if fees > math.MaxInt64 {
		return 0, 0, fmt.Errorf("Voltr LP fee accumulator exceeds signed range")
	}
	supply = lpSupplyRaw
	for _, value := range []uint64{fees, b.LPSupplyDeadWeightRaw} {
		if value > math.MaxUint64-supply {
			return 0, 0, fmt.Errorf("Voltr effective LP supply overflows u64")
		}
		supply += value
	}
	if supply > math.MaxInt64 {
		return 0, 0, fmt.Errorf("Voltr effective LP supply exceeds signed range")
	}
	return fees, supply, nil
}

func decodeVoltrVaultBook(v *Vault, account ConfirmedAccount) (VoltrVaultBook, error) {
	vault, err := voltr.DecodeVault(chainAccount(account, v.VoltrVault))
	if err != nil {
		return VoltrVaultBook{}, fmt.Errorf("Voltr vault account envelope or layout drifted")
	}
	if vault.AssetMint != solanaKey(bridgeUSDC) || vault.IdleATA != solanaKey(v.idleATA) || vault.LPMint != solanaKey(v.lpMint) ||
		vault.Manager != solanaKey(v.squads) || vault.Admin != solanaKey(bridgeSettingsSigner) {
		return VoltrVaultBook{}, fmt.Errorf("Voltr vault asset, LP, manager, or admin identity drifted")
	}
	return VoltrVaultBook{Vault: vault}, nil
}

func decodeVoltrLPSupply(v *Vault, account ConfirmedAccount) (uint64, error) {
	if account.Address != v.lpMint || account.Owner != bridgeTokenProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) < voltrLPMintLength {
		return 0, fmt.Errorf("Voltr LP mint account envelope drifted")
	}
	return binary.LittleEndian.Uint64(account.Data[36:44]), nil
}

type RouteNAVCustodies struct {
	VoltrIdleRaw, StrategyUSDCraw, SquadsUSDCraw, SquadsPRIMEraw uint64
	SquadsDebtRaw                                                uint64
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
	DebtIdleValueRaw        uint64
	PositionCollateralValue uint64
	PositionDebtValue       uint64
	Obligation              obligationPresence
	Receipt                 voltr.StrategyReceipt
	Voltr                   VoltrVaultBook
	LPSupplyRaw             uint64
	SnapshotDigest          string
	Report                  BridgeReport
	OtherLanes              []heldLaneState
}

func (n RouteNAVSnapshot) otherLanesState() string {
	states := make([]string, len(n.OtherLanes))
	for i, lane := range n.OtherLanes {
		states[i] = lane.String()
	}
	return strings.Join(states, "|")
}

func pinnedRouteNAVAddressesForRoute(v *Vault, route RuntimeRoute) []string {
	var config []string
	if !v.trustful() {
		config = []string{v.Strategy}
	}
	addresses := append(config,
		v.strategyReceipt,
		v.idleATA,
		v.strategyATA,
		v.cash,
		route.CollateralCustody,
		route.Kamino.Obligation,
		route.Kamino.CollateralReserve,
		route.Kamino.DebtReserve,
		route.Kamino.Market,
		v.VoltrVault,
		v.lpMint,
	)
	if route.Kamino.DebtMint != "" && route.Kamino.DebtMint != bridgeUSDC {
		addresses = append(addresses, route.DebtCustody, usdcReference.DebtReserve)
	}
	return addresses
}

func heldLaneNAVAddresses(route RuntimeRoute) []string {
	addresses := []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve, route.Kamino.Market, route.CollateralCustody}
	if route.Kamino.DebtMint != "" && route.Kamino.DebtMint != bridgeUSDC {
		addresses = append(addresses, route.DebtCustody, usdcReference.DebtReserve)
	}
	return addresses
}

func pinnedRouteNAVAddressesForRoutes(v *Vault, routes []RuntimeRoute) []string {
	if len(routes) == 0 {
		return nil
	}
	addresses := pinnedRouteNAVAddressesForRoute(v, routes[0])
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		seen[address] = true
	}
	for _, route := range routes[1:] {
		for _, address := range heldLaneNAVAddresses(route) {
			if !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		}
	}
	return addresses
}

func navRoutes(v *Vault, working RuntimeRoute, held []string) ([]RuntimeRoute, error) {
	routes := []RuntimeRoute{working}
	for _, lane := range held {
		if lane == working.Lane {
			continue
		}
		route, err := v.route(lane)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func selectRouteNAVAccountsForRoutes(v *Vault, accounts []ConfirmedAccount, routes []RuntimeRoute) ([]ConfirmedAccount, error) {
	pinned := pinnedRouteNAVAddressesForRoutes(v, routes)
	selected := make([]ConfirmedAccount, 0, len(pinned))
	for _, address := range pinned {
		account := accountAt(accounts, address)
		if account.Address == "" {
			return nil, fmt.Errorf("NAV account %s is absent", address)
		}
		selected = append(selected, account)
	}
	return selected, nil
}

func decodeStrategyReceipt(v *Vault, account ConfirmedAccount) (voltr.StrategyReceipt, error) {
	receipt, err := voltr.DecodeStrategyReceipt(chainAccount(account, v.strategyReceipt))
	if err != nil {
		return voltr.StrategyReceipt{}, fmt.Errorf("Voltr strategy receipt envelope or layout drifted")
	}
	// The custody balance is reported rather than required zero: a nonzero
	// value must surface as a custody monitor HOLD, not as an undecodable
	// account.
	if receipt.Vault != solanaKey(v.VoltrVault) || receipt.Strategy != solanaKey(v.Strategy) ||
		receipt.AdaptorProgram != solanaKey(v.Adaptor) || receipt.Version != voltr.UpgradedStrategyReceiptVersion {
		return voltr.StrategyReceipt{}, fmt.Errorf("Voltr strategy receipt binding or reserved bytes drifted")
	}
	return receipt, nil
}

func strategyReceiptIntegrityFault(account ConfirmedAccount) bool {
	return account.Address == "" || account.Owner != voltr.ProgramID.String() || len(account.Data) != voltr.StrategyReceiptSize
}

func decodeRouteNAVCustodiesForRoute(v *Vault, accounts []ConfirmedAccount, route RuntimeRoute) (RouteNAVCustodies, error) {
	idle, err := decodePinnedUSDC(accountAt(accounts, v.idleATA), v.idleAuth)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Voltr idle custody: %w", err)
	}
	strategy, err := decodePinnedUSDC(accountAt(accounts, v.strategyATA), v.strategyAuth)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Voltr strategy custody: %w", err)
	}
	squadsUSDC, err := decodePinnedUSDC(accountAt(accounts, v.cash), v.squads)
	if err != nil {
		return RouteNAVCustodies{}, fmt.Errorf("decode Squads USDC custody: %w", err)
	}
	collateralMint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
	if err != nil {
		return RouteNAVCustodies{}, err
	}
	authority, err := decodeBase58PublicKey(v.squads)
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
		if route.DebtCustody == "" || route.DebtCustody == v.cash || route.DebtCustody == route.CollateralCustody {
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

// valueBetweenTokenRaw converts raw units between assets without assuming
// matching decimals or a stablecoin peg. The two reserve prices use the same
// scaled-fraction quote denomination, which cancels exactly in their ratio.
func valueBetweenTokenRaw(raw uint64, tokenDecimals, debtDecimals uint8, tokenPriceSF, debtPriceSF [16]byte, liability bool) (uint64, error) {
	return valueBetweenPrices(raw, tokenDecimals, debtDecimals, littleInt(tokenPriceSF[:]), littleInt(debtPriceSF[:]), liability)
}

func valueBetweenPrices(raw uint64, tokenDecimals, debtDecimals uint8, tokenPrice, debtPrice *big.Int, liability bool) (uint64, error) {
	if tokenDecimals > 18 || debtDecimals > 18 {
		return 0, fmt.Errorf("unsupported token decimal scale")
	}
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

func navInputFingerprintForRoutes(v *Vault, slot int64, accounts []ConfirmedAccount, custodies RouteNAVCustodies, routes []RuntimeRoute) (string, error) {
	route := routes[0]
	pinned := pinnedRouteNAVAddressesForRoutes(v, routes)
	if slot <= 0 {
		return "", fmt.Errorf("NAV slot is invalid")
	}
	if len(accounts) != len(pinned) {
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
	source, valuationSlot := accounts[0].ValuationSource, accounts[0].ValuationSlot
	if source != "" && (source != routeRefreshValuationSource || valuationSlot != slot) || source == "" && valuationSlot != 0 {
		return "", fmt.Errorf("NAV valuation provenance is invalid")
	}
	parts := make([]string, 0, len(pinned)+1)
	if source != "" {
		parts = append(parts, fmt.Sprintf("valuation:%s:%d", source, valuationSlot))
	}
	for _, address := range pinned {
		account, ok := byAddress[address]
		if !ok {
			return "", fmt.Errorf("NAV account %s is absent", address)
		}
		if account.ValuationSource != source || account.ValuationSlot != valuationSlot {
			return "", fmt.Errorf("NAV mixes valuation sources or slots")
		}
		hash := sha256.Sum256(account.Data)
		parts = append(parts, fmt.Sprintf("%s:%s:%d:%t:%x", address, account.Owner, account.Lamports, account.Executable, hash[:]))
	}
	parts = append(parts, fmt.Sprintf("post:%d:%d:%d:%d", custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw, custodies.SquadsPRIMEraw))
	if route.Kamino.DebtMint != "" && route.Kamino.DebtMint != bridgeUSDC {
		parts = append(parts, fmt.Sprintf("post-debt:%d", custodies.SquadsDebtRaw))
	}
	sort.Strings(parts)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", slot, strings.Join(parts, "|"))))
	return hex.EncodeToString(hash[:]), nil
}

func ComputeRouteNAVForRoutes(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, routes []RuntimeRoute) (RouteNAVSnapshot, error) {
	return computeRouteNAVForRoutes(slot, accounts, manifest, override, routes)
}

func computeRouteNAVForRoutes(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, routes []RuntimeRoute) (RouteNAVSnapshot, error) {
	if len(routes) == 0 {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV lane set is empty")
	}
	route, v := routes[0], manifest.vault
	for i, other := range routes {
		for _, earlier := range routes[:i] {
			if other.Lane == earlier.Lane {
				return RouteNAVSnapshot{}, fmt.Errorf("NAV lane set repeats a lane")
			}
		}
	}
	if slot <= 0 || !sha256Pattern.MatchString(manifest.SHA256) {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV manifest or slot is invalid")
	}
	if len(accounts) != len(pinnedRouteNAVAddressesForRoutes(v, routes)) {
		return RouteNAVSnapshot{}, fmt.Errorf("NAV account namespace contains unsupported custody")
	}
	if !v.trustful() {
		if _, err := decodeObservedAdaptorConfig(v, accountAt(accounts, v.Strategy)); err != nil {
			return RouteNAVSnapshot{}, err
		}
	}
	receipt, err := decodeStrategyReceipt(v, accountAt(accounts, v.strategyReceipt))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if receipt.PositionValueRaw > bridgeMaxNAV {
		return RouteNAVSnapshot{}, fmt.Errorf("prior Voltr NAV state is incoherent")
	}
	custodies, err := decodeRouteNAVCustodiesForRoute(v, accounts, route)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	vault, err := decodeVoltrVaultBook(v, accountAt(accounts, v.VoltrVault))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	lpSupply, err := decodeVoltrLPSupply(v, accountAt(accounts, v.lpMint))
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	if _, _, err := vault.LPTotalsRaw(lpSupply); err != nil {
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
	cashOnly := observedCashOnly && !obligation.hasPosition && custodies.SquadsPRIMEraw == 0 && custodies.SquadsDebtRaw == 0
	reserves, err := decodeLaneReserves(accounts, kaminoConfig, slot, obligation, cashOnly)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	var held laneValues
	if !cashOnly {
		if held, err = reserves.value(obligation, custodies.SquadsPRIMEraw, custodies.SquadsDebtRaw); err != nil {
			return RouteNAVSnapshot{}, err
		}
	}
	primeIdleValue, collateralValue, debtValue, debtIdleValue := held.collateralIdle, held.collateral, held.debt, held.debtIdle
	values := []uint64{custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw, primeIdleValue, collateralValue, debtValue, debtIdleValue}
	for _, value := range values {
		if value > math.MaxInt64 {
			return RouteNAVSnapshot{}, fmt.Errorf("NAV component exceeds signed range")
		}
	}
	fingerprint, err := navInputFingerprintForRoutes(v, slot, accounts, custodies, routes)
	if err != nil {
		return RouteNAVSnapshot{}, err
	}
	// The strategy custody ATA is deliberately absent from this list. On the
	// current Voltr binary the vault books that balance itself (receipt offset
	// 128) and credits it into totalValue when it is swept, so counting it here
	// double counts it. Every remaining component is value held outside Voltr
	// custody: Squads cash, idle collateral, and the Kamino net position.
	components := []NAVComponent{
		{Account: v.cash, Owner: v.squads, Raw: int64(custodies.SquadsUSDCraw), Slot: slot, Known: true},
		{Account: route.CollateralCustody, Owner: v.squads, Raw: int64(primeIdleValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":collateral", Owner: kamino.ProgramID.String(), Raw: int64(collateralValue), Slot: slot, Known: true},
		{Account: kaminoConfig.Obligation + ":debt", Owner: kamino.ProgramID.String(), Raw: int64(debtValue), Slot: slot, Known: true, Liability: true},
	}
	if kaminoConfig.DebtMint != bridgeUSDC {
		components = append(components, NAVComponent{Account: route.DebtCustody, Owner: v.squads, Raw: int64(debtIdleValue), Slot: slot, Known: true})
	}
	counted := map[string]bool{v.cash: true, route.CollateralCustody: true}
	if kaminoConfig.DebtMint != bridgeUSDC {
		counted[route.DebtCustody] = true
	}
	otherLanes := make([]heldLaneState, 0, len(routes)-1)
	for _, other := range routes[1:] {
		value, err := valueHeldLane(v, slot, accounts, other, counted)
		if err != nil {
			return RouteNAVSnapshot{}, fmt.Errorf("value held lane %s: %w", other.Lane, err)
		}
		components = append(components, value.components...)
		otherLanes = append(otherLanes, value.state)
	}
	nav, err := ComputeNAV(NAVSnapshotContext{
		Slot: slot, ReceiptFingerprint: fingerprint,
		ManifestSHA256: manifest.SHA256,
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
		DebtIdleValueRaw: debtIdleValue,
		Obligation:       observedObligation(obligationAccount.Lamports != 0),
		Receipt:          receipt, Voltr: vault, LPSupplyRaw: lpSupply,
		SnapshotDigest: nav.SnapshotDigest,
		Report:         BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: uint64(nav.Raw), SnapshotDigest: nav.SnapshotDigest},
		OtherLanes:     otherLanes,
	}, nil
}

type heldLaneState struct {
	lane                                                   string
	collateralRaw, debtRaw, collateralIdleRaw, debtIdleRaw uint64
}

func (h heldLaneState) String() string {
	return fmt.Sprintf("%s:%d:%d:%d:%d", h.lane, h.collateralRaw, h.debtRaw, h.collateralIdleRaw, h.debtIdleRaw)
}

type heldLaneValue struct {
	components []NAVComponent
	state      heldLaneState
}

// laneReserves is one lane's three priced reserves: its collateral, its debt,
// and the USDC reserve every valuation is quoted in (the debt reserve itself
// for a USDC-debt lane).
type laneReserves struct {
	collateral, debt, usdc decodedKaminoReserve
}

type laneValues struct {
	collateralIdle, collateral, debt, debtIdle uint64
}

func decodeLaneReserves(accounts []ConfirmedAccount, config KaminoObservationConfig, slot int64, obligation decodedKaminoObligation, unhealthyAllowed bool) (laneReserves, error) {
	collateral, err := decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
	if err != nil {
		return laneReserves{}, err
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
	if err != nil {
		return laneReserves{}, err
	}
	emergency, err := decodeKaminoMarketEmergency(accountAt(accounts, config.Market), config)
	if err != nil {
		return laneReserves{}, err
	}
	if err := validateKaminoReserveHealth(slot, emergency, obligation, collateral, debt); err != nil && !unhealthyAllowed {
		return laneReserves{}, err
	}
	if collateral.refreshedSlot > slot || debt.refreshedSlot > slot || obligation.refreshedSlot > slot {
		return laneReserves{}, fmt.Errorf("Kamino valuation claims a future refresh slot")
	}
	usdc := debt
	if config.DebtMint != bridgeUSDC {
		reference := usdcReference
		if usdc, err = decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), bridgeUSDC, reference); err != nil {
			return laneReserves{}, fmt.Errorf("decode NAV USDC reference: %w", err)
		}
		if err := validateKaminoReserveHealth(slot, emergency, obligation, usdc); err != nil && !unhealthyAllowed {
			return laneReserves{}, err
		}
		if usdc.refreshedSlot > slot {
			return laneReserves{}, fmt.Errorf("NAV USDC reference claims a future slot")
		}
	}
	if usdc.mintDecimals != 6 {
		return laneReserves{}, fmt.Errorf("NAV USDC reference decimals drifted")
	}
	return laneReserves{collateral: collateral, debt: debt, usdc: usdc}, nil
}

func (r laneReserves) value(o decodedKaminoObligation, collateralIdleRaw, debtIdleRaw uint64) (laneValues, error) {
	redeemable, err := r.collateral.redeemLiquidityRaw(o.collateralDepositedRaw)
	if err != nil {
		return laneValues{}, err
	}
	toUSDC := func(raw uint64, reserve decodedKaminoReserve, liability bool) (uint64, error) {
		return valueBetweenTokenRaw(raw, reserve.mintDecimals, 6, reserve.marketPriceSF, r.usdc.marketPriceSF, liability)
	}
	var v laneValues
	if v.collateralIdle, err = toUSDC(collateralIdleRaw, r.collateral, false); err != nil {
		return laneValues{}, err
	}
	if v.collateral, err = toUSDC(redeemable, r.collateral, false); err != nil {
		return laneValues{}, err
	}
	debtRaw, err := o.debtAtReserveRate(r.debt)
	if err != nil {
		return laneValues{}, err
	}
	if v.debt, err = toUSDC(debtRaw, r.debt, true); err != nil {
		return laneValues{}, err
	}
	if v.debtIdle, err = toUSDC(debtIdleRaw, r.debt, false); err != nil {
		return laneValues{}, err
	}
	return v, nil
}

// valueHeldLane values one non-working valued lane from the same batch: its
// obligation, and each custody account it names that no earlier lane has
// counted (the vault's custody is counted once, never per lane). A lane with
// no position and no counted custody balance contributes nothing and needs no
// market valuation; any exposure keeps the reserve health gate.
func valueHeldLane(v *Vault, slot int64, accounts []ConfirmedAccount, route RuntimeRoute, counted map[string]bool) (heldLaneValue, error) {
	config := route.Kamino
	var collateralIdle, debtIdle uint64
	var err error
	countCollateral := !counted[route.CollateralCustody]
	if countCollateral {
		if collateralIdle, err = vaultCustodyRaw(v, accounts, route.CollateralCustody, config.CollateralMint, route.CollateralTokenProgram); err != nil {
			return heldLaneValue{}, err
		}
	}
	nonUSDC := config.DebtMint != bridgeUSDC
	countDebt := nonUSDC && route.DebtCustody != "" && route.DebtCustody != v.cash && !counted[route.DebtCustody]
	if countDebt {
		if debtIdle, err = vaultCustodyRaw(v, accounts, route.DebtCustody, config.DebtMint, route.DebtTokenProgram); err != nil {
			return heldLaneValue{}, err
		}
	}
	counted[route.CollateralCustody] = true
	if nonUSDC {
		counted[route.DebtCustody] = true
	}
	obligationAccount := accountAt(accounts, config.Obligation)
	if obligationAccount.Address != config.Obligation {
		return heldLaneValue{}, fmt.Errorf("held lane obligation is absent")
	}
	obligation := decodedKaminoObligation{}
	if obligationAccount.Lamports != 0 {
		if obligation, err = decodeKaminoObligation(obligationAccount, config); err != nil {
			return heldLaneValue{}, err
		}
	}
	state := heldLaneState{lane: route.Lane, collateralRaw: obligation.collateralDepositedRaw, debtRaw: obligation.debtRaw, collateralIdleRaw: collateralIdle, debtIdleRaw: debtIdle}
	if !obligation.hasPosition && collateralIdle == 0 && debtIdle == 0 {
		return heldLaneValue{state: state}, nil
	}
	reserves, err := decodeLaneReserves(accounts, config, slot, obligation, false)
	if err != nil {
		return heldLaneValue{}, err
	}
	values, err := reserves.value(obligation, collateralIdle, debtIdle)
	if err != nil {
		return heldLaneValue{}, err
	}
	for _, value := range []uint64{values.collateral, values.debt, values.collateralIdle, values.debtIdle} {
		if value > math.MaxInt64/4 {
			return heldLaneValue{}, fmt.Errorf("NAV component exceeds signed range")
		}
	}
	out := heldLaneValue{state: state}
	out.components = append(out.components,
		NAVComponent{Account: config.Obligation + ":collateral", Owner: kamino.ProgramID.String(), Raw: int64(values.collateral), Slot: slot, Known: true},
		NAVComponent{Account: config.Obligation + ":debt", Owner: kamino.ProgramID.String(), Raw: int64(values.debt), Slot: slot, Known: true, Liability: true})
	if countCollateral {
		out.components = append(out.components, NAVComponent{Account: route.CollateralCustody, Owner: v.squads, Raw: int64(values.collateralIdle), Slot: slot, Known: true})
	}
	if countDebt {
		out.components = append(out.components, NAVComponent{Account: route.DebtCustody, Owner: v.squads, Raw: int64(values.debtIdle), Slot: slot, Known: true})
	}
	return out, nil
}
