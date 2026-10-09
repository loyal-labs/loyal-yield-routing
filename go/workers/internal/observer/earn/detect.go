package earn

import (
	"bytes"
	"encoding/json"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// Policy-family detection, ported from loyal-actions detection.rs
// (detect_yield_route_policy_create, detect_yield_setup_policy_create,
// detect_balance_sweep_policy_create, detect_jupiter_cross_mint_policy_action)
// and the loyal-squads-policy-monitor event builders.

var (
	loyalHubProgram      = solana.MustPublicKeyFromBase58("LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH")
	subscriptionsProgram = solana.MustPublicKeyFromBase58("De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44")
	tokenProgram         = solana.TokenProgramID
	token2022Program     = solana.Token2022ProgramID
	usdcMint             = solana.MustPublicKeyFromBase58(fleet.USDCMint)
	rentSysvar           = solana.SysVarRentPubkey

	safeMarkets = []solana.PublicKey{
		solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
		solana.MustPublicKeyFromBase58("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
		solana.MustPublicKeyFromBase58("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
		solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
		solana.MustPublicKeyFromBase58("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"),
	}
	mediumMarkets = []solana.PublicKey{
		solana.MustPublicKeyFromBase58("DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek"),
		solana.MustPublicKeyFromBase58("GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs"),
		solana.MustPublicKeyFromBase58("CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw"),
	}
	aggressiveMarkets = []solana.PublicKey{
		solana.MustPublicKeyFromBase58("52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf"),
		solana.MustPublicKeyFromBase58("9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU"),
		solana.MustPublicKeyFromBase58("5wJeMrUYECGq41fxRESKALVcHnNX26TAWy4W98yULsua"),
		solana.MustPublicKeyFromBase58("ByYiZxp8QrdN9qbdtaAiePN8AAr3qvTPppNJDpf5DVJ5"),
	}

	// EARN_STABLECOINS order: CASH, USDG, PYUSD (Token-2022), USDC, USDT, USDS.
	earnStables = []struct{ mint, program solana.PublicKey }{
		{solana.MustPublicKeyFromBase58(fleet.CashMint), token2022Program},
		{solana.MustPublicKeyFromBase58(fleet.USDGMint), token2022Program},
		{solana.MustPublicKeyFromBase58(fleet.PYUSDMint), token2022Program},
		{usdcMint, tokenProgram},
		{solana.MustPublicKeyFromBase58(fleet.USDTMint), tokenProgram},
		{solana.MustPublicKeyFromBase58(fleet.USDSMint), tokenProgram},
	}
)

const (
	jupiterSwapSlippageOffset      = 24
	hubSwapExactIn                 = 1
	hubSwapTagOffset               = 0
	hubSwapMaxFeeOffset            = 25
	subscriptionsTransferRecurring = 5
	subscriptionsInitAuthority     = 0
	subscriptionsCreateRecurring   = 2
	transferDelegatorOffset        = 9
	transferMintOffset             = 41
	delegationDiscriminator        = 3
	delegationDelegatorOffset      = 3
	delegationDelegateeOffset      = 35
	delegationAuthorityOffset      = 107
	delegationMintOffset           = 139
	delegationPerPeriodOffset      = 195
)

func pda(program solana.PublicKey, seeds ...[]byte) solana.PublicKey {
	key, _, err := solana.FindProgramAddress(seeds, program)
	if err != nil {
		panic(err)
	}
	return key
}

func squadsVault(settings solana.PublicKey, index uint8) solana.PublicKey {
	key, _, err := squads.SmartAccountAddress(settings, index)
	if err != nil {
		panic(err)
	}
	return key
}

func associatedToken(owner, mint, program solana.PublicKey) solana.PublicKey {
	key, err := spl.AssociatedTokenAddress(owner, mint, program)
	if err != nil {
		panic(err)
	}
	return key
}

func uniqueKeys(keys []solana.PublicKey) []solana.PublicKey {
	var out []solana.PublicKey
	for _, key := range keys {
		if !containsKey(out, key) {
			out = append(out, key)
		}
	}
	return out
}

func containsKey(keys []solana.PublicKey, key solana.PublicKey) bool {
	for _, existing := range keys {
		if existing == key {
			return true
		}
	}
	return false
}

func sameKeySet(left, right []solana.PublicKey) bool {
	if len(left) != len(right) {
		return false
	}
	for _, key := range left {
		if !containsKey(right, key) {
			return false
		}
	}
	return true
}

func equalKeys(left, right []solana.PublicKey) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func keyStrings(keys []solana.PublicKey) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.String())
	}
	return out
}

