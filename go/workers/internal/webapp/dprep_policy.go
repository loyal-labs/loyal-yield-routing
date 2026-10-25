package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type dprepGeneration int

const (
	dprepGenerationNone dprepGeneration = iota
	dprepGenerationCompatible
	dprepGenerationLegacy
)

type dprepRentCandidate struct {
	account solana.PublicKey
	known   bool
	kind    string
	label   string
	space   uint64
	stage   string
}

type dprepResolvedPolicy struct {
	account      solana.PublicKey
	seed         uint64
	setupAccount *solana.PublicKey
	setupSeed    uint64
	operation    *DepositWirePreparedOperation
	finalize     *DepositWirePreparedOperation
	persistence  *EarnPolicyPersistence
	rent         []dprepRentCandidate
}

type dprepKnownRoute struct {
	account solana.PublicKey
	seed    uint64
}

type dprepPolicyShapes struct {
	route, legacyRoute, setup squads.Policy
}

func dprepShapesFor(account EarnAccount) (dprepPolicyShapes, error) {
	route, err := EarnRoutePayload(account.Cluster, account.Vault, nil)
	if err != nil {
		return dprepPolicyShapes{}, err
	}
	legacy, err := EarnRoutePayload(account.Cluster, account.Vault, &depositTokenProgramID)
	if err != nil {
		return dprepPolicyShapes{}, err
	}
	setup, err := EarnSetupPayload(account.Cluster, account.Vault)
	if err != nil {
		return dprepPolicyShapes{}, err
	}
	return dprepPolicyShapes{
		route:       squads.Policy{VaultIndex: earnprep.EarnVaultIndex, Constraints: route},
		legacyRoute: squads.Policy{VaultIndex: earnprep.EarnVaultIndex, Constraints: legacy},
		setup:       squads.Policy{VaultIndex: earnprep.EarnVaultIndex, Constraints: setup},
	}, nil
}

func dprepPolicyOf(decoded squads.DecodedPolicy) squads.Policy {
	policy := squads.Policy{VaultIndex: decoded.VaultIndex, Constraints: decoded.Constraints}
	if decoded.Full != nil {
		policy.SpendingLimits = decoded.Full.Payload.SpendingLimits
	}
	return policy
}

func (s dprepPolicyShapes) generation(decoded squads.DecodedPolicy) dprepGeneration {
	policy := dprepPolicyOf(decoded)
	switch {
	case policy.Equal(s.route):
		return dprepGenerationCompatible
	case policy.Equal(s.legacyRoute):
		return dprepGenerationLegacy
	}
	return dprepGenerationNone
}

func dprepAssertTokenProgram(generation dprepGeneration, tokenProgram solana.PublicKey) error {
	if generation == dprepGenerationLegacy && tokenProgram == solana.Token2022ProgramID {
		return earnError(http.StatusConflict, "earn_policy_update_required",
			"This Earn policy must be updated before it can deposit Token-2022 assets.")
	}
	return nil
}

func (h dprepRoutes) assertCanonical(account EarnAccount, address solana.PublicKey, decoded squads.DecodedPolicy, want squads.Policy, label string, seed uint64) error {
	if seed > depositMaxPolicySeed {
		return fmt.Errorf("%s seed is too large for this client.", label)
	}
	expected, _, err := squads.PolicyAddress(account.Settings, seed)
	if err != nil {
		return err
	}
	switch {
	case address != expected:
		return fmt.Errorf("%s account does not match its policy seed.", label)
	case decoded.Settings != account.Settings:
		return fmt.Errorf("%s belongs to another Settings account.", label)
	case decoded.Seed != seed:
		return fmt.Errorf("%s seed does not match its account.", label)
	case decoded.Threshold != 1:
		return fmt.Errorf("%s threshold is not canonical.", label)
	case decoded.TimeLock != 0:
		return fmt.Errorf("%s timelock is not canonical.", label)
	case len(decoded.Signers) != 1:
		return fmt.Errorf("%s signer set is not canonical.", label)
	case decoded.Signers[0] != h.PolicySigner:
		return fmt.Errorf("%s deployment signer does not match.", label)
	case len(decoded.Permissions) != 1 || decoded.Permissions[0] != squads.FullPermissions:
		return fmt.Errorf("%s signer permissions are not canonical.", label)
	case !dprepPolicyOf(decoded).Equal(want):
		return fmt.Errorf("%s instruction constraints are not canonical.", label)
	}
	return nil
}

