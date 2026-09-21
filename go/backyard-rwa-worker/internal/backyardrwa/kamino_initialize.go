package backyardrwa

import (
	"encoding/binary"
	"fmt"
)

// All identities are fixed by the lane and a reviewed, installed policy. Rent
// and network fees are native SOL flows, separate from user USDC principal.
type KaminoInitializationRequest struct {
	RouteLane               string `json:"routeLane"`
	PolicySeed              uint64 `json:"policySeed"`
	PolicyAccountDataSHA256 string `json:"policyAccountDataSha256"`
	RecentBlockhash         string `json:"recentBlockhash"`
	LastValidBlockHeight    int64  `json:"lastValidBlockHeight"`
	RentLamports            uint64 `json:"rentLamports"`
	MaximumFeeLamports      uint64 `json:"maximumFeeLamports"`
}

func CompileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	if r.LastValidBlockHeight <= 0 || r.RentLamports == 0 || r.MaximumFeeLamports == 0 || !validSHA256(r.PolicyAccountDataSHA256) {
		return nil, fmt.Errorf("incomplete Multiply initialization admission")
	}
	policy, err := policySetupAddress(r.PolicySeed)
	if err != nil {
		return nil, err
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		return nil, err
	}
	outer, err := wrapSquadsKaminoPolicy(policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), 0, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileLegacyMessage(mustKey(bridgeDelegate), blockhash, []compiledInstruction{outer})
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// compileKaminoInitializationMessage is the manifest-aware form. Installed
// selector lanes keep the exact public path above; the candidate AUTO lane is
// admitted only against the reviewed binding, whose policy identity, account
// digest and appended initializer index are retained from the manifest — never
// from the request. Any other lane holds.
func (m RouteManifest) compileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	if selectorLane(r.RouteLane) {
		return CompileKaminoInitializationMessage(r)
	}
	if r.RouteLane != autoAUTOPYUSD.Lane {
		return nil, budgetHold("initializer_lane_unreviewed")
	}
	binding, index, err := m.autoInitializerBinding()
	if err != nil {
		return nil, err
	}
	if r.PolicySeed != binding.PolicySeed || r.PolicyAccountDataSHA256 != binding.AccountDataSHA256 {
		return nil, budgetHold("initializer_request_manifest_mismatch")
	}
	if r.LastValidBlockHeight <= 0 || r.RentLamports == 0 || r.MaximumFeeLamports == 0 {
		return nil, fmt.Errorf("incomplete Multiply initialization admission")
	}
	policy, err := policySetupAddress(binding.PolicySeed)
	if err != nil {
		return nil, err
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoRouteInitializer(route)
	if err != nil {
		return nil, err
	}
	outer, err := wrapSquadsKaminoPolicy(policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
	if err != nil {
		return nil, err
	}
	// The eight-constraint installed policy needs the reviewed 64 KiB heap frame
	// during execute as well as during create, so the AUTO initializer leg goes
	// through the closed resource wrapper; the public compiler above keeps the
	// exact installed selector-lane bytes.
	message, err := compileAutoResourceLegacyMessage(mustKey(bridgeDelegate), blockhash, outer)
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil || !selectorLane(route.Lane) {
		return fmt.Errorf("unreviewed initialized obligation")
	}
	return validateInitializedObligationOnRoute(route, r, a)
}

// validateInitializedKaminoObligationOnRoute is the manifest-aware form: the
// AUTO candidate is admitted only after the request identity matches the
// reviewed binding — the same seed and account digest check compile and
// prestate apply — and the empty-state checks below are the exact installed
// checks.
func (m RouteManifest) validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	if selectorLane(r.RouteLane) {
		return validateInitializedKaminoObligation(r, a)
	}
	if r.RouteLane != autoAUTOPYUSD.Lane {
		return budgetHold("initializer_lane_unreviewed")
	}
	binding, _, err := m.autoInitializerBinding()
	if err != nil {
		return err
	}
	if binding.PolicySeed != r.PolicySeed || binding.AccountDataSHA256 != r.PolicyAccountDataSHA256 {
		return budgetHold("initializer_request_manifest_mismatch")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return err
	}
	return validateInitializedObligationOnRoute(route, r, a)
}

func validateInitializedObligationOnRoute(route RuntimeRoute, r KaminoInitializationRequest, a ConfirmedAccount) error {
	o, err := decodeKaminoObligation(a, route.Kamino)
	if err != nil {
		return err
	}
	if a.Lamports != r.RentLamports || binary.LittleEndian.Uint64(a.Data[8:16]) != 1 || o.hasPosition ||
		!allZero(a.Data[96:1184]) || !allZero(a.Data[1208:2208]) ||
		a.Data[kaminoObligationElevationGroupOffset] != 0 || !allZero(a.Data[2288:2320]) {
		return fmt.Errorf("initialized obligation state differs from admitted empty Multiply account")
	}
	return nil
}

// Construct only the three reviewed Multiply PDAs. Policy installation, rent
// admission and journal execution are separate; this function grants no authority.
func kaminoMultiplyInitializer(lane string) (compiledInstruction, error) {
	route, err := runtimeRoute(lane)
	if err != nil || !selectorLane(route.Lane) || route.Kamino.DebtMint != bridgeUSDC {
		return compiledInstruction{}, fmt.Errorf("unreviewed Multiply initializer lane")
	}
	return kaminoRouteInitializer(route)
}

// kaminoRouteInitializer is the lane-independent checked core: the identical
// nine-account topology with the route's own debt mint, and the obligation PDA
// re-derived from the route identities and compared to the reviewed route
// obligation, so a candidate lane can never drift to a synthetic authority.
func kaminoRouteInitializer(route RuntimeRoute) (compiledInstruction, error) {
	program, owner := mustKey(kaminoProgram), mustKey(bridgeVault)
	market, collateral, debt := mustKey(route.Kamino.Market), mustKey(route.Kamino.CollateralMint), mustKey(route.Kamino.DebtMint)
	obligation, err := findProgramDerivedAddress([]byte{1}, program[:], []byte{0}, owner[:], market[:], collateral[:], debt[:])
	if err != nil || obligation != route.Kamino.Obligation {
		return compiledInstruction{}, fmt.Errorf("Multiply obligation PDA drifted")
	}
	metadata, err := findProgramDerivedAddress([]byte("user_meta"), program[:], owner[:])
	if err != nil {
		return compiledInstruction{}, err
	}
	return compiledInstruction{program: program,
		accounts: []accountMeta{
			meta(bridgeVault, true, false), meta(bridgeVault, true, true),
			meta(obligation, false, true), meta(route.Kamino.Market, false, false),
			meta(route.Kamino.CollateralMint, false, false), meta(route.Kamino.DebtMint, false, false),
			meta(metadata, false, false), meta("SysvarRent111111111111111111111111111111111", false, false),
			meta("11111111111111111111111111111111", false, false),
		}, data: []byte{251, 10, 231, 76, 27, 11, 159, 96, 1, 0}}, nil
}
