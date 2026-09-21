package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The candidate seed/address/hash below are the local fixture values proven by
// the deployed-Squads semantics test (seed 900). Production values arrive only
// when the coordinator adds the reviewed autoPolicy manifest section; nothing
// here writes production configuration.
const (
	autoFixtureSeed   = uint64(900)
	autoFixturePolicy = "3boNKi29ewKLBybQERZUFeGuueFykVsXP5BEahtoCCH5"
	autoFixtureHash   = "aec4b9fb8bc9d30ed0ac4242306165b89ab1bc44d81dd63b5f535196c3655660"
)

func autoFixtureConstraintIndices() map[string]byte {
	out := make(map[string]byte, len(autoConstraintIndices))
	for key, index := range autoConstraintIndices {
		out[key] = index
	}
	return out
}

func autoFixtureBinding(t *testing.T) AutoPolicyBinding {
	t.Helper()
	derived, err := derivePolicyAccount(autoFixtureSeed)
	if err != nil {
		t.Fatal(err)
	}
	if derived != autoFixturePolicy {
		t.Fatalf("fixture seed %d derives %s, not the proven policy address", autoFixtureSeed, derived)
	}
	return AutoPolicyBinding{Lane: autoAUTOPYUSD.Lane, PolicySeed: autoFixtureSeed, Policy: derived,
		AccountDataSHA256: autoFixtureHash, ConstraintIndices: autoFixtureConstraintIndices()}
}

func autoFixtureManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	binding := autoFixtureBinding(t)
	manifest.RuntimeBindings.AutoPolicy = &binding
	return manifest
}

// The exact installed binding the release manifest carries (Policy156, the
// coordinator-reviewed on-chain install): the fixture derives the policy
// address from the seed so a constant typo can never masquerade as the
// installed identity.
const (
	installedAutoPolicySeed   = uint64(156)
	installedAutoPolicyKey    = "H6X87EqwDcM2qigQ4SadS3uWFozkkGSwvUxDuYYcD92q"
	installedAutoPolicyDigest = "80b702a28425a937a6e852b71b4e3c85b24cebb2170df1f717253015f8261a78"
)

func installedAutoFixtureBinding(t *testing.T) AutoPolicyBinding {
	t.Helper()
	derived, err := derivePolicyAccount(installedAutoPolicySeed)
	if err != nil {
		t.Fatal(err)
	}
	if derived != installedAutoPolicyKey {
		t.Fatalf("installed seed %d derives %s, not the installed policy address", installedAutoPolicySeed, derived)
	}
	indices := autoFixtureConstraintIndices()
	indices[autoInitializerConstraintKey] = autoInitializerConstraintIndex
	return AutoPolicyBinding{Lane: autoAUTOPYUSD.Lane, PolicySeed: installedAutoPolicySeed, Policy: derived,
		AccountDataSHA256: installedAutoPolicyDigest, ConstraintIndices: indices}
}

// autoAbsentBindingManifest is the EXPLICIT absent-binding fixture: the
// embedded manifest with the AUTO binding cleared. The shipped pre-install
// state is represented by this fixture, never inferred from the embedded
// manifest, which carries the installed binding after the release.
func autoAbsentBindingManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.RuntimeBindings.AutoPolicy = nil
	return manifest
}

// requireEmbeddedInstalledBinding asserts the embedded manifest resolves the
// EXACT installed binding — lane, seed, policy, account digest and every
// constraint index — and returns the loaded manifest for further assertions.
// An embedded manifest without the binding, or with any drifted value, fails.
func requireEmbeddedInstalledBinding(t *testing.T) RouteManifest {
	t.Helper()
	embedded, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := embedded.autoPolicyBinding()
	if err != nil {
		t.Fatalf("embedded manifest does not carry the installed AUTO binding: %v", err)
	}
	want := installedAutoFixtureBinding(t)
	if got.Lane != want.Lane || got.PolicySeed != want.PolicySeed || got.Policy != want.Policy || got.AccountDataSHA256 != want.AccountDataSHA256 || len(got.ConstraintIndices) != len(want.ConstraintIndices) {
		t.Fatalf("embedded AUTO binding drifted from the installed binding: got %+v want %+v", got, want)
	}
	for key, index := range want.ConstraintIndices {
		if got.ConstraintIndices[key] != index {
			t.Fatalf("embedded AUTO binding constraint %s = %d, want %d", key, got.ConstraintIndices[key], index)
		}
	}
	return embedded
}

// The initializer-enabled candidate is the SAME policy family at the
// superseding seed 901 with the appended eighth (initialize) constraint. The
// unit fixture's AccountDataSHA256 is SYNTHETIC: it hashes the controlled
// account bytes below so prestate tests can observe a coherent account. The
// real deployed account hash arrives from the semantic capture
// (auto-single-policy-semantics-proof.json records instructionDataSha256 and
// accountDataSha256 separately — the create-instruction hash is NEVER the
// account hash) and supersedes this value before any production binding.
const (
	autoInitializerFixtureSeed                 = uint64(901)
	autoInitializerFixturePolicy               = "F4V3EwtkJQr8iELDtdND6v6mkL4EPdSCxSVKPhc76MYa"
	autoInitializerFixtureSyntheticAccountData = "auto-initializer-candidate-policy-account"
)

func autoInitializerFixtureBinding(t *testing.T) AutoPolicyBinding {
	t.Helper()
	derived, err := derivePolicyAccount(autoInitializerFixtureSeed)
	if err != nil {
		t.Fatal(err)
	}
	if derived != autoInitializerFixturePolicy {
		t.Fatalf("initializer fixture seed %d derives %s, not the superseding candidate address", autoInitializerFixtureSeed, derived)
	}
	indices := autoFixtureConstraintIndices()
	indices[autoInitializerConstraintKey] = autoInitializerConstraintIndex
	return AutoPolicyBinding{Lane: autoAUTOPYUSD.Lane, PolicySeed: autoInitializerFixtureSeed, Policy: derived,
		AccountDataSHA256: sha256Bytes([]byte(autoInitializerFixtureSyntheticAccountData)), ConstraintIndices: indices}
}

func autoInitializerFixtureManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	binding := autoInitializerFixtureBinding(t)
	manifest.RuntimeBindings.AutoPolicy = &binding
	return manifest
}

// autoVenueKey deterministically derives a non-authority venue account for
// fixture instructions. Patterns never collide with the pinned identities.
func autoVenueKey(index byte) string {
	pattern := bytes.Repeat([]byte{0xA0 ^ index}, 32)
	pattern[0] = index + 1
	return encodeBase58(pattern)
}

// autoJupiterTestInstruction builds a legacy SharedAccountsRoute instruction on
// one approved AUTO edge with the exact reviewed boundaries (0 classic token
// program, 2 vault signer, 3 source custody, 6 destination custody, 7/8 mints,
// 9 Jupiter platform-fee pin) plus `filler` venue accounts.
func autoJupiterTestInstruction(t *testing.T, action Action, amount, out uint64, filler int) JupiterSwapInstruction {
	t.Helper()
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(action, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	accounts := make([]JupiterInstructionAccount, 10+filler)
	for index := range accounts {
		accounts[index] = JupiterInstructionAccount{Pubkey: autoVenueKey(byte(index))}
	}
	accounts[0] = JupiterInstructionAccount{Pubkey: classicTokenProgram}
	accounts[2] = JupiterInstructionAccount{Pubkey: bridgeVault, IsSigner: true}
	accounts[3] = JupiterInstructionAccount{Pubkey: sourceATA, IsWritable: true}
	accounts[6] = JupiterInstructionAccount{Pubkey: destinationATA, IsWritable: true}
	accounts[7] = JupiterInstructionAccount{Pubkey: sourceMint}
	accounts[8] = JupiterInstructionAccount{Pubkey: destinationMint}
	accounts[9] = JupiterInstructionAccount{Pubkey: jupiterV6Program}
	data := make([]byte, 37)
	copy(data, jupiterSharedAccountsRoute)
	plan, err := hex.DecodeString("01010000007400640001")
	if err != nil {
		t.Fatal(err)
	}
	copy(data[8:18], plan)
	for index := 0; index < 8; index++ {
		data[18+index] = byte(amount >> (8 * index))
		data[26+index] = byte(out >> (8 * index))
	}
	data[34], data[35], data[36] = 50, 0, 0
	return JupiterSwapInstruction{ProgramID: jupiterV6Program, Accounts: accounts, Data: base64.StdEncoding.EncodeToString(data)}
}

func autoJupiterTestRequest(t *testing.T, action Action, amount, out uint64, filler int) JupiterSwapRequest {
	t.Helper()
	return autoJupiterTestRequestWithBinding(t, autoFixtureBinding(t), action, amount, out, filler)
}

func autoJupiterTestRequestWithBinding(t *testing.T, binding AutoPolicyBinding, action Action, amount, out uint64, filler int) JupiterSwapRequest {
	t.Helper()
	index, err := autoSwapConstraintKey(action)
	if err != nil {
		t.Fatal(err)
	}
	// The honest enforceable minimum for the wire below (legacy dialect, data
	// slippage 50): quoted output scaled by (10000-50)/10000, never the
	// advisory quoted output itself.
	return JupiterSwapRequest{Action: action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: out * 9950 / 10000,
		Policy: binding.Policy, PolicyAccountDataSHA256: binding.AccountDataSHA256,
		PolicyConstraintIndex: binding.ConstraintIndices[index],
		Instruction:           autoJupiterTestInstruction(t, action, amount, out, filler),
		RecentBlockhash:       bridgeSettings, LastValidBlockHeight: 99, RouteLane: autoAUTOPYUSD.Lane}
}

func TestAutoFixtureSeedAgreesWithDerivedPolicyAccount(t *testing.T) {
	binding := autoFixtureBinding(t)
	if binding.PolicySeed != 900 || binding.Policy != autoFixturePolicy {
		t.Fatalf("fixture seed/address drifted: %+v", binding)
	}
	for _, seed := range []uint64{141, 142, 143, 144} {
		derived, err := derivePolicyAccount(seed)
		if err != nil {
			t.Fatal(err)
		}
		if derived == autoFixturePolicy {
			t.Fatalf("basic seed %d collides with the AUTO fixture policy", seed)
		}
	}
}

func TestAutoPolicyBindingValidationRejectsDrift(t *testing.T) {
	base := autoFixtureBinding(t)
	mutate := func(f func(*AutoPolicyBinding)) AutoPolicyBinding {
		next := base
		next.ConstraintIndices = autoFixtureConstraintIndices()
		f(&next)
		return next
	}
	tests := []struct {
		name string
		got  AutoPolicyBinding
		want string
	}{
		{"wrong lane", mutate(func(b *AutoPolicyBinding) { b.Lane = SelectedRouteID }), "is not"},
		{"unset seed", mutate(func(b *AutoPolicyBinding) { b.PolicySeed = 0 }), "seed is unset"},
		{"basic seed alias", mutate(func(b *AutoPolicyBinding) {
			b.PolicySeed = basicPolicySeeds[BasicCollateralLifecycle]
			b.Policy = autoFixturePolicy
		}), "aliases installed"},
		{"seed and address disagree", mutate(func(b *AutoPolicyBinding) { b.Policy = autoVenueKey(1) }), "disagree"},
		{"malformed hash", mutate(func(b *AutoPolicyBinding) { b.AccountDataSHA256 = "nothex" }), "hash is malformed"},
		{"short hash", mutate(func(b *AutoPolicyBinding) { b.AccountDataSHA256 = "abc4" }), "hash is malformed"},
		{"constraint count", mutate(func(b *AutoPolicyBinding) { delete(b.ConstraintIndices, "repay") }), "exactly the seven"},
		// Regression: dropping deposit (expected 0) and adding one foreign key
		// preserves len==7; the presence check must still reject it.
		{"missing deposit behind extra key", mutate(func(b *AutoPolicyBinding) {
			delete(b.ConstraintIndices, "deposit")
			b.ConstraintIndices["initializer"] = 7
		}), `missing proven constraint "deposit"`},
		{"drifted deposit index", mutate(func(b *AutoPolicyBinding) { b.ConstraintIndices["deposit"] = 5 }), `constraint "deposit" drifted`},
		{"drifted swap index", mutate(func(b *AutoPolicyBinding) { b.ConstraintIndices["swapPYUSDToUSDC"] = 4 }), `constraint "swapPYUSDToUSDC" drifted`},
		{"drifted initializer index", func() AutoPolicyBinding {
			next := autoInitializerFixtureBinding(t)
			next.ConstraintIndices[autoInitializerConstraintKey] = 6
			return next
		}(), "initializer drifted from the appended index"},
		{"foreign extra key in initializer slot", func() AutoPolicyBinding {
			next := autoInitializerFixtureBinding(t)
			delete(next.ConstraintIndices, autoInitializerConstraintKey)
			next.ConstraintIndices["initializer"] = 7
			return next
		}(), `unproven constraint "initializer"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateAutoPolicyBinding(test.got); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %q; got %v", test.want, err)
			}
			manifest, err := loadEmbeddedRouteManifest()
			if err != nil {
				t.Fatal(err)
			}
			binding := test.got
			manifest.RuntimeBindings.AutoPolicy = &binding
			if _, err := manifest.autoPolicyBinding(); err == nil {
				t.Fatal("drifted binding resolved")
			}
		})
	}
}

// TestAbsentAutoBindingHoldsAutoPathsAndKeepsInstalledLanes pins BOTH binding
// states. The explicit absent-binding fixture (the shipped pre-install state)
// must keep every AUTO path held closed with the exact typed hold, while the
// embedded manifest — which carries the installed binding after the release —
// must resolve exactly that installed binding and nothing else.
func TestAbsentAutoBindingHoldsAutoPathsAndKeepsInstalledLanes(t *testing.T) {
	requireEmbeddedInstalledBinding(t)
	absent := autoAbsentBindingManifest(t)
	if _, err := absent.autoPolicyBinding(); err != nil {
		assertBudgetHold(t, err, "auto_policy_not_activated")
	} else {
		t.Fatal("absent AUTO binding resolved")
	}
	// The catalog knows every AUTO pair, including the two the reviewed combined
	// policy does not authorize; absence must hold, never fall through to it.
	for _, action := range []Action{SwapStableToCollateralStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		if _, err := catalogJupiterBindingForRoute(action, autoAUTOPYUSD.Lane); err != nil {
			t.Fatalf("catalog AUTO observation pin for %s disappeared: %v", action, err)
		}
		_, err := absent.jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane)
		if action == SwapUSDCToDebtStep {
			// The excluded pair fails on edge rejection before any binding
			// lookup; the catalog entry is never reached either way.
			if err == nil || !strings.Contains(err.Error(), "not an approved AUTO") {
				t.Fatalf("excluded catalog pair %s resolved: %v", action, err)
			}
			continue
		}
		if err != nil {
			assertBudgetHold(t, err, "auto_policy_not_activated")
		} else {
			t.Fatalf("AUTO %s resolved without a reviewed binding", action)
		}
	}
	_, err := absent.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 9}, autoAUTOPYUSD.Lane)
	assertBudgetHold(t, err, "auto_policy_not_activated")
	// A structurally empty AUTO request fails closed before the binding hold
	// (packet shape is checked first); the exact-identity wrapper hold is
	// asserted with a complete request in the candidate tests below.
	if _, err = CompileKaminoMessage(KaminoPrimeUSDCRequest{RouteLane: autoAUTOPYUSD.Lane}); err == nil {
		t.Fatal("empty AUTO Kamino request compiled")
	}
	// Same for Jupiter: an empty AUTO request fails closed on request shape;
	// the exact-identity wrapper hold is asserted in the candidate tests.
	if _, err = CompileJupiterMessage(JupiterSwapRequest{RouteLane: autoAUTOPYUSD.Lane}); err == nil {
		t.Fatal("empty AUTO Jupiter request compiled")
	}
	// The shipped lanes stay byte-identical: Maple catalog policies and the
	// basic swap family still resolve through their own bindings, independent
	// of the AUTO binding state.
	if binding, err := absent.jupiterPolicyForRoute(SwapUSDCToPrimeStep, ""); err != nil || binding.Policy == "" {
		t.Fatalf("installed forward Jupiter binding broke: %v", err)
	}
	fixture := basicPolicyFixtureManifest(t)
	binding, err := fixture.jupiterPolicyForRoute(SwapStableToCollateralStep, SelectedRouteID)
	if err != nil || !binding.BasicPolicy {
		t.Fatalf("basic lane binding broke while AUTO stayed unbound: %v", err)
	}
	if _, err := fixture.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 9}, SelectedRouteID); err != nil {
		t.Fatalf("basic lane Kamino packet broke while AUTO stayed unbound: %v", err)
	}
}

// TestEmbeddedManifestCarriesExactInstalledAutoBinding pins the installed
// state: the embedded release manifest resolves exactly the installed
// Policy156 binding — the complete eight-constraint initializer set included —
// so the AUTO lane authority, funding scope and initializer binding all admit
// through the production manifest, while the explicit absent fixture keeps
// each gate closed.
func TestEmbeddedManifestCarriesExactInstalledAutoBinding(t *testing.T) {
	embedded := requireEmbeddedInstalledBinding(t)
	if _, index, err := embedded.autoInitializerBinding(); err != nil || index != autoInitializerConstraintIndex {
		t.Fatalf("installed binding did not resolve the appended initializer constraint: index=%d err=%v", index, err)
	}
	if !embedded.selectorEntryLaneAllowed(autoAUTOPYUSD.Lane) {
		t.Fatal("installed manifest did not admit the candidate AUTO lane authority")
	}
	if !embedded.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) || !embedded.selectorEntryFundingLane(autoAUTOPYUSD.Lane, true) {
		t.Fatal("installed manifest did not admit funded AUTO allocation")
	}
	absent := autoAbsentBindingManifest(t)
	if absent.selectorEntryLaneAllowed(autoAUTOPYUSD.Lane) || absent.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) || absent.selectorEntryFundingLane(autoAUTOPYUSD.Lane, true) {
		t.Fatal("absent binding admitted the AUTO lane authority or funding")
	}
}

func TestAutoKaminoCandidateCompilesOnlyAgainstTheReviewedBinding(t *testing.T) {
	manifest := autoFixtureManifest(t)
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	if request.Policy != autoFixturePolicy || request.PolicyAccountDataSHA256 != autoFixtureHash || request.PolicyConstraintIndex != 0 {
		t.Fatalf("candidate packet did not carry the reviewed binding: %+v", request)
	}
	delegate := mustKey(bridgeDelegate)
	compiled, err := manifest.compileKaminoMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("AUTO seed-900 Kamino deposit message = %d bytes (+65 signature = %d of %d packet bytes)", len(compiled), len(compiled)+65, solanaPacketBytes)
	if compiled[0] != 1 {
		t.Fatal("fitting AUTO lifecycle leg left the legacy envelope")
	}
	// The candidate wire is assembled exactly as the signer would, from the
	// manifest-aware compile: the production wrapper itself must still hold
	// until the coordinator's manifest ships (asserted below).
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	wire := append(encodeShortVec(1), ed25519.Sign(key, compiled)...)
	wire = append(wire, compiled...)
	if len(wire) > solanaPacketBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), compiled, wire[1:1+ed25519.SignatureSize]) {
		t.Fatal("AUTO lifecycle wire is not a bounded one-signer packet")
	}
	// The production sign wrapper reloads the embedded manifest. Before the
	// release it held with auto_policy_not_activated; with the installed
	// binding shipped, the candidate identity is refused as a binding mismatch
	// — still closed, but named exactly.
	_, err = buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, publicKeyFromBytes(key.Public().(ed25519.PublicKey)))
	if err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("production sign wrapper did not refuse the candidate identity: %v", err)
	}
	// Request-supplied look-alike values are rejected: identity is retained from
	// the reviewed manifest, never from the request.
	wrongPolicy := request
	wrongPolicy.Policy = mustEncodeKey(t, basicPolicySeeds[BasicCollateralLifecycle])
	if _, err := manifest.compileKaminoMessage(wrongPolicy, delegate); err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("foreign policy address compiled: %v", err)
	}
	wrongHash := request
	wrongHash.PolicyAccountDataSHA256 = strings.Repeat("0", 64)
	if _, err := manifest.compileKaminoMessage(wrongHash, delegate); err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("foreign policy hash compiled: %v", err)
	}
	wrongIndex := request
	wrongIndex.PolicyConstraintIndex = 1
	if _, err := manifest.compileKaminoMessage(wrongIndex, delegate); err == nil {
		t.Fatal("wrong constraint index compiled")
	}
	// The production wrapper reloads the embedded manifest. Before the release
	// that manifest had no AUTO binding and this request held with
	// auto_policy_not_activated; with the installed binding shipped, the same
	// candidate identity is refused as a binding mismatch — still closed, but
	// named exactly.
	_, err = CompileKaminoMessage(request)
	if err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("production wrapper did not refuse the candidate identity: %v", err)
	}
	// The installed state compiles end to end: a packet request resolved
	// through the embedded manifest carries exactly the installed identity and
	// the production wrapper compiles it without any request-supplied values.
	installed := requireEmbeddedInstalledBinding(t)
	installedRequest, err := installed.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	want := installedAutoFixtureBinding(t)
	if installedRequest.Policy != want.Policy || installedRequest.PolicyAccountDataSHA256 != want.AccountDataSHA256 || installedRequest.PolicyConstraintIndex != want.ConstraintIndices["deposit"] {
		t.Fatalf("installed packet lost the installed binding: %+v", installedRequest)
	}
	installedCompiled, err := CompileKaminoMessage(installedRequest)
	if err != nil {
		t.Fatalf("production wrapper refused the installed binding: %v", err)
	}
	if embeddedCompiled, err := installed.compileKaminoMessage(installedRequest, delegate); err != nil || !bytes.Equal(installedCompiled, embeddedCompiled) {
		t.Fatalf("installed wrapper drifted from the manifest compile: %v", err)
	}
	// The absent-binding fixture keeps the exact shipped hold: the same request
	// shape cannot resolve a packet at all without a binding.
	_, absentErr := autoAbsentBindingManifest(t).kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	assertBudgetHold(t, absentErr, "auto_policy_not_activated")
}

func mustEncodeKey(t *testing.T, seed uint64) string {
	t.Helper()
	policy, err := derivePolicyAccount(seed)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestAutoJupiterCandidateCompilesOnlyAgainstTheReviewedBinding(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegate := mustKey(bridgeDelegate)
	request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	if request.PolicyConstraintIndex != 4 {
		t.Fatalf("USDC->AUTO edge did not select constraint 4: %d", request.PolicyConstraintIndex)
	}
	compiled, err := manifest.compileJupiterMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if compiled[0] != 1 {
		t.Fatal("fitting AUTO swap left the legacy envelope")
	}
	t.Logf("AUTO seed-900 one-hop USDC->AUTO swap message = %d bytes (+65 signature = %d of %d packet bytes)", len(compiled), len(compiled)+65, solanaPacketBytes)
	// Every drift the reviewed binding forbids must fail closed.
	wrongPolicy := request
	wrongPolicy.Policy = mustEncodeKey(t, basicPolicySeeds[BasicDebtLifecycle])
	if _, err := manifest.compileJupiterMessage(wrongPolicy, delegate); err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("foreign policy address compiled: %v", err)
	}
	wrongHash := request
	wrongHash.PolicyAccountDataSHA256 = strings.Repeat("f", 64)
	if _, err := manifest.compileJupiterMessage(wrongHash, delegate); err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("foreign policy hash compiled: %v", err)
	}
	wrongIndex := request
	wrongIndex.PolicyConstraintIndex = 5
	if _, err := manifest.compileJupiterMessage(wrongIndex, delegate); err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("foreign constraint index compiled: %v", err)
	}
	// USDC->PYUSD is a known catalog pair for this lane but not one of the five
	// reviewed edges: the request cannot smuggle it in.
	foreign := request
	foreign.Action = SwapUSDCToDebtStep
	if _, err := manifest.compileJupiterMessage(foreign, delegate); err == nil || !strings.Contains(err.Error(), "not an approved AUTO") {
		t.Fatalf("excluded USDC->PYUSD pair compiled: %v", err)
	}
	// The combined policy pins the legacy SharedAccountsRoute dialect.
	v2 := request
	v2Data, _ := base64.StdEncoding.Strict().DecodeString(v2.Instruction.Data)
	copy(v2Data[:8], jupiterSharedAccountsRouteV2)
	v2.Instruction.Data = base64.StdEncoding.EncodeToString(v2Data)
	if _, err := manifest.compileJupiterMessage(v2, delegate); err == nil || !strings.Contains(err.Error(), "requires legacy sharedAccountsRoute") {
		t.Fatalf("V2 dialect compiled under the combined policy: %v", err)
	}
	// Boundary 9 stays the Jupiter platform-fee pin. The account slice is
	// shared between request copies, so the drift case needs its own vector.
	drifted := request
	drifted.Instruction.Accounts = append([]JupiterInstructionAccount(nil), request.Instruction.Accounts...)
	drifted.Instruction.Accounts[9].Pubkey = classicTokenProgram
	if _, err := manifest.compileJupiterMessage(drifted, delegate); err == nil || !strings.Contains(err.Error(), "boundary 9 drifted") {
		t.Fatalf("platform-fee pin drift compiled: %v", err)
	}
	// No request-only authorization: before the release, identical exact values
	// held with auto_policy_not_activated because the embedded manifest carried
	// no binding; with the installed binding shipped, the same candidate values
	// are refused as a binding mismatch — still closed, never the catalog.
	_, err = CompileJupiterMessage(request)
	if err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("production wrapper did not refuse the candidate identity: %v", err)
	}
	// The installed state compiles end to end through the production wrapper,
	// with every identity retained from the embedded manifest.
	installed := requireEmbeddedInstalledBinding(t)
	installedRequest := autoJupiterTestRequestWithBinding(t, installedAutoFixtureBinding(t), SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	installedCompiled, err := CompileJupiterMessage(installedRequest)
	if err != nil {
		t.Fatalf("production wrapper refused the installed binding: %v", err)
	}
	if manifestCompiled, err := installed.compileJupiterMessage(installedRequest, delegate); err != nil || !bytes.Equal(installedCompiled, manifestCompiled) {
		t.Fatalf("installed wrapper drifted from the manifest compile: %v", err)
	}
	// The absent-binding fixture keeps the exact shipped hold on the same edge.
	_, absentErr := autoAbsentBindingManifest(t).jupiterPolicyForRoute(SwapStableToCollateralStep, autoAUTOPYUSD.Lane)
	assertBudgetHold(t, absentErr, "auto_policy_not_activated")
	// Binding resolution marks the source so the fresh-header check cannot be
	// satisfied by catalog metadata.
	binding, err := manifest.jupiterPolicyForRoute(SwapStableToCollateralStep, autoAUTOPYUSD.Lane)
	if err != nil || !binding.AutoPolicy || binding.CatalogLane != "" || binding.PolicyConstraintIndex != 4 {
		t.Fatalf("AUTO binding resolution drifted: %+v %v", binding, err)
	}
}

func TestAutoManifestsStayIndependentAndSelectorGatesHold(t *testing.T) {
	if selectorLane(autoAUTOPYUSD.Lane) {
		t.Fatal("candidate AUTO lane became a selector lane")
	}
	candidate := autoFixtureManifest(t)
	requireEmbeddedInstalledBinding(t)
	if _, err := candidate.autoPolicyBinding(); err != nil {
		t.Fatalf("candidate binding stopped resolving: %v", err)
	}
	// The absent state stays explicitly constructible and held.
	if _, err := autoAbsentBindingManifest(t).autoPolicyBinding(); err == nil {
		t.Fatal("absent-binding fixture resolved an AUTO binding")
	}
	// Independence: rebuilding the manifest after the candidate was built still
	// resolves exactly the installed binding — no package-global AUTO state,
	// and mutating one manifest never reaches another.
	again := requireEmbeddedInstalledBinding(t)
	candidate.RuntimeBindings.AutoPolicy = nil
	if _, err := again.autoPolicyBinding(); err != nil {
		t.Fatal("candidate mutation reached an independent manifest")
	}
	if onceMore := requireEmbeddedInstalledBinding(t); onceMore.RuntimeBindings.AutoPolicy == nil {
		t.Fatal("candidate mutation reached a fresh load")
	}
	observation := autoFixtureManifest(t)
	observation.selectorObservation = true
	observation.observationLane = autoAUTOPYUSD.Lane
	if _, err := observation.activeRuntimeRoute(); err == nil || !strings.Contains(err.Error(), "unadmitted observation lane") {
		t.Fatalf("candidate AUTO lane slipped into observation activation: %v", err)
	}
	// The catalog entry stays an observation pin; construction still resolves
	// only through the reviewed binding.
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	if route.KaminoPolicies[kaminoLegDeposit].Policy != "651fFC9yEuWmSjKKswxeW8xe9HWpVyoDLqk6J3uRTiVh" {
		t.Fatal("AUTO observation pins were consumed by the binding work")
	}
}

// autoFixtureLookupTable fabricates one chain-shaped address-lookup-table
// account carrying exactly the supplied venue keys.
func autoFixtureLookupTable(t *testing.T, entries []string) LookupTableSnapshot {
	t.Helper()
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint64(body[4:12], ^uint64(0))
	for _, entry := range entries {
		key, err := decodeKey(entry)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, key[:]...)
	}
	table := LookupTableSnapshot{Address: autoVenueKey(0xEE), Owner: addressLookupTableProgram,
		Lamports: 56960640, Data: body, ObservedSlot: 1_000_000}
	if _, err := decodeMessageLookupTable(table); err != nil {
		t.Fatalf("fabricated lookup table is invalid: %v", err)
	}
	return table
}

func TestAutoOversizedSwapEdgeIsMeasuredAndUsesTheV0EscapeHatch(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegate := mustKey(bridgeDelegate)
	// Measure where the legacy packet limit is crossed across the validator's
	// full account range, then exercise the exact oversized edge.
	crossed, crossedBytes, maxAccountsBytes := 0, 0, 0
	for filler := 0; filler <= 64-10; filler++ {
		request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, filler)
		inner, err := validateJupiterInstructionForRoute(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
		if err != nil {
			t.Fatal(err)
		}
		outer, err := wrapSquadsJupiterPolicy(mustKey(request.Policy), delegate, delegate, request.PolicyConstraintIndex, inner)
		if err != nil {
			t.Fatal(err)
		}
		message, err := compileLegacyMessage(delegate, mustKey(bridgeSettings), []compiledInstruction{outer})
		if err != nil {
			t.Fatal(err)
		}
		if filler == 22 { // Jupiter is quoted with maxAccounts=32
			maxAccountsBytes = len(message) + 65
		}
		if crossed == 0 && len(message)+65 > solanaPacketBytes {
			crossed, crossedBytes = 10+filler, len(message)+65
		}
	}
	if crossed == 0 {
		t.Fatal("no account count crossed the legacy packet limit; measurement is stale")
	}
	t.Logf("AUTO USDC->AUTO legacy packet: realistic Jupiter maxAccounts=32 quote = %d bytes (fits: %v); limit %d crossed at %d inner accounts (%d bytes)",
		maxAccountsBytes, maxAccountsBytes <= solanaPacketBytes, solanaPacketBytes, crossed, crossedBytes)
	request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-10)
	if _, err := manifest.compileJupiterMessage(request, delegate); err == nil || !strings.Contains(err.Error(), "unsigned message does not fit") {
		t.Fatalf("oversized AUTO edge compiled without hints: %v", err)
	}
	fillers := make([]string, 0, len(request.Instruction.Accounts))
	for index, account := range request.Instruction.Accounts {
		if index < 10 {
			continue // reviewed boundaries and authorities stay static
		}
		fillers = append(fillers, account.Pubkey)
	}
	table := autoFixtureLookupTable(t, fillers)
	request.Instruction.LookupTableAddresses = []string{table.Address}
	request.LookupTables = []LookupTableSnapshot{table}
	message, err := manifest.compileJupiterMessage(request, delegate)
	if err != nil || message[0] != 0x80 || message[1] != 1 {
		t.Fatalf("validated AUTO hints did not produce a v0 outer message: %v", err)
	}
	if len(message)+65 > solanaPacketBytes {
		t.Fatalf("v0 AUTO packet %d still exceeds the limit", len(message)+65)
	}
	t.Logf("AUTO USDC->AUTO v0 packet with one quoted lookup table = %d bytes (+65 signature = %d of %d)", len(message), len(message)+65, solanaPacketBytes)
	staticKeys, _, outerData := decodeV0OuterInstruction(t, message)
	if !bytes.Equal(outerData[:8], squadsExecuteSyncDiscriminator) {
		t.Fatal("v0 outer instruction left the Squads execute")
	}
	for _, authority := range []string{request.Policy, bridgeSquadsProgram, bridgeDelegate} {
		found := false
		for _, key := range staticKeys {
			found = found || key == authority
		}
		if !found {
			t.Fatalf("v0 message offloaded authority account %s", authority)
		}
	}
	// Without quoted hints the same edge must stay fail-closed, never guess.
	hintless := request
	hintless.LookupTables = nil
	hintless.Instruction.LookupTableAddresses = nil
	if _, err := manifest.prepareJupiterLookupTables(context.Background(), nil, hintless, 1); err == nil ||
		!strings.Contains(err.Error(), "unsupported construction") {
		t.Fatalf("hintless oversized AUTO edge prepared: %v", err)
	}
}

func TestAutoQuoteEvidenceThroughCandidateManifest(t *testing.T) {
	manifest := autoFixtureManifest(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	instruction := autoJupiterTestInstruction(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			response = `{"inputMint":"` + bridgeUSDC + `","outputMint":"` + route.Kamino.CollateralMint +
				`","inAmount":"1000000","outAmount":"990000","otherAmountThreshold":"985050",` +
				`"swapMode":"ExactIn","slippageBps":50,"platformFee":null,"routePlan":[{}]}`
		case "/swap-instructions":
			body, _ := json.Marshal(map[string]any{"setupInstructions": []any{}, "otherInstructions": []any{},
				"cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction,
				"addressLookupTableAddresses": []any{}})
			response = string(body)
		default:
			t.Errorf("unexpected Jupiter endpoint %s", r.URL.Path)
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: newJSONBody(response), Header: make(http.Header)}, nil
	})
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Method != "getLatestBlockhash" {
			t.Fatalf("fitting AUTO quote path read %s; no chain lookup read was expected", body.Method)
		}
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"blockhash":"` + bridgeSettings + `","lastValidBlockHeight":99}}}`), nil
	})
	decision := Decision{Action: SwapStableToCollateralStep, Reason: "candidate_quote", IdempotencyKey: "candidate-quote", AmountRaw: 1_000_000, StrategyKey: autoAUTOPYUSD.Lane}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, decision, 2_000_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	request := evidence.Request
	if request.RouteLane != autoAUTOPYUSD.Lane || request.Policy != autoFixturePolicy ||
		request.PolicyAccountDataSHA256 != autoFixtureHash || request.PolicyConstraintIndex != 4 {
		t.Fatalf("quote evidence did not retain the reviewed binding: %+v", request)
	}
	if request.MinimumOutputRaw != 985_050 || request.QuotedOutputRaw != 990_000 {
		t.Fatalf("quote economics drifted: %+v", request)
	}
	source := evidence.ExpectedEffects.Accounts[0]
	destination := evidence.ExpectedEffects.Accounts[1]
	if source.Address != bridgeSquadsATA || source.AfterRaw != 1_000_000 || source.Owner != classicTokenProgram {
		t.Fatalf("source effect drifted: %+v", source)
	}
	if destination.Address != route.CollateralCustody || destination.AfterRaw != 985_050 {
		t.Fatalf("destination effect drifted: %+v", destination)
	}
	delegate := mustKey(bridgeDelegate)
	if _, err := manifest.compileJupiterMessage(request, delegate); err != nil {
		t.Fatalf("quote-path request did not compile under the reviewed binding: %v", err)
	}
	// The same decision through the explicit absent-binding fixture must hold
	// before any quote: authorization is never request-supplied.
	if _, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, autoAbsentBindingManifest(t), decision, 2_000_000, 0, 42); err == nil {
		t.Fatal("absent binding resolved quote evidence for AUTO")
	} else {
		assertBudgetHold(t, err, "auto_policy_not_activated")
	}
	// The embedded manifest carries the installed binding, so the same decision
	// resolves quote evidence carrying exactly the installed identity — the
	// installed state this release ships.
	embedded := requireEmbeddedInstalledBinding(t)
	installedEvidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, embedded, decision, 2_000_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	installedRequest := installedEvidence.Request
	if installedRequest.RouteLane != autoAUTOPYUSD.Lane || installedRequest.Policy != installedAutoPolicyKey ||
		installedRequest.PolicyAccountDataSHA256 != installedAutoPolicyDigest || installedRequest.PolicyConstraintIndex != 4 {
		t.Fatalf("installed quote evidence lost the installed binding: %+v", installedRequest)
	}
	// A foreign action on this lane is rejected before any quote is requested.
	foreign := decision
	foreign.Action = SwapUSDCToDebtStep
	if _, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, foreign, 2_000_000, 0, 42); err == nil ||
		!strings.Contains(err.Error(), "not an approved AUTO") {
		t.Fatalf("foreign AUTO pair quoted: %v", err)
	}
}

func TestAutoQuoteEvidencePreparesChainLookupTablesForOversizedEdges(t *testing.T) {
	manifest := autoFixtureManifest(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	instruction := autoJupiterTestInstruction(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-10)
	table := autoFixtureLookupTable(t, nil)
	for index := 10; index < len(instruction.Accounts); index++ {
		table.Data = append(table.Data, mustDecodeKey(t, instruction.Accounts[index].Pubkey)...)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			response = `{"inputMint":"` + bridgeUSDC + `","outputMint":"` + route.Kamino.CollateralMint +
				`","inAmount":"1000000","outAmount":"990000","otherAmountThreshold":"985050",` +
				`"swapMode":"ExactIn","slippageBps":50,"platformFee":null,"routePlan":[{}]}`
		case "/swap-instructions":
			body, _ := json.Marshal(map[string]any{"setupInstructions": []any{}, "otherInstructions": []any{},
				"cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction,
				"addressLookupTableAddresses": []string{table.Address}})
			response = string(body)
		default:
			t.Errorf("unexpected Jupiter endpoint %s", r.URL.Path)
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: newJSONBody(response), Header: make(http.Header)}, nil
	})
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch body.Method {
		case "getLatestBlockhash":
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"blockhash":"` + bridgeSettings + `","lastValidBlockHeight":99}}}`), nil
		case "getMultipleAccounts":
			reads++
			var addresses []string
			if err := json.Unmarshal(body.Params[0], &addresses); err != nil || len(addresses) != 1 || addresses[0] != table.Address {
				t.Fatalf("lookup read left the quoted identity: %v %v", addresses, err)
			}
			value := []map[string]any{{"owner": table.Owner, "lamports": table.Lamports, "executable": false,
				"data": []string{base64.StdEncoding.EncodeToString(table.Data), "base64"}}}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":` + marshalTestJSON(t, value) + `}}`), nil
		default:
			t.Fatalf("unexpected RPC read %s", body.Method)
			return nil, nil
		}
	})
	decision := Decision{Action: SwapStableToCollateralStep, Reason: "candidate_quote", IdempotencyKey: "candidate-quote-oversized", AmountRaw: 1_000_000, StrategyKey: autoAUTOPYUSD.Lane}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, decision, 2_000_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(evidence.Request.LookupTables) != 1 {
		t.Fatalf("oversized AUTO quote did not resolve the quoted table: reads=%d tables=%v", reads, evidence.Request.LookupTables)
	}
	message, err := manifest.compileJupiterMessage(evidence.Request, mustKey(bridgeDelegate))
	if err != nil || message[0] != 0x80 || len(message)+65 > solanaPacketBytes {
		t.Fatalf("quote-path v0 packet did not compile: %v", err)
	}
}

