package backyard

import "fmt"

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