// presetFor is detect_yield_route_universe_preset: an exact ordered match of
// the deduplicated markets against one Kamino Stable risk profile.
func presetFor(markets []solana.PublicKey) (preset, profile *string) {
	unique := uniqueKeys(markets)
	candidate := append([]solana.PublicKey(nil), safeMarkets...)
	for _, step := range []struct {
		name  string
		extra []solana.PublicKey
	}{{"safe", nil}, {"medium", mediumMarkets}, {"aggressive", aggressiveMarkets}} {
		candidate = uniqueKeys(append(candidate, step.extra...))
		if equalKeys(unique, candidate) {
			name, risk := "kamino_stable", step.name
			return &name, &risk
		}
	}
	return nil, nil
}

func accountsByIndex(constraint squads.InstructionConstraintView) map[uint8]squads.AccountConstraintView {
	out := make(map[uint8]squads.AccountConstraintView, len(constraint.AccountConstraints))
	for _, account := range constraint.AccountConstraints {
		out[account.AccountIndex] = account
	}
	return out
}

func sameOwner(constraint squads.AccountConstraintView, owner *solana.PublicKey) bool {
	if constraint.Owner == nil || owner == nil {
		return constraint.Owner == nil && owner == nil
	}
	return *constraint.Owner == *owner
}

func pubkeysOf(constraint squads.AccountConstraintView, ok bool, owner *solana.PublicKey) ([]solana.PublicKey, bool) {
	if !ok || !sameOwner(constraint, owner) || constraint.Pubkeys == nil {
		return nil, false
	}
	return constraint.Pubkeys, true
}

func singleOf(constraint squads.AccountConstraintView, ok bool, owner *solana.PublicKey) (solana.PublicKey, bool) {
	keys, ok := pubkeysOf(constraint, ok, owner)
	if !ok || len(keys) != 1 {
		return solana.PublicKey{}, false
	}
	return keys[0], true
}

func singleIs(accounts map[uint8]squads.AccountConstraintView, index uint8, owner *solana.PublicKey, expected solana.PublicKey) bool {
	constraint, ok := accounts[index]
	key, ok := singleOf(constraint, ok, owner)
	return ok && key == expected
}

func isSlice(c squads.DataConstraintView, offset uint64, expected []byte) bool {
	return c.DataOffset == offset && c.Operator == squads.OpEquals && c.DataValue.Kind == 5 && bytes.Equal(c.DataValue.Bytes, expected)
}

func hasSlice(constraints []squads.DataConstraintView, offset uint64, expected []byte) bool {
	for _, c := range constraints {
		if isSlice(c, offset, expected) {
			return true
		}
	}
	return false
}

func hasBytes(constraints []squads.DataConstraintView, offset uint64, expected []byte) bool {
	for _, c := range constraints {
		if c.Operator != squads.OpEquals || c.DataValue.Kind != 5 || offset < c.DataOffset {
			continue
		}
		relative := offset - c.DataOffset
		if relative+uint64(len(expected)) <= uint64(len(c.DataValue.Bytes)) && bytes.Equal(c.DataValue.Bytes[relative:relative+uint64(len(expected))], expected) {
			return true
		}
	}
	return false
}

func isU8(c squads.DataConstraintView, offset uint64, expected uint8) bool {
	return c.DataOffset == offset && c.Operator == squads.OpEquals && c.DataValue.Kind == 0 && c.DataValue.U8 == expected
}

func hasU8(constraints []squads.DataConstraintView, offset uint64, expected uint8) bool {
	for _, c := range constraints {
		if isU8(c, offset, expected) {
			return true
		}
	}
	return false
}

func u16LTE(constraints []squads.DataConstraintView, offset uint64) (uint16, bool) {
	for _, c := range constraints {
		if c.DataOffset == offset && c.Operator == squads.OpLessThanOrEqualTo && c.DataValue.Kind == 1 {
			return c.DataValue.U16, true
		}
	}
	return 0, false
}

