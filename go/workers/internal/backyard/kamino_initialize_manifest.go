package backyard

import "fmt"

// initializationRequest is lane's initializer request through its installed
// initializer policy.
func (m RouteManifest) initializationRequest(policies installedPolicies, lane string, blockhash LatestBlockhash, rent, maximumFee uint64) (KaminoInitializationRequest, error) {
	key, _ := initializerPolicyLeg(lane)
	policy, err := policies.account(key)
	if err != nil {
		return KaminoInitializationRequest{}, err
	}
	r := KaminoInitializationRequest{RouteLane: lane, Policy: policy, RecentBlockhash: blockhash.Blockhash,
		LastValidBlockHeight: blockhash.LastValidBlockHeight, RentLamports: rent, MaximumFeeLamports: maximumFee}
	if _, err := m.compileKaminoInitializationMessage(r); err != nil {
		return KaminoInitializationRequest{}, err
	}
	return r, nil
}

func (m RouteManifest) validateInitializationRequest(r KaminoInitializationRequest) error {
	_, err := m.compileKaminoInitializationMessage(r)
	return err
}

// validateInitializerDecision is the narrow manifest-aware initializer
// decision validator for the recovery observer. It keeps the exact installed
// shape requirements — InitializeKaminoObligation, multiply_obligation_missing,
// zero amount, nonempty idempotency key, and the journal lane matching the
// request lane — with selector lanes still resolved by
// validateSelectorInitializerDecision. The AUTO lane is admitted only when
// the request itself compiles through validateInitializationRequest; no other
// decision validation is relaxed.
func (m RouteManifest) validateInitializerDecision(d Decision, r KaminoInitializationRequest) error {
	if d.Action != InitializeKaminoObligation || d.StrategyKey != r.RouteLane {
		return fmt.Errorf("initializer journal decision differs")
	}
	if basicLane(d.StrategyKey) {
		return validateSelectorInitializerDecision(d)
	}
	if d.Reason != "multiply_obligation_missing" || d.AmountRaw != 0 || d.IdempotencyKey == "" {
		return fmt.Errorf("invalid Multiply initialization decision")
	}
	return m.validateInitializationRequest(r)
}