func (h dprepRoutes) policyAt(ctx context.Context, address solana.PublicKey, label string) (*squads.DecodedPolicy, error) {
	_, read, err := h.Chain.Accounts(ctx, []solana.PublicKey{address}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	if read[0] == nil {
		return nil, nil
	}
	if read[0].Owner != squads.ProgramID {
		return nil, fmt.Errorf("%s %s has an unexpected owner.", label, address)
	}
	decoded, err := squads.DecodeProgramInteractionPolicy(read[0])
	if err != nil {
		return nil, fmt.Errorf("%s %s is not a decodable policy account.", label, address)
	}
	return &decoded, nil
}

func dprepPolicyAddress(settings solana.PublicKey, seed uint64) (solana.PublicKey, error) {
	address, _, err := squads.PolicyAddress(settings, seed)
	return address, err
}

func dprepCurrentSeed(settings squads.Settings) uint64 {
	if settings.PolicySeed == nil {
		return 0
	}
	return *settings.PolicySeed
}

func (h dprepRoutes) resolvePolicy(ctx context.Context, account EarnAccount, tokenProgram solana.PublicKey, known *dprepKnownRoute) (dprepResolvedPolicy, error) {
	shapes, err := dprepShapesFor(account)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	if known != nil {
		return h.resolveKnownPolicy(ctx, account, tokenProgram, shapes, *known)
	}
	return h.scanPolicies(ctx, account, tokenProgram, shapes)
}

func (h dprepRoutes) resolveKnownPolicy(ctx context.Context, account EarnAccount, tokenProgram solana.PublicKey, shapes dprepPolicyShapes, known dprepKnownRoute) (dprepResolvedPolicy, error) {
	setupSeed := known.seed + 1
	if setupSeed > depositMaxPolicySeed {
		return dprepResolvedPolicy{}, errors.New("Earn setup policy seed is too large for this client.")
	}
	setupAddress, err := dprepPolicyAddress(account.Settings, setupSeed)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	settings, err := depositSettingsFor(ctx, h.Chain, account.Settings)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	route, err := h.policyAt(ctx, known.account, "Earn route policy")
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	setup, err := h.policyAt(ctx, setupAddress, "Earn setup policy")
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	if route == nil {
		return h.scanPolicies(ctx, account, tokenProgram, shapes)
	}
	generation := shapes.generation(*route)
	if generation == dprepGenerationNone {
		if err := h.assertCanonical(account, known.account, *route, shapes.route, "Earn route policy", known.seed); err != nil {
			return dprepResolvedPolicy{}, err
		}
		return dprepResolvedPolicy{}, errors.New("Earn route policy capability is not recognized.")
	}
	if err := h.assertCanonical(account, known.account, *route, shapes.routeFor(generation), "Earn route policy", known.seed); err != nil {
		return dprepResolvedPolicy{}, err
	}
	if err := dprepAssertTokenProgram(generation, tokenProgram); err != nil {
		return dprepResolvedPolicy{}, err
	}
	if setup != nil {
		if err := h.assertCanonical(account, setupAddress, *setup, shapes.setup, "Earn setup policy", setupSeed); err != nil {
			return dprepResolvedPolicy{}, err
		}
		return dprepResolvedPolicy{account: known.account, seed: known.seed, setupAccount: &setupAddress, setupSeed: setupSeed}, nil
	}
	if dprepCurrentSeed(settings) != known.seed {
		return dprepResolvedPolicy{}, errors.New("Earn setup policy is absent but the Settings seed has advanced. Refresh policy state before depositing.")
	}
	return h.createSetupOnly(ctx, account, known.seed)
}

func (s dprepPolicyShapes) routeFor(generation dprepGeneration) squads.Policy {
	if generation == dprepGenerationLegacy {
		return s.legacyRoute
	}
	return s.route
}

type dprepScannedRoute struct {
	address    solana.PublicKey
	decoded    squads.DecodedPolicy
	generation dprepGeneration
}

func (h dprepRoutes) scanPolicies(ctx context.Context, account EarnAccount, tokenProgram solana.PublicKey, shapes dprepPolicyShapes) (dprepResolvedPolicy, error) {
	settings, err := depositSettingsFor(ctx, h.Chain, account.Settings)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	installed, err := h.installedPolicies(ctx, account.Settings, settings)
	if err != nil {
		return dprepResolvedPolicy{}, fmt.Errorf("Cannot safely scan Earn policies. %w", err)
	}

	var routes []dprepScannedRoute
	setups := map[uint64]solana.PublicKey{}
	for _, entry := range installed {
		decoded := entry.decoded
		if decoded.VaultIndex != earnprep.EarnVaultIndex || len(decoded.Constraints) == 0 {
			continue
		}
		if !dprepAllKLend(decoded.Constraints) {
			continue
		}
		if generation := shapes.generation(decoded); generation != dprepGenerationNone {
			if err := h.assertCanonical(account, entry.address, decoded, shapes.routeFor(generation), "Earn route policy", decoded.Seed); err != nil {
				return dprepResolvedPolicy{}, err
			}
			routes = append(routes, dprepScannedRoute{address: entry.address, decoded: decoded, generation: generation})
			continue
		}
		if dprepPolicyOf(decoded).Equal(shapes.setup) {
			if err := h.assertCanonical(account, entry.address, decoded, shapes.setup, "Earn setup policy", decoded.Seed); err != nil {
				return dprepResolvedPolicy{}, err
			}
			setups[decoded.Seed] = entry.address
			continue
		}
		slog.Warn(fmt.Sprintf("[smart-account-vaults] skipping non-canonical Earn policy %s (seed %d) during policy scan", entry.address, decoded.Seed))
	}

	slices.SortStableFunc(routes, func(left, right dprepScannedRoute) int {
		if left.generation != right.generation {
			if left.generation == dprepGenerationCompatible {
				return -1
			}
			return 1
		}
		if left.decoded.Seed > right.decoded.Seed {
			return -1
		}
		return 1
	})
	var newest *dprepScannedRoute
	for i := range routes {
		route := routes[i]
		setupSeed := route.decoded.Seed + 1
		if setupSeed > depositMaxPolicySeed {
			return dprepResolvedPolicy{}, errors.New("Earn setup policy seed is too large for this client.")
		}
		if setupAddress, ok := setups[setupSeed]; ok {
			if err := dprepAssertTokenProgram(route.generation, tokenProgram); err != nil {
				return dprepResolvedPolicy{}, err
			}
			return dprepResolvedPolicy{account: route.address, seed: route.decoded.Seed, setupAccount: &setupAddress, setupSeed: setupSeed}, nil
		}
		setupAddress, err := dprepPolicyAddress(account.Settings, setupSeed)
		if err != nil {
			return dprepResolvedPolicy{}, err
		}
		occupied, err := h.policyAt(ctx, setupAddress, "Earn setup policy")
		if err != nil {
			return dprepResolvedPolicy{}, err
		}
		if occupied != nil {
			return dprepResolvedPolicy{}, fmt.Errorf("Policy seed %d is occupied by an incompatible Earn setup account.", setupSeed)
		}
		if newest == nil {
			newest = &routes[i]
		}
	}

	if newest != nil {
		if err := dprepAssertTokenProgram(newest.generation, tokenProgram); err != nil {
			return dprepResolvedPolicy{}, err
		}
		if dprepCurrentSeed(settings) != newest.decoded.Seed {
			return dprepResolvedPolicy{}, errors.New("Earn route policy has no setup twin and the Settings seed has advanced. Refresh policy state before depositing.")
		}
		return h.createSetupOnly(ctx, account, newest.decoded.Seed)
	}
	if len(setups) > 0 {
		return dprepResolvedPolicy{}, errors.New("Earn setup policy exists without its canonical route policy. Refresh policy state before depositing.")
	}

	nextSeed := squads.NextPolicySeed(settings)
	if nextSeed+1 > depositMaxPolicySeed {
		return dprepResolvedPolicy{}, errors.New("Earn setup policy seed is too large for this client.")
	}
	for _, candidate := range []struct {
		seed  uint64
		label string
	}{{nextSeed, "Earn route policy"}, {nextSeed + 1, "Earn setup policy"}} {
		address, err := dprepPolicyAddress(account.Settings, candidate.seed)
		if err != nil {
			return dprepResolvedPolicy{}, err
		}
		occupied, err := h.policyAt(ctx, address, candidate.label)
		if err != nil {
			return dprepResolvedPolicy{}, err
		}
		if occupied != nil {
			return dprepResolvedPolicy{}, fmt.Errorf("%s account is already occupied at the next Settings seed.", candidate.label)
		}
	}
	latest, err := depositSettingsFor(ctx, h.Chain, account.Settings)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	if squads.NextPolicySeed(latest) != nextSeed {
		return dprepResolvedPolicy{}, errors.New("Earn policy Settings seed changed during preparation. Refresh and try again.")
	}
	return h.createPolicies(account, nextSeed)
}

func dprepAllKLend(constraints []squads.InstructionConstraintView) bool {
	for _, constraint := range constraints {
		if constraint.ProgramID != depositKLendProgramID {
			return false
		}
	}
	return true
}

type dprepInstalledPolicy struct {
	address solana.PublicKey
	decoded squads.DecodedPolicy
}

func (h dprepRoutes) installedPolicies(ctx context.Context, settingsKey solana.PublicKey, settings squads.Settings) ([]dprepInstalledPolicy, error) {
	current := dprepCurrentSeed(settings)
	if current == 0 {
		return nil, nil
	}
	addresses := make([]solana.PublicKey, 0, current)
	for seed := uint64(1); seed <= current; seed++ {
		address, err := dprepPolicyAddress(settingsKey, seed)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	_, accounts, err := h.Chain.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	var out []dprepInstalledPolicy
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if account.Owner != squads.ProgramID {
			return nil, fmt.Errorf("Policy account %s has an unexpected owner.", account.Key)
		}
		decoded, err := squads.DecodeProgramInteractionPolicy(account)
		if err != nil {
			continue
		}
		if decoded.Settings != settingsKey {
			return nil, fmt.Errorf("Policy account %s belongs to an unexpected Settings account.", account.Key)
		}
		out = append(out, dprepInstalledPolicy{address: account.Key, decoded: decoded})
	}
	return out, nil
}

func (h dprepRoutes) createdPolicies(account EarnAccount, seed uint64) (*EarnPolicyPrepareResponse, error) {
	prior := seed - 1
	return depositPreparePolicyResponse(account, squads.Settings{PolicySeed: &prior}, h.PolicySigner)
}

func (h dprepRoutes) createPolicies(account EarnAccount, seed uint64) (dprepResolvedPolicy, error) {
	created, err := h.createdPolicies(account, seed)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	wire := created.PreparedPolicy
	setupAccount := solana.MustPublicKeyFromBase58(wire.SetupPolicy.Account)
	routeAccount := solana.MustPublicKeyFromBase58(wire.Policy.Account)
	return dprepResolvedPolicy{
		account:      routeAccount,
		seed:         seed,
		setupAccount: &setupAccount,
		setupSeed:    seed + 1,
		operation:    &wire.Prepared,
		finalize:     wire.FinalizePrepared,
		persistence:  &wire.Persistence,
		rent: []dprepRentCandidate{
			dprepPolicyRent(routeAccount, "Earn route policy account rent", wire.Prepared, "policy"),
			dprepPolicyRent(setupAccount, "Earn setup policy account rent", *wire.FinalizePrepared, "policy-finalize"),
		},
	}, nil
}

func (h dprepRoutes) createSetupOnly(ctx context.Context, account EarnAccount, seed uint64) (dprepResolvedPolicy, error) {
	latest, err := depositSettingsFor(ctx, h.Chain, account.Settings)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	if dprepCurrentSeed(latest) != seed {
		return dprepResolvedPolicy{}, errors.New("Earn policy Settings changed during setup resolution. Refresh and try again.")
	}
	created, err := h.createPolicies(account, seed)
	if err != nil {
		return dprepResolvedPolicy{}, err
	}
	return dprepResolvedPolicy{
		account:      created.account,
		seed:         seed,
		setupAccount: created.setupAccount,
		setupSeed:    created.setupSeed,
		finalize:     created.finalize,
		rent:         created.rent[1:],
	}, nil
}
