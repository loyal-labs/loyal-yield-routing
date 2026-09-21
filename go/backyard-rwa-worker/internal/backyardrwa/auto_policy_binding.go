package backyardrwa

import "fmt"

// The combined AUTO candidate policy is ONE Squads ProgramInteraction account
// carrying the seven proven constraints (deposit, withdraw, borrow, repay and
// three sharedAccountsRoute bicliques). It resolves only from an explicit
// reviewed manifest value: no catalog entry, no package-global mutable state,
// no manifest-load side effects, no automatic fallback.
//
// The candidate seed/address/hash proven by the deployed-Squads semantics test
// (seed 900) are local fixture values; production values arrive only when the
// coordinator adds the autoPolicy manifest section.

type AutoPolicyBinding struct {
	Lane              string          `json:"lane"`
	PolicySeed        uint64          `json:"policySeed"`
	Policy            string          `json:"policy"`
	AccountDataSHA256 string          `json:"accountDataSha256"`
	ConstraintIndices map[string]byte `json:"constraintIndices"`
}

// autoConstraintIndices is the proven constraint order of the combined policy
// (auto_constraint_index in loyal-actions): the four lifecycle legs plus the
// minimal exact biclique cover of the five AUTO custody pairs.
var autoConstraintIndices = map[string]byte{
	"deposit":               0,
	"withdraw":              1,
	"borrow":                2,
	"repay":                 3,
	"swapUSDCOrPYUSDToAUTO": 4,
	"swapAUTOToUSDCOrPYUSD": 5,
	"swapPYUSDToUSDC":       6,
}

// The appended eighth constraint of the SAME candidate policy: the exact AUTO
// obligation initializer (onboarding only; no second policy account). A
// binding MAY carry it at exactly this index; every other extra key is
// unproven.
const (
	autoInitializerConstraintKey   = "initialize"
	autoInitializerConstraintIndex = byte(7)
)

// autoPolicyBinding validates and returns the optional binding. Absence holds
// every AUTO path while Maple/Prime/OnRe stay exactly as shipped. A malformed
// present binding fails AUTO paths only: validation runs at access, never at
// manifest load, so it cannot disable the shipped lanes.
func (m RouteManifest) autoPolicyBinding() (AutoPolicyBinding, error) {
	if m.RuntimeBindings.AutoPolicy == nil {
		return AutoPolicyBinding{}, budgetHold("auto_policy_not_activated")
	}
	if err := validateAutoPolicyBinding(*m.RuntimeBindings.AutoPolicy); err != nil {
		return AutoPolicyBinding{}, err
	}
	return *m.RuntimeBindings.AutoPolicy, nil
}

func validateAutoPolicyBinding(binding AutoPolicyBinding) error {
	if binding.Lane != autoAUTOPYUSD.Lane {
		return fmt.Errorf("AUTO policy binding lane %q is not %q", binding.Lane, autoAUTOPYUSD.Lane)
	}
	if binding.PolicySeed == 0 {
		return fmt.Errorf("AUTO policy binding seed is unset")
	}
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		if binding.PolicySeed == basicPolicySeeds[family] {
			return fmt.Errorf("AUTO policy binding seed aliases installed %s", family)
		}
	}
	derived, err := derivePolicyAccount(binding.PolicySeed)
	if err != nil {
		return fmt.Errorf("derive AUTO policy account: %w", err)
	}
	if derived != binding.Policy {
		return fmt.Errorf("AUTO policy binding seed and address disagree")
	}
	if !validSHA256(binding.AccountDataSHA256) {
		return fmt.Errorf("AUTO policy binding data hash is malformed")
	}
	// Append-only: the seven proven indexes stay mandatory, and the optional
	// eighth entry must be the exact initializer key at the exact appended
	// index. Any other extra key is unproven and rejected.
	if len(binding.ConstraintIndices) < len(autoConstraintIndices) || len(binding.ConstraintIndices) > len(autoConstraintIndices)+1 {
		return fmt.Errorf("AUTO policy binding must carry exactly the seven proven constraint indexes")
	}
	for key, index := range autoConstraintIndices {
		value, ok := binding.ConstraintIndices[key]
		if !ok {
			return fmt.Errorf("AUTO policy binding is missing proven constraint %q", key)
		}
		if value != index {
			return fmt.Errorf("AUTO policy binding constraint %q drifted from the proven index", key)
		}
	}
	initializer, hasInitializer := binding.ConstraintIndices[autoInitializerConstraintKey]
	for key := range binding.ConstraintIndices {
		if key == autoInitializerConstraintKey {
			continue
		}
		if _, proven := autoConstraintIndices[key]; !proven {
			return fmt.Errorf("AUTO policy binding carries unproven constraint %q", key)
		}
	}
	if hasInitializer && initializer != autoInitializerConstraintIndex {
		return fmt.Errorf("AUTO policy binding initializer drifted from the appended index %d", autoInitializerConstraintIndex)
	}
	return nil
}

// autoJupiterBinding resolves the reviewed binding for one approved AUTO swap
// edge. Foreign pairs (the excluded reverse edge, USDS/ONyc/AUTO-only edges)
// never resolve.
func (m RouteManifest) autoJupiterBinding(action Action) (JupiterPolicyBinding, error) {
	key, err := autoSwapConstraintKey(action)
	if err != nil {
		return JupiterPolicyBinding{}, err
	}
	binding, err := m.autoPolicyBinding()
	if err != nil {
		return JupiterPolicyBinding{}, err
	}
	return JupiterPolicyBinding{AutoPolicy: true, Action: action, Policy: binding.Policy,
		PolicyAccountDataSHA256: binding.AccountDataSHA256,
		PolicyConstraintIndex:   binding.ConstraintIndices[key], AmountOffset: 18}, nil
}

