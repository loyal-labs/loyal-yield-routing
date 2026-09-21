package backyardrwa

import (
	"bytes"
	"reflect"
	"testing"
)

// Doc-13 candidate readiness: with a validated explicit manifest binding, AUTO
// observation/readiness requires the ONE combined policy account plus the four
// existing masked bridge policy accounts. The historical four Kamino shards and
// six catalog swap edges stay observation pins only: they are never requested
// for AUTO and can never establish readiness, with or without a binding.
//
// The combined pin binds to fixture bytes under the reviewed seed-900 address,
// because the deployed account image lives in the Rust semantics evidence, not
// in Go fixtures. Live bytes exist only for the seed-152 allocation bridge
// policy, so that pin keeps its production digest and image; the other three
// bridge pins bind to fixture bytes under their production mask ranges
// (unchanged), which exercises the same masked comparator the send path uses.

type autoReadinessFixture struct {
	manifest    RouteManifest
	binding     AutoPolicyBinding
	accountData map[string][]byte
	masks       map[string][][2]int64
}

func autoReadinessAccountBytes(seed byte, length int) []byte {
	data := make([]byte, length)
	for index := range data {
		data[index] = seed + byte(index%251)
	}
	return data
}

func applyPolicyMask(data []byte, mask [][2]int64, value byte) {
	for _, bounds := range mask {
		for offset := int(bounds[0]); offset < int(bounds[1]); offset++ {
			data[offset] = value
		}
	}
}

func autoMaskedDigest(t *testing.T, data []byte, mask [][2]int64) string {
	t.Helper()
	if err := validatePolicyByteMask(mask); err != nil {
		t.Fatal(err)
	}
	masked := append([]byte(nil), data...)
	applyPolicyMask(masked, mask, 0)
	return sha256Bytes(masked)
}

func newAutoReadinessFixture(t *testing.T) *autoReadinessFixture {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	combined := autoReadinessAccountBytes(0x11, 2849)
	binding := autoFixtureBinding(t)
	binding.AccountDataSHA256 = sha256Bytes(combined)
	manifest.RuntimeBindings.AutoPolicy = &binding
	fixture := &autoReadinessFixture{manifest: manifest, binding: binding,
		accountData: map[string][]byte{binding.Policy: combined}, masks: map[string][][2]int64{}}
	for index := range manifest.RuntimeBindings.BridgePolicies {
		entry := &manifest.RuntimeBindings.BridgePolicies[index]
		if entry.Action == VoltrAllocateToSquads {
			if entry.NormalizedDigest != strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads] {
				t.Fatal("embedded allocation bridge digest drifted from the captured policy evidence")
			}
			fixture.accountData[entry.Account] = liveSquadsPolicy152Bytes(t)
			fixture.masks[entry.Account] = entry.MaskedByteRanges
			continue
		}
		if err := validatePolicyByteMask(entry.MaskedByteRanges); err != nil {
			t.Fatal(err)
		}
		data := autoReadinessAccountBytes(byte(0x30+index), 1548)
		entry.NormalizedDigest = autoMaskedDigest(t, data, entry.MaskedByteRanges)
		fixture.accountData[entry.Account] = data
		fixture.masks[entry.Account] = entry.MaskedByteRanges
	}
	return fixture
}