func hasTokenAuthority(constraint squads.AccountConstraintView, ok bool, authority solana.PublicKey) bool {
	return ok && constraint.Owner != nil && *constraint.Owner == tokenProgram && constraint.Pubkeys == nil && hasSlice(constraint.AccountData, 32, authority[:])
}

func hubOwnerSupported(owner *solana.PublicKey) bool {
	return owner == nil || *owner == tokenProgram || *owner == token2022Program
}

type kaminoLeg struct {
	vault                   solana.PublicKey
	markets, liquidityMints []solana.PublicKey
}

func sameMintMarketsSupported(markets []solana.PublicKey) bool {
	if len(markets) == 0 {
		return false
	}
	for _, market := range markets {
		if !containsKey(safeMarkets, market) {
			return false
		}
	}
	return true
}

func fullKaminoLeg(accounts map[uint8]squads.AccountConstraintView, vault solana.PublicKey) (kaminoLeg, bool) {
	c9, ok9 := accounts[9]
	if !hasTokenAuthority(c9, ok9, vault) || !singleIs(accounts, 11, nil, tokenProgram) || !singleIs(accounts, 12, nil, tokenProgram) {
		return kaminoLeg{}, false
	}
	c2, ok2 := accounts[2]
	markets, ok := pubkeysOf(c2, ok2, nil)
	if !ok {
		return kaminoLeg{}, false
	}
	c5, ok5 := accounts[5]
	mints, ok := pubkeysOf(c5, ok5, &tokenProgram)
	if !ok {
		return kaminoLeg{}, false
	}
	return kaminoLeg{vault, markets, mints}, true
}

func compactSameMintLeg(accounts map[uint8]squads.AccountConstraintView, vault solana.PublicKey) (kaminoLeg, bool) {
	c2, ok2 := accounts[2]
	markets, ok := pubkeysOf(c2, ok2, nil)
	if !ok {
		return kaminoLeg{}, false
	}
	markets = uniqueKeys(markets)
	var mints []solana.PublicKey
	if c5, ok5 := accounts[5]; ok5 {
		if keys, ok := pubkeysOf(c5, true, nil); ok {
			mints = uniqueKeys(keys)
		}
	}
	if !sameMintMarketsSupported(markets) {
		return kaminoLeg{}, false
	}
	for _, mint := range mints {
		supported := false
		for _, stable := range earnStables {
			supported = supported || stable.mint == mint
		}
		if !supported {
			return kaminoLeg{}, false
		}
	}
	return kaminoLeg{vault, markets, mints}, true
}

func classifyKaminoWithdraw(constraint squads.InstructionConstraintView) (kaminoLeg, bool) {
	if constraint.ProgramID != kamino.ProgramID || !hasSlice(constraint.DataConstraints, 0, kamino.WithdrawV2Discriminator[:]) {
		return kaminoLeg{}, false
	}
	accounts := accountsByIndex(constraint)
	c0, ok0 := accounts[0]
	vault, ok := singleOf(c0, ok0, nil)
	if !ok {
		return kaminoLeg{}, false
	}
	if leg, ok := fullKaminoLeg(accounts, vault); ok {
		return leg, true
	}
	return compactSameMintLeg(accounts, vault)
}

func classifyKaminoDeposit(constraint squads.InstructionConstraintView, vault solana.PublicKey) (kaminoLeg, bool) {
	if constraint.ProgramID != kamino.ProgramID || !hasSlice(constraint.DataConstraints, 0, kamino.DepositV2Discriminator[:]) {
		return kaminoLeg{}, false
	}
	accounts := accountsByIndex(constraint)
	if !singleIs(accounts, 0, nil, vault) {
		return kaminoLeg{}, false
	}
	if leg, ok := fullKaminoLeg(accounts, vault); ok {
		return leg, true
	}
	return compactSameMintLeg(accounts, vault)
}

func hasU8OrSlice(constraints []squads.DataConstraintView, offset uint64, expected uint8) bool {
	return hasU8(constraints, offset, expected) || hasBytes(constraints, offset, []byte{expected})
}

