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

// autoInitializerBinding resolves the appended initializer constraint of the
// reviewed candidate AUTO policy. Absence of the eighth entry holds: a
// seven-constraint binding never implies initializer admission.
func (m RouteManifest) autoInitializerBinding() (AutoPolicyBinding, byte, error) {
	binding, err := m.autoPolicyBinding()
	if err != nil {
		return AutoPolicyBinding{}, 0, err
	}
	index, ok := binding.ConstraintIndices[autoInitializerConstraintKey]
	if !ok {
		return AutoPolicyBinding{}, 0, budgetHold("auto_initializer_constraint_not_reviewed")
	}
	return binding, index, nil
}

func (m RouteManifest) initializationRequest(lane string, blockhash LatestBlockhash, rent, maximumFee uint64) (KaminoInitializationRequest, error) {
	var r KaminoInitializationRequest
	if lane == autoAUTOPYUSD.Lane {
		b, _, err := m.autoInitializerBinding()
		if err != nil {
			return KaminoInitializationRequest{}, err
		}
		r = KaminoInitializationRequest{RouteLane: lane, PolicySeed: b.PolicySeed, PolicyAccountDataSHA256: b.AccountDataSHA256, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RentLamports: rent, MaximumFeeLamports: maximumFee}
		if _, err = m.compileKaminoInitializationMessage(r); err != nil {
			return KaminoInitializationRequest{}, err
		}
		return r, nil
	}
	b, err := m.initializerBinding(lane)
	if err != nil {
		return KaminoInitializationRequest{}, err
	}
	r = KaminoInitializationRequest{RouteLane: lane, PolicySeed: b.PolicySeed, PolicyAccountDataSHA256: b.AccountDataSHA256, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RentLamports: rent, MaximumFeeLamports: maximumFee}
	if _, err = CompileKaminoInitializationMessage(r); err != nil {
		return KaminoInitializationRequest{}, err
	}
	return r, nil
}

func (m RouteManifest) validateInitializationRequest(r KaminoInitializationRequest) error {
	if r.RouteLane == autoAUTOPYUSD.Lane {
		b, _, err := m.autoInitializerBinding()
		if err != nil {
			return err
		}
		if b.PolicySeed != r.PolicySeed || b.AccountDataSHA256 != r.PolicyAccountDataSHA256 {
			return budgetHold("initializer_request_manifest_mismatch")
		}
		_, err = m.compileKaminoInitializationMessage(r)
		return err
	}
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

// validateInitializerDecision is the narrow manifest-aware initializer
// decision validator for the recovery observer. It keeps the exact installed
// shape requirements — InitializeKaminoObligation, multiply_obligation_missing,
// zero amount, nonempty idempotency key, and the journal lane matching the
// request lane — with selector lanes still resolved by
// validateSelectorInitializerDecision. The candidate AUTO lane is admitted
// only when the request itself resolves the reviewed binding through
// validateInitializationRequest; no other decision validation is relaxed.
func (m RouteManifest) validateInitializerDecision(d Decision, r KaminoInitializationRequest) error {
	if d.Action != InitializeKaminoObligation || d.StrategyKey != r.RouteLane {
		return fmt.Errorf("initializer journal decision differs")
	}
	if selectorLane(d.StrategyKey) {
		return validateSelectorInitializerDecision(d)
	}
	if d.Reason != "multiply_obligation_missing" || d.AmountRaw != 0 || d.IdempotencyKey == "" {
		return fmt.Errorf("invalid Multiply initialization decision")
	}
	return m.validateInitializationRequest(r)
}