func mustDecodeKey(t *testing.T, value string) []byte {
	t.Helper()
	key, err := decodeKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key[:]
}

// testAutoJupiterCatalogLaneFailClosed replaces the historical catalog
// construction proof for the AUTO lane, named for both binding states: the
// retained shard policies stay observation pins, the explicit absent-binding
// fixture (the shipped pre-install state) keeps policy resolution closed,
// approved edges keep v0 lookup-hint eligibility, the excluded USDC->PYUSD
// pair never resolves even under a valid binding, and the embedded manifest's
// installed binding resolves approved edges through exactly the installed
// identity.
func testAutoJupiterCatalogLaneFailClosed(t *testing.T, action Action, key string) {
	t.Helper()
	_, edgeErr := autoSwapConstraintKey(action)
	if _, err := autoAbsentBindingManifest(t).jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane); err != nil {
		if edgeErr != nil {
			// The excluded pair fails on edge rejection before any binding
			// lookup; either way the catalog entry is never reached.
			if !strings.Contains(err.Error(), "not an approved AUTO") {
				t.Fatalf("excluded pair %s failed for the wrong reason: %v", key, err)
			}
		} else {
			assertBudgetHold(t, err, "auto_policy_not_activated")
		}
	} else if edgeErr == nil {
		t.Fatal("absent binding resolved an AUTO swap policy")
	}
	if edgeErr != nil {
		if acceptsJupiterLookupHints(autoAUTOPYUSD.Lane, action) {
			t.Fatalf("non-edge AUTO action %s gained lookup-hint eligibility", action)
		}
		candidate := autoFixtureManifest(t)
		if _, err := candidate.jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane); err == nil ||
			!strings.Contains(err.Error(), "not an approved AUTO") {
			t.Fatalf("excluded catalog pair %s resolved under the candidate binding: %v", key, err)
		}
		_, installedErr := requireEmbeddedInstalledBinding(t).jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane)
		if installedErr == nil || !strings.Contains(installedErr.Error(), "not an approved AUTO") {
			t.Fatalf("excluded catalog pair %s resolved under the installed binding: %v", key, installedErr)
		}
		return
	}
	if !acceptsJupiterLookupHints(autoAUTOPYUSD.Lane, action) {
		t.Fatalf("approved AUTO edge %s lost lookup-hint eligibility", key)
	}
	binding, err := autoFixtureManifest(t).jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane)
	if err != nil || !binding.AutoPolicy || binding.CatalogLane != "" {
		t.Fatalf("approved AUTO edge %s lost its candidate binding: %v", key, err)
	}
	installedBinding, err := requireEmbeddedInstalledBinding(t).jupiterPolicyForRoute(action, autoAUTOPYUSD.Lane)
	if err != nil || !installedBinding.AutoPolicy || installedBinding.CatalogLane != "" ||
		installedBinding.Policy != installedAutoPolicyKey || installedBinding.PolicyAccountDataSHA256 != installedAutoPolicyDigest {
		t.Fatalf("approved AUTO edge %s did not resolve the installed binding: %+v %v", key, installedBinding, err)
	}
}