func classifyInitObligation(constraint squads.InstructionConstraintView, vault solana.PublicKey) ([]solana.PublicKey, bool) {
	if constraint.ProgramID != kamino.ProgramID || !hasBytes(constraint.DataConstraints, 0, kamino.InitObligationDiscriminator[:]) ||
		!hasU8OrSlice(constraint.DataConstraints, 8, 0) || !hasU8OrSlice(constraint.DataConstraints, 9, 0) {
		return nil, false
	}
	accounts := accountsByIndex(constraint)
	if !singleIs(accounts, 0, nil, vault) || !singleIs(accounts, 1, nil, vault) {
		return nil, false
	}
	c3, ok3 := accounts[3]
	markets, ok := pubkeysOf(c3, ok3, nil)
	if !ok {
		return nil, false
	}
	markets = uniqueKeys(markets)
	if c2, ok2 := accounts[2]; ok2 {
		obligations, ok := pubkeysOf(c2, true, nil)
		if !ok {
			return nil, false
		}
		obligations = uniqueKeys(obligations)
		if len(obligations) != len(markets) {
			return nil, false
		}
		var expected []solana.PublicKey
		for _, market := range markets {
			obligation, err := kamino.VanillaObligation(vault, market)
			if err != nil {
				return nil, false
			}
			expected = append(expected, obligation)
		}
		if !sameKeySet(obligations, expected) {
			return nil, false
		}
	}
	userMetadata, err := kamino.UserMetadataAddress(vault)
	if err != nil || !singleIs(accounts, 4, nil, solana.PublicKey{}) || !singleIs(accounts, 5, nil, solana.PublicKey{}) ||
		!singleIs(accounts, 6, nil, userMetadata) || !singleIs(accounts, 7, nil, rentSysvar) || !singleIs(accounts, 8, nil, solana.SystemProgramID) {
		return nil, false
	}
	return markets, true
}

func classifyRefreshObligation(constraint squads.InstructionConstraintView, vault solana.PublicKey) ([]solana.PublicKey, bool) {
	if constraint.ProgramID != kamino.ProgramID || !hasSlice(constraint.DataConstraints, 0, kamino.RefreshObligationDiscriminator[:]) {
		return nil, false
	}
	accounts := accountsByIndex(constraint)
	c0, ok0 := accounts[0]
	markets, ok := pubkeysOf(c0, ok0, nil)
	if !ok {
		return nil, false
	}
	c1, ok1 := accounts[1]
	obligations, ok := pubkeysOf(c1, ok1, nil)
	if !ok {
		return nil, false
	}
	markets, obligations = uniqueKeys(markets), uniqueKeys(obligations)
	if len(obligations) != len(markets) {
		return nil, false
	}
	var expected []solana.PublicKey
	for _, market := range markets {
		obligation, err := kamino.VanillaObligation(vault, market)
		if err != nil {
			return nil, false
		}
		expected = append(expected, obligation)
	}
	if !sameKeySet(obligations, expected) {
		return nil, false
	}
	return markets, true
}

// swapLane is SwapLaneEvent with serde's internally tagged "kind".
type swapLane struct {
	Kind                 string  `json:"kind"`
	ProgramID            string  `json:"program_id,omitempty"`
	ExactInDiscriminator []int   `json:"exact_in_discriminator,omitempty"`
	HubAuthorizer        string  `json:"hub_authorizer,omitempty"`
	MaxFeeBPS            *uint16 `json:"max_fee_bps,omitempty"`
}

func classifyJupiterSwap(constraint squads.InstructionConstraintView, vault solana.PublicKey) ([]solana.PublicKey, *swapLane, bool) {
	if !hasSlice(constraint.DataConstraints, 0, jupiter.RouteV2Discriminator[:]) {
		return nil, nil, false
	}
	if _, ok := u16LTE(constraint.DataConstraints, jupiterSwapSlippageOffset); !ok {
		return nil, nil, false
	}
	accounts := accountsByIndex(constraint)
	c1, ok1 := accounts[1]
	c2, ok2 := accounts[2]
	if !singleIs(accounts, 0, nil, vault) || !hasTokenAuthority(c1, ok1, vault) || !hasTokenAuthority(c2, ok2, vault) || !singleIs(accounts, 5, nil, tokenProgram) {
		return nil, nil, false
	}
	c3, ok3 := accounts[3]
	input, ok := pubkeysOf(c3, ok3, &tokenProgram)
	if !ok {
		return nil, nil, false
	}
	c4, ok4 := accounts[4]
	output, ok := pubkeysOf(c4, ok4, &tokenProgram)
	if !ok {
		return nil, nil, false
	}
	discriminator := make([]int, len(jupiter.RouteV2Discriminator))
	for i, value := range jupiter.RouteV2Discriminator {
		discriminator[i] = int(value)
	}
	return append(append([]solana.PublicKey(nil), input...), output...), &swapLane{Kind: "jupiter", ProgramID: constraint.ProgramID.String(), ExactInDiscriminator: discriminator}, true
}