func (f *autoReadinessFixture) accounts(t *testing.T, route RuntimeRoute, extra ...ConfirmedAccount) []ConfirmedAccount {
	t.Helper()
	pins, err := catalogRoutePolicyPins(route, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	accounts := make([]ConfirmedAccount, 0, len(pins)+len(extra))
	for address := range pins {
		data, ok := f.accountData[address]
		if !ok {
			t.Fatalf("no fixture bytes for pinned account %s", address)
		}
		accounts = append(accounts, ConfirmedAccount{Address: address, Owner: bridgeSquadsProgram, Lamports: 1, Data: append([]byte(nil), data...)})
	}
	return append(accounts, extra...)
}

func TestAutoCandidateReadinessRequiresCombinedAndBridgePins(t *testing.T) {
	fixture := newAutoReadinessFixture(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := catalogRoutePolicyPins(route, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 5 {
		t.Fatalf("candidate readiness resolved %d pins, want the combined policy plus four bridge pins", len(pins))
	}
	if _, ok := pins[fixture.binding.Policy]; !ok {
		t.Fatal("combined AUTO policy is not a readiness pin")
	}
	// No historical shard may remain an accidental requirement.
	for _, b := range route.KaminoPolicies {
		if _, ok := pins[b.Policy]; ok {
			t.Fatalf("old Kamino shard %s is still an AUTO readiness requirement", b.Policy)
		}
	}
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		b, err := catalogJupiterBindingForRoute(action, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := pins[b.Policy]; ok {
			t.Fatalf("old catalog swap shard %s is still an AUTO readiness requirement", b.Policy)
		}
	}
	// The bridge pins keep their accounts, masked digests and masks.
	binding, err := fixture.manifest.bridgePolicy(VoltrAllocateToSquads)
	if err != nil {
		t.Fatal(err)
	}
	pin, ok := pins[binding.Account]
	if !ok || pin.digest != binding.NormalizedDigest || len(pin.mask) == 0 {
		t.Fatalf("candidate readiness lost the masked bridge pin: %+v", pin)
	}
	ready, exit := liveRuntimePolicyReadiness(fixture.manifest, route, fixture.accounts(t, route))
	if !ready || !exit {
		t.Fatalf("healthy candidate batch was not AUTO ready: ready=%t exit=%t", ready, exit)
	}
}

func TestAutoCandidateReadinessRejectsDriftedPins(t *testing.T) {
	fixture := newAutoReadinessFixture(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	base := fixture.accounts(t, route)
	clone := func(accounts []ConfirmedAccount) []ConfirmedAccount {
		out := make([]ConfirmedAccount, 0, len(accounts))
		for _, account := range accounts {
			account.Data = append([]byte(nil), account.Data...)
			out = append(out, account)
		}
		return out
	}
	find := func(accounts []ConfirmedAccount, address string) *ConfirmedAccount {
		for index := range accounts {
			if accounts[index].Address == address {
				return &accounts[index]
			}
		}
		t.Fatalf("pin account %s missing from the batch", address)
		return nil
	}
	expectNotReady := func(name string, accounts []ConfirmedAccount) {
		t.Helper()
		ready, exit := liveRuntimePolicyReadiness(fixture.manifest, route, accounts)
		if ready || exit {
			t.Fatalf("%s left AUTO candidate readiness true", name)
		}
	}
	combined := fixture.binding.Policy
	missing := clone(base)
	for index, account := range missing {
		if account.Address == combined {
			missing = append(missing[:index], missing[index+1:]...)
			break
		}
	}
	expectNotReady("missing combined policy account", missing)
	wrongOwner := clone(base)
	find(wrongOwner, combined).Owner = bridgeVault
	expectNotReady("wrong combined owner", wrongOwner)
	executable := clone(base)
	find(executable, combined).Executable = true
	expectNotReady("executable combined policy", executable)
	unfunded := clone(base)
	find(unfunded, combined).Lamports = 0
	expectNotReady("unfunded combined policy", unfunded)
	drifted := clone(base)
	find(drifted, combined).Data[100] ^= 0x40
	expectNotReady("drifted combined bytes", drifted)
	var bridgeAddress string
	for address := range fixture.masks {
		if address != bindingAddressFor(t, fixture, VoltrAllocateToSquads) {
			bridgeAddress = address
			break
		}
	}
	if bridgeAddress == "" {
		t.Fatal("fixture lost its fixture-bound bridge pins")
	}
	lostBridge := clone(base)
	for index, account := range lostBridge {
		if account.Address == bridgeAddress {
			lostBridge = append(lostBridge[:index], lostBridge[index+1:]...)
			break
		}
	}
	expectNotReady("missing bridge pin", lostBridge)
	driftedBridge := clone(base)
	find(driftedBridge, bridgeAddress).Data[32] ^= 0x20
	expectNotReady("drifted bridge bytes", driftedBridge)

	// Charging or restamping a bridge limit must never break readiness: the
	// masked ranges stay excluded by the same comparator the send path uses.
	charged := clone(base)
	for index := range charged {
		if mask, ok := fixture.masks[charged[index].Address]; ok {
			applyPolicyMask(charged[index].Data, mask, 0x7f)
		}
	}
	if ready, exit := liveRuntimePolicyReadiness(fixture.manifest, route, charged); !ready || !exit {
		t.Fatalf("charged bridge policies broke AUTO candidate readiness: ready=%t exit=%t", ready, exit)
	}
}

func bindingAddressFor(t *testing.T, fixture *autoReadinessFixture, action Action) string {
	t.Helper()
	binding, err := fixture.manifest.bridgePolicy(action)
	if err != nil {
		t.Fatal(err)
	}
	return binding.Account
}

// TestAutoReadinessNeverFallsBackToHistoricalShards pins the readiness
// closure in both binding states: the explicit absent-binding fixture (the
// shipped pre-install state) never lets historical shard accounts establish
// readiness, while the embedded manifest — which carries the installed
// binding after the release — resolves exactly that installed binding.
func TestAutoReadinessNeverFallsBackToHistoricalShards(t *testing.T) {
	embedded := requireEmbeddedInstalledBinding(t)
	absent := autoAbsentBindingManifest(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	// Every historical shard account present, funded, correctly owned - and
	// even a combined-shaped account. Without the reviewed binding none of it
	// establishes readiness.
	accounts := []ConfirmedAccount{}
	for _, b := range route.KaminoPolicies {
		accounts = append(accounts, ConfirmedAccount{Address: b.Policy, Owner: bridgeSquadsProgram, Lamports: 1, Data: bytes.Repeat([]byte{5}, 2849)})
	}
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		b, err := catalogJupiterBindingForRoute(action, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, ConfirmedAccount{Address: b.Policy, Owner: bridgeSquadsProgram, Lamports: 1, Data: bytes.Repeat([]byte{5}, 96)})
	}
	accounts = append(accounts, ConfirmedAccount{Address: autoFixturePolicy, Owner: bridgeSquadsProgram, Lamports: 1, Data: bytes.Repeat([]byte{5}, 2849)})
	pins, err := catalogRoutePolicyPins(route, absent)
	if err == nil {
		t.Fatalf("absent AUTO binding resolved a readiness pin set: %v", pins)
	}
	assertBudgetHold(t, err, "auto_policy_not_activated")
	if ready, exit := liveRuntimePolicyReadiness(absent, route, accounts); ready || exit {
		t.Fatal("historical shards established AUTO readiness without the reviewed binding")
	}
	// The installed manifest resolves its pin set from the exact installed
	// binding — and the same shard accounts still fail readiness against it,
	// because none of them is the installed policy account.
	installedPins, err := catalogRoutePolicyPins(route, embedded)
	if err != nil {
		t.Fatalf("installed binding did not resolve a readiness pin set: %v", err)
	}
	if pin, ok := installedPins[installedAutoPolicyKey]; !ok || pin.digest != installedAutoPolicyDigest {
		t.Fatalf("installed pin set lost the installed policy account: %v", reflect.ValueOf(installedPins).MapKeys())
	}
	if ready, exit := liveRuntimePolicyReadiness(embedded, route, accounts); ready || exit {
		t.Fatal("historical shards established AUTO readiness under the installed binding")
	}
}

func TestInstalledCatalogLanesKeepTheirPinSets(t *testing.T) {
	embedded, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"Ethena/USDe/PYUSD", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		pins, err := catalogRoutePolicyPins(route, embedded)
		if err != nil {
			t.Fatal(err)
		}
		// Pin the actual installed identities, digests and masks - not a
		// fixed cardinality: lanes may legitimately share one policy account
		// across edges (Prime/PRIME/USDS folds PRIME->USDC and PRIME->USDS
		// into one pin), so the set is only as large as its distinct
		// addresses.
		distinct := map[string]bool{}
		for _, b := range route.KaminoPolicies {
			distinct[b.Policy] = true
			pin, ok := pins[b.Policy]
			if !ok || pin.digest != b.DataSHA256 || len(pin.mask) != 0 {
				t.Fatalf("%s lost its installed Kamino shard pin %s: %+v", lane, b.Policy, pin)
			}
		}
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			b, err := catalogJupiterBindingForRoute(action, route.Lane)
			if err != nil {
				t.Fatal(err)
			}
			distinct[b.Policy] = true
			pin, ok := pins[b.Policy]
			if !ok || pin.digest != b.PolicySHA256 || len(pin.mask) != 0 {
				t.Fatalf("%s lost its installed swap edge pin %s (%s): %+v", lane, b.Policy, action, pin)
			}
		}
		for _, b := range embedded.RuntimeBindings.BridgePolicies {
			distinct[b.Account] = true
			pin, ok := pins[b.Account]
			if !ok || pin.digest != b.NormalizedDigest || !reflect.DeepEqual(pin.mask, b.MaskedByteRanges) {
				t.Fatalf("%s lost its masked bridge pin %s: %+v", lane, b.Account, pin)
			}
		}
		if len(pins) != len(distinct) {
			t.Fatalf("%s resolved %d pins, want exactly the %d distinct installed identities", lane, len(pins), len(distinct))
		}
		if ready, exit := liveRuntimePolicyReadiness(embedded, route, nil); ready || exit {
			t.Fatalf("%s readiness passed without any accounts", lane)
		}
	}
}

func TestAutoCandidateFixedAddressesTrackTheCombinedBinding(t *testing.T) {
	fixture := newAutoReadinessFixture(t)
	fixture.manifest.RuntimeActivation.SelectedLane = autoAUTOPYUSD.Lane
	route, err := fixture.manifest.activeRuntimeRoute()
	if err != nil || route.Lane != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate manifest did not select the AUTO route: %v, %v", route, err)
	}
	set := map[string]bool{}
	for _, address := range routeFixedAddresses(fixture.manifest) {
		set[address] = true
	}
	if !set[fixture.binding.Policy] {
		t.Fatal("candidate batch omits the combined policy account")
	}
	for address := range fixture.masks {
		if !set[address] {
			t.Fatalf("candidate batch omits bridge pin %s", address)
		}
	}
	for _, b := range route.KaminoPolicies {
		if set[b.Policy] {
			t.Fatalf("obsolete Kamino shard %s is still requested", b.Policy)
		}
	}
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		b, err := catalogJupiterBindingForRoute(action, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		if set[b.Policy] {
			t.Fatalf("obsolete catalog swap shard %s is still requested", b.Policy)
		}
	}
	// Protocol and NAV identities survive untouched.
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
		if !set[address] {
			t.Fatalf("NAV identity %s omitted from the candidate batch", address)
		}
	}
	if !set[route.CollateralLiquiditySupply] || !set[route.DebtLiquiditySupply] {
		t.Fatal("candidate batch omitted the route liquidity supplies")
	}

	// An absent binding must fail readiness without tearing down the rest of
	// the confirmed batch or quietly requiring the old shard accounts.
	plain, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	plain.RuntimeActivation.SelectedLane = autoAUTOPYUSD.Lane
	bare := routeFixedAddresses(plain)
	if len(bare) == 0 {
		t.Fatal("absent AUTO binding tore down the observation address set")
	}
	bareSet := map[string]bool{}
	for _, address := range bare {
		bareSet[address] = true
	}
	if bareSet[fixture.binding.Policy] {
		t.Fatal("absent binding still requested a combined policy account")
	}
	for _, b := range route.KaminoPolicies {
		if bareSet[b.Policy] {
			t.Fatalf("absent binding fell back to requesting shard %s", b.Policy)
		}
	}
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
		if !bareSet[address] {
			t.Fatalf("absent binding dropped NAV identity %s", address)
		}
	}
}