// testAutoKaminoCatalogLegFailClosed replaces the historical catalog
// construction proof for AUTO lifecycle legs, named for both binding states:
// the retained shard policies stay observation pins, the explicit
// absent-binding fixture (the shipped pre-install state) keeps packet
// resolution closed, the candidate binding carries the exact retained account
// vector, and the embedded manifest's installed binding resolves the same
// account vector through the installed identity.
func testAutoKaminoCatalogLegFailClosed(t *testing.T, action Action, leg kaminoPrimeUSDCLeg, accounts KaminoPrimeUSDCAccounts) {
	t.Helper()
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	_, err := autoAbsentBindingManifest(t).kaminoPacketForRoute(action, leg, 77, blockhash, autoAUTOPYUSD.Lane)
	if err != nil {
		assertBudgetHold(t, err, "auto_policy_not_activated")
	} else {
		t.Fatal("absent binding resolved an AUTO lifecycle packet")
	}
	candidate := autoFixtureManifest(t)
	request, err := candidate.kaminoPacketForRoute(action, leg, 77, blockhash, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	binding := autoFixtureBinding(t)
	if request.Policy != binding.Policy || request.PolicyAccountDataSHA256 != binding.AccountDataSHA256 ||
		request.PolicyConstraintIndex != autoKaminoConstraintIndex(leg) {
		t.Fatal("candidate AUTO packet lost the reviewed binding identity")
	}
	if !reflect.DeepEqual(request.Accounts, accounts) {
		t.Fatal("candidate AUTO packet diverges from the retained account vector")
	}
	compiled, err := candidate.compileKaminoMessage(request, mustKey(bridgeDelegate))
	if err != nil || len(compiled)+65 > solanaPacketBytes {
		t.Fatalf("candidate AUTO lifecycle compile failed: %v", err)
	}
	// The production wrapper reloads the embedded manifest: before the release
	// it held with auto_policy_not_activated; with the installed binding
	// shipped, the candidate identity is refused as a binding mismatch.
	_, err = CompileKaminoMessage(request)
	if err == nil || !strings.Contains(err.Error(), "does not match the reviewed AUTO binding") {
		t.Fatalf("production wrapper did not refuse the candidate identity: %v", err)
	}
	// The installed state resolves the same retained account vector through
	// exactly the installed identity.
	installedRequest, err := requireEmbeddedInstalledBinding(t).kaminoPacketForRoute(action, leg, 77, blockhash, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	if installedRequest.Policy != installedAutoPolicyKey || installedRequest.PolicyAccountDataSHA256 != installedAutoPolicyDigest ||
		installedRequest.PolicyConstraintIndex != autoKaminoConstraintIndex(leg) {
		t.Fatal("installed AUTO packet lost the installed binding identity")
	}
	if !reflect.DeepEqual(installedRequest.Accounts, accounts) {
		t.Fatal("installed AUTO packet diverges from the retained account vector")
	}
	installedCompiled, err := CompileKaminoMessage(installedRequest)
	if err != nil {
		t.Fatalf("production wrapper refused the installed binding: %v", err)
	}
	if embeddedCompiled, err := requireEmbeddedInstalledBinding(t).compileKaminoMessage(installedRequest, mustKey(bridgeDelegate)); err != nil || !bytes.Equal(installedCompiled, embeddedCompiled) {
		t.Fatalf("installed wrapper drifted from the manifest compile: %v", err)
	}
}

func newJSONBody(value string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(value))
}

func marshalTestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