func classifyHubSwap(constraint squads.InstructionConstraintView, vault solana.PublicKey) ([]solana.PublicKey, *swapLane, bool) {
	if constraint.ProgramID != loyalHubProgram || !hasU8(constraint.DataConstraints, hubSwapTagOffset, hubSwapExactIn) {
		return nil, nil, false
	}
	maxFee, ok := u16LTE(constraint.DataConstraints, hubSwapMaxFeeOffset)
	if !ok {
		return nil, nil, false
	}
	accounts := accountsByIndex(constraint)
	config := pda(loyalHubProgram, []byte("config"))
	if !singleIs(accounts, 0, &loyalHubProgram, config) || !singleIs(accounts, 1, nil, vault) {
		return nil, nil, false
	}
	for _, index := range []uint8{2, 3} {
		c, ok := accounts[index]
		if !ok || !hubOwnerSupported(c.Owner) || c.Pubkeys != nil || !hasSlice(c.AccountData, 32, vault[:]) {
			return nil, nil, false
		}
	}
	var mints []solana.PublicKey
	for _, index := range []uint8{6, 7} {
		c, ok := accounts[index]
		if !ok || !hubOwnerSupported(c.Owner) || c.Pubkeys == nil {
			return nil, nil, false
		}
		mints = append(mints, c.Pubkeys...)
	}
	c9, ok9 := accounts[9]
	authorizer, ok := singleOf(c9, ok9, nil)
	if !ok || !singleIs(accounts, 10, nil, tokenProgram) {
		return nil, nil, false
	}
	return mints, &swapLane{Kind: "loyal_hub", HubAuthorizer: authorizer.String(), MaxFeeBPS: &maxFee}, true
}

