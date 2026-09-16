package backyardrwa

import "fmt"

// These pins are deployment evidence, never supplied by the partner API or the
// opportunity feed. Absence leaves initialization unavailable on older images.
type KaminoInitializerBinding struct {
	Lane              string `json:"lane"`
	PolicySeed        uint64 `json:"policySeed"`
	Policy            string `json:"policy"`
	AccountDataSHA256 string `json:"accountDataSha256"`
}

func (m RouteManifest) validateInitializerBindings() error {
	bindings := m.RuntimeBindings.MultiplyInitializers
	if len(bindings) == 0 {
		return nil
	}
	if len(bindings) != 3 {
		return fmt.Errorf("initializer manifest must bind all three pilot lanes")
	}
	lanes, seeds := map[string]bool{}, map[uint64]bool{}
	for _, b := range bindings {
		policy, err := policySetupAddress(b.PolicySeed)
		if err != nil || b.PolicySeed == 0 || !selectorLane(b.Lane) || lanes[b.Lane] || seeds[b.PolicySeed] || encodeBase58(policy[:]) != b.Policy || !validSHA256(b.AccountDataSHA256) {
			return fmt.Errorf("initializer manifest policy identity differs")
		}
		lanes[b.Lane], seeds[b.PolicySeed] = true, true
	}
	return nil
}

func (m RouteManifest) initializerBinding(lane string) (KaminoInitializerBinding, error) {
	if err := m.validateInitializerBindings(); err != nil {
		return KaminoInitializerBinding{}, err
	}
	for _, b := range m.RuntimeBindings.MultiplyInitializers {
		if b.Lane == lane {
			return b, nil
		}
	}
	return KaminoInitializerBinding{}, budgetHold("initializer_policy_not_activated")
}

func (m RouteManifest) initializationRequest(lane string, blockhash LatestBlockhash, rent, maximumFee uint64) (KaminoInitializationRequest, error) {
	b, err := m.initializerBinding(lane)
	if err != nil {
		return KaminoInitializationRequest{}, err
	}
	r := KaminoInitializationRequest{RouteLane: lane, PolicySeed: b.PolicySeed, PolicyAccountDataSHA256: b.AccountDataSHA256, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RentLamports: rent, MaximumFeeLamports: maximumFee}
	if _, err = CompileKaminoInitializationMessage(r); err != nil {
		return KaminoInitializationRequest{}, err
	}
	return r, nil
}

func (m RouteManifest) validateInitializationRequest(r KaminoInitializationRequest) error {
	b, err := m.initializerBinding(r.RouteLane)
	if err != nil {
		return err
	}
	if b.PolicySeed != r.PolicySeed || b.AccountDataSHA256 != r.PolicyAccountDataSHA256 {
		return budgetHold("initializer_request_manifest_mismatch")
	}
	_, err = CompileKaminoInitializationMessage(r)
	return err
}