// autoKaminoBinding resolves the reviewed binding for one AUTO lifecycle leg.
func (m RouteManifest) autoKaminoBinding(leg kaminoPrimeUSDCLeg) (AutoPolicyBinding, byte, error) {
	key, err := autoKaminoLegConstraintKey(leg)
	if err != nil {
		return AutoPolicyBinding{}, 0, err
	}
	binding, err := m.autoPolicyBinding()
	if err != nil {
		return AutoPolicyBinding{}, 0, err
	}
	return binding, binding.ConstraintIndices[key], nil
}

func autoSwapConstraintKey(action Action) (string, error) {
	switch action {
	case SwapStableToCollateralStep, SwapDebtToCollateralStep:
		return "swapUSDCOrPYUSDToAUTO", nil
	case SwapCollateralToStableStep, SwapCollateralToDebtStep:
		return "swapAUTOToUSDCOrPYUSD", nil
	case SwapDebtToUSDCStep:
		return "swapPYUSDToUSDC", nil
	default:
		return "", fmt.Errorf("action %s is not an approved AUTO swap edge", action)
	}
}

func autoKaminoLegConstraintKey(leg kaminoPrimeUSDCLeg) (string, error) {
	switch leg {
	case kaminoLegDeposit:
		return "deposit", nil
	case kaminoLegWithdraw:
		return "withdraw", nil
	case kaminoLegBorrow:
		return "borrow", nil
	case kaminoLegRepay:
		return "repay", nil
	default:
		return "", fmt.Errorf("unknown AUTO lifecycle leg")
	}
}

// autoKaminoConstraintIndex is the proven per-leg index of the combined
// policy, used by the route-aware low-level validation.
func autoKaminoConstraintIndex(leg kaminoPrimeUSDCLeg) byte {
	key, err := autoKaminoLegConstraintKey(leg)
	if err != nil {
		return 0xff
	}
	return autoConstraintIndices[key]
}

// autoSwapEdge resolves the exact custody pair from the route's own reviewed
// identities (protocol identity, not activation). The five pairs are the
// minimal exact cover of the AUTO recipe; every other pair is rejected.
func autoSwapEdge(route RuntimeRoute, action Action) (sourceMint, destinationMint, sourceATA, destinationATA string, err error) {
	collateral, debt := route.Kamino.CollateralMint, route.Kamino.DebtMint
	switch action {
	case SwapStableToCollateralStep:
		return bridgeUSDC, collateral, bridgeSquadsATA, route.CollateralCustody, nil
	case SwapDebtToCollateralStep:
		return debt, collateral, route.DebtCustody, route.CollateralCustody, nil
	case SwapCollateralToStableStep:
		return collateral, bridgeUSDC, route.CollateralCustody, bridgeSquadsATA, nil
	case SwapCollateralToDebtStep:
		return collateral, debt, route.CollateralCustody, route.DebtCustody, nil
	case SwapDebtToUSDCStep:
		return debt, bridgeUSDC, route.DebtCustody, bridgeSquadsATA, nil
	default:
		return "", "", "", "", fmt.Errorf("action %s is not an approved AUTO Jupiter edge", action)
	}
}

func autoTokenProgram(route RuntimeRoute, mint string) (string, error) {
	switch mint {
	case bridgeUSDC:
		return classicTokenProgram, nil
	case route.Kamino.CollateralMint:
		return route.CollateralTokenProgram, nil
	case route.Kamino.DebtMint:
		return route.DebtTokenProgram, nil
	default:
		return "", fmt.Errorf("mint %s is not an AUTO route asset", mint)
	}
}

// autoTokenPrograms replaces the catalog metadata read for the candidate AUTO
// lane in the observation paths.
func autoTokenPrograms(route RuntimeRoute, action Action) (source, destination string, err error) {
	sourceMint, destinationMint, _, _, err := autoSwapEdge(route, action)
	if err != nil {
		return "", "", err
	}
	if source, err = autoTokenProgram(route, sourceMint); err != nil {
		return "", "", err
	}
	if destination, err = autoTokenProgram(route, destinationMint); err != nil {
		return "", "", err
	}
	return source, destination, nil
}

// requireAutoKaminoBinding retains the reviewed binding into low-level
// compilation: the leg is resolved structurally from the exact request, then
// the request identity is compared against the manifest's reviewed binding. A
// caller cannot authorize itself by supplying matching-looking values.
func (m RouteManifest) requireAutoKaminoBinding(request KaminoPrimeUSDCRequest, route RuntimeRoute) error {
	_, leg, err := kaminoResolvedRouteInstruction(request, route)
	if err != nil {
		return err
	}
	binding, index, err := m.autoKaminoBinding(leg)
	if err != nil {
		return err
	}
	if request.Policy != binding.Policy || request.PolicyAccountDataSHA256 != binding.AccountDataSHA256 || request.PolicyConstraintIndex != index {
		return fmt.Errorf("Kamino policy does not match the reviewed AUTO binding")
	}
	return nil
}

// requireAutoJupiterBinding is the swap-side retention twin: it runs after the
// inner instruction passed structural validation, and compares the request
// identity against the reviewed manifest binding only.
func (m RouteManifest) requireAutoJupiterBinding(request JupiterSwapRequest) error {
	binding, err := m.autoJupiterBinding(request.Action)
	if err != nil {
		return err
	}
	if request.Policy != binding.Policy || request.PolicyAccountDataSHA256 != binding.PolicyAccountDataSHA256 || request.PolicyConstraintIndex != binding.PolicyConstraintIndex {
		return fmt.Errorf("Jupiter policy does not match the reviewed AUTO binding")
	}
	return nil
}