// detectYieldRoute is detect_yield_route_policy_create mapped to its event.
func detectYieldRoute(action squads.SettingsAction) (PolicyMatchInput, bool) {
	constraints := action.Payload.Constraints
	if len(constraints) < 2 {
		return PolicyMatchInput{}, false
	}
	var withdraw *kaminoLeg
	for _, constraint := range constraints {
		if leg, ok := classifyKaminoWithdraw(constraint); ok {
			withdraw = &leg
			break
		}
	}
	if withdraw == nil {
		return PolicyMatchInput{}, false
	}
	var deposit *kaminoLeg
	var initMarkets, refreshMarkets, stableMints []solana.PublicKey
	lanes := []swapLane{}
	hasJupiter, hasHub := false, false
	for _, constraint := range constraints {
		if leg, ok := classifyKaminoDeposit(constraint, withdraw.vault); ok {
			if deposit != nil {
				return PolicyMatchInput{}, false
			}
			deposit = &leg
			continue
		}
		if _, ok := classifyKaminoWithdraw(constraint); ok {
			continue
		}
		if markets, ok := classifyInitObligation(constraint, withdraw.vault); ok {
			initMarkets = append(initMarkets, markets...)
			continue
		}
		if markets, ok := classifyRefreshObligation(constraint, withdraw.vault); ok {
			refreshMarkets = append(refreshMarkets, markets...)
			continue
		}
		if mints, lane, ok := classifyJupiterSwap(constraint, withdraw.vault); ok {
			stableMints = append(stableMints, mints...)
			lanes = append(lanes, *lane)
			hasJupiter = true
			continue
		}
		if mints, lane, ok := classifyHubSwap(constraint, withdraw.vault); ok {
			stableMints = append(stableMints, mints...)
			lanes = append(lanes, *lane)
			hasHub = true
			continue
		}
		return PolicyMatchInput{}, false
	}
	if deposit == nil {
		return PolicyMatchInput{}, false
	}
	modes := []string{"same_mint_kamino"}
	if hasJupiter {
		modes = append(modes, "cross_mint_jupiter")
	}
	if hasHub {
		modes = append(modes, "cross_mint_loyal_hub")
	}
	markets := uniqueKeys(append(append([]solana.PublicKey(nil), withdraw.markets...), deposit.markets...))
	if initMarkets = uniqueKeys(initMarkets); len(initMarkets) > 0 && !sameKeySet(initMarkets, markets) {
		return PolicyMatchInput{}, false
	}
	if refreshMarkets = uniqueKeys(refreshMarkets); len(refreshMarkets) > 0 && !sameKeySet(refreshMarkets, markets) {
		return PolicyMatchInput{}, false
	}
	liquidityMints := uniqueKeys(append(append([]solana.PublicKey(nil), withdraw.liquidityMints...), deposit.liquidityMints...))
	if len(stableMints) == 0 {
		stableMints = liquidityMints
	} else {
		stableMints = uniqueKeys(stableMints)
	}
	preset, risk := presetFor(markets)
	encodedLanes, _ := json.Marshal(lanes)
	return PolicyMatchInput{
		Settings: action.Settings.String(), Authority: action.Authority.String(), PolicySeed: action.PolicySeed,
		PolicyAccount: action.PolicyAccount.String(), VaultIndex: action.Payload.VaultIndex,
		VaultPubkey:      squadsVault(action.Settings, action.Payload.VaultIndex).String(),
		DelegatedSigners: keyStrings(uniqueKeys(action.DelegatedSigners)), Threshold: action.Threshold,
		RouteModes: modes, StableMints: keyStrings(stableMints), KaminoMarkets: keyStrings(markets),
		KaminoLiquidityMints: keyStrings(liquidityMints), UniversePreset: preset, RiskProfile: risk, SwapLanes: encodedLanes,
	}, true
}

// detectYieldSetup is detect_yield_setup_policy_create mapped to its event.
func detectYieldSetup(action squads.SettingsAction) (PolicyMatchInput, bool) {
	if len(action.Payload.Constraints) != 1 {
		return PolicyMatchInput{}, false
	}
	vault := squadsVault(action.Settings, action.Payload.VaultIndex)
	markets, ok := classifyInitObligation(action.Payload.Constraints[0], vault)
	if !ok {
		return PolicyMatchInput{}, false
	}
	markets = uniqueKeys(markets)
	if !sameMintMarketsSupported(markets) {
		return PolicyMatchInput{}, false
	}
	preset, risk := presetFor(markets)
	return PolicyMatchInput{
		Settings: action.Settings.String(), Authority: action.Authority.String(), PolicySeed: action.PolicySeed,
		PolicyAccount: action.PolicyAccount.String(), VaultIndex: action.Payload.VaultIndex, VaultPubkey: vault.String(),
		DelegatedSigners: keyStrings(uniqueKeys(action.DelegatedSigners)), Threshold: action.Threshold,
		RouteModes: []string{"kamino_setup"}, StableMints: []string{}, KaminoMarkets: keyStrings(markets),
		KaminoLiquidityMints: []string{}, UniversePreset: preset, RiskProfile: risk, SwapLanes: json.RawMessage("[]"),
	}, true
}

func accountDataKey(constraint squads.AccountConstraintView, offset uint64) (solana.PublicKey, bool) {
	if constraint.Owner == nil || *constraint.Owner != subscriptionsProgram || constraint.Pubkeys != nil {
		return solana.PublicKey{}, false
	}
	for _, c := range constraint.AccountData {
		if c.DataOffset == offset && c.Operator == squads.OpEquals && c.DataValue.Kind == 5 && len(c.DataValue.Bytes) == 32 {
			return solana.PublicKeyFromBytes(c.DataValue.Bytes), true
		}
	}
	return solana.PublicKey{}, false
}

// detectBalanceSweep is detect_balance_sweep_policy_create mapped to its event.
func detectBalanceSweep(action squads.SettingsAction) (BalanceSweepPolicyMatchInput, bool) {
	if len(action.Payload.Constraints) != 1 {
		return BalanceSweepPolicyMatchInput{}, false
	}
	constraint := action.Payload.Constraints[0]
	if constraint.ProgramID != subscriptionsProgram || !hasU8(constraint.DataConstraints, 0, subscriptionsTransferRecurring) || !hasSlice(constraint.DataConstraints, transferMintOffset, usdcMint[:]) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	accounts := accountsByIndex(constraint)
	recurring, ok := accounts[0]
	if !ok {
		return BalanceSweepPolicyMatchInput{}, false
	}
	wallet, ok := accountDataKey(recurring, delegationDelegatorOffset)
	if !ok || !hasSlice(constraint.DataConstraints, transferDelegatorOffset, wallet[:]) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	if recurring.Owner == nil || recurring.Pubkeys != nil || !hasU8(recurring.AccountData, 0, delegationDiscriminator) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	vault, ok := accountDataKey(recurring, delegationDelegateeOffset)
	if !ok {
		return BalanceSweepPolicyMatchInput{}, false
	}
	authority, ok := accountDataKey(recurring, delegationAuthorityOffset)
	if !ok || authority != pda(subscriptionsProgram, []byte("SubscriptionAuthority"), wallet[:], usdcMint[:]) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	if mint, ok := accountDataKey(recurring, delegationMintOffset); !ok || mint != usdcMint {
		return BalanceSweepPolicyMatchInput{}, false
	}
	var perPeriod uint64
	found := false
	for _, c := range recurring.AccountData {
		if c.DataOffset == delegationPerPeriodOffset && c.Operator == squads.OpLessThanOrEqualTo && c.DataValue.Kind == 3 {
			perPeriod, found = c.DataValue.U64, true
			break
		}
	}
	if !found || !singleIs(accounts, 1, &subscriptionsProgram, authority) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	c2, ok2 := accounts[2]
	walletATA, ok := singleOf(c2, ok2, &tokenProgram)
	if !ok {
		return BalanceSweepPolicyMatchInput{}, false
	}
	c3, ok3 := accounts[3]
	vaultATA, ok := singleOf(c3, ok3, &tokenProgram)
	if !ok || !singleIs(accounts, 4, &tokenProgram, usdcMint) || !singleIs(accounts, 5, nil, tokenProgram) || !singleIs(accounts, 6, nil, vault) ||
		!singleIs(accounts, 7, nil, pda(subscriptionsProgram, []byte("event_authority"))) || !singleIs(accounts, 8, nil, subscriptionsProgram) {
		return BalanceSweepPolicyMatchInput{}, false
	}
	vaultPubkey := squadsVault(action.Settings, action.Payload.VaultIndex)
	if vault != vaultPubkey {
		return BalanceSweepPolicyMatchInput{}, false
	}
	return BalanceSweepPolicyMatchInput{
		Settings: action.Settings.String(), Authority: action.Authority.String(), PolicySeed: action.PolicySeed,
		PolicyAccount: action.PolicyAccount.String(), VaultIndex: action.Payload.VaultIndex, VaultPubkey: vaultPubkey.String(),
		Wallet: wallet.String(), WalletUSDCATA: walletATA.String(), VaultUSDCATA: vaultATA.String(),
		TokenMint: usdcMint.String(), WalletTokenATA: walletATA.String(), VaultTokenATA: vaultATA.String(),
		DelegatedSigners: keyStrings(uniqueKeys(action.DelegatedSigners)), Threshold: action.Threshold, MaxAmountPerPeriod: perPeriod,
	}, true
}

// detectCrossMintPolicy is detect_jupiter_cross_mint_policy_action mapped to
// its manifest event.
func detectCrossMintPolicy(instruction squads.Instruction) (CrossMintSwapPolicyManifestInput, bool) {
	action, err := squads.DecodeStrictPolicyAction(instruction)
	if err != nil || action == nil {
		return CrossMintSwapPolicyManifestInput{}, false
	}
	payload := action.Payload
	if !squads.CreationTableIsTight(payload) || len(payload.Constraints) != 2 {
		return CrossMintSwapPolicyManifestInput{}, false
	}
	dialects := []struct {
		discriminator                     []byte
		authority, output                 uint8
		slippageOffset, platformFeeOffset uint64
	}{{jupiter.RouteV2Discriminator[:], 0, 2, 24, 26}, {jupiter.SharedAccountsRouteV2Discriminator[:], 1, 5, 25, 27}}
	var vault solana.PublicKey
	var maxSlippage uint16
	for index, constraint := range payload.Constraints {
		dialect := dialects[index]
		if constraint.ProgramID != jupiter.ProgramID || len(constraint.AccountConstraints) != 2 || len(constraint.DataConstraints) != 3 {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		authority, output := constraint.AccountConstraints[0], constraint.AccountConstraints[1]
		if authority.AccountIndex != dialect.authority || authority.Owner != nil || authority.Pubkeys == nil || len(authority.Pubkeys) != 1 {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		candidate := authority.Pubkeys[0]
		if output.AccountIndex != dialect.output || output.Owner != nil || output.Pubkeys == nil {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		var expected []solana.PublicKey
		for _, stable := range earnStables {
			expected = append(expected, associatedToken(candidate, stable.mint, stable.program))
		}
		if len(uniqueKeys(output.Pubkeys)) != len(output.Pubkeys) || !sameKeySet(uniqueKeys(output.Pubkeys), uniqueKeys(expected)) {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		discriminator, slippage, fee := constraint.DataConstraints[0], constraint.DataConstraints[1], constraint.DataConstraints[2]
		if !isSlice(discriminator, 0, dialect.discriminator) || !isU8(fee, dialect.platformFeeOffset, 0) {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		if slippage.DataOffset != dialect.slippageOffset || slippage.Operator != squads.OpLessThanOrEqualTo || slippage.DataValue.Kind != 1 {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		bps := slippage.DataValue.U16
		if bps == 0 || bps > 10_000 || (index == 1 && bps != maxSlippage) {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		if index == 1 && candidate != vault {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		vault, maxSlippage = candidate, bps
	}
	shard, sourceProgram := "", solana.PublicKey{}
	for _, candidate := range []struct {
		name    string
		program solana.PublicKey
	}{{"classic", tokenProgram}, {"token_2022", token2022Program}} {
		var mints []solana.PublicKey
		for _, stable := range earnStables {
			if stable.program == candidate.program {
				mints = append(mints, stable.mint)
			}
		}
		var limitMints []solana.PublicKey
		for _, limit := range payload.SpendingLimits {
			limitMints = append(limitMints, limit.Mint)
		}
		if sameKeySet(uniqueKeys(limitMints), mints) {
			shard, sourceProgram = candidate.name, candidate.program
			break
		}
	}
	if shard == "" || len(payload.SpendingLimits) != 3 {
		return CrossMintSwapPolicyManifestInput{}, false
	}
	var cap uint64
	for index, limit := range payload.SpendingLimits {
		if limit.Start != 0 || limit.Expiration != nil || limit.Period != 1 || limit.MaxPerPeriod == 0 || (index > 0 && limit.MaxPerPeriod != cap) {
			return CrossMintSwapPolicyManifestInput{}, false
		}
		cap = limit.MaxPerPeriod
	}
	if vault != squadsVault(action.Envelope.Settings, payload.VaultIndex) {
		return CrossMintSwapPolicyManifestInput{}, false
	}
	var sourceMints []string
	for _, stable := range earnStables {
		if stable.program == sourceProgram {
			sourceMints = append(sourceMints, stable.mint.String())
		}
	}
	mutation := "update"
	var seed *uint64
	if action.Create {
		mutation, seed = "create", &action.PolicySeed
	}
	return CrossMintSwapPolicyManifestInput{
		Mutation: mutation, Settings: action.Envelope.Settings.String(), Authority: action.Envelope.Authority.String(),
		PolicySeed: seed, PolicyAccount: action.PolicyAccount.String(), VaultIndex: payload.VaultIndex, VaultPubkey: vault.String(),
		DelegatedSigner: action.DelegatedSigner.String(), SourceShard: shard, MaxSlippageBPS: maxSlippage, DailySourceMintSpendingCap: cap,
		ManifestFingerprint: fleet.CrossMintManifestFingerprint(action.Envelope.Settings.String(), vault.String(), action.DelegatedSigner.String(),
			payload.VaultIndex, maxSlippage, cap, sourceMints, sourceProgram.String()),
	}, true
}
