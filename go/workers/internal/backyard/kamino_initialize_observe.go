package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// Execution admission prestate: strictly absent-only. The target obligation
// must not exist when an initializer is admitted for execution. An unreviewed
// lane keeps the installed typed hold: admission prestate failure is a
// retryable observation outcome, not a fatal configuration error.
func validateKaminoInitializationPrestate(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, minimumSlot int64) (int64, error) {
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, selectorExitBound{}, false, inner)
}

// validateKaminoInitializationPrestateOnRoute is the manifest-aware form: the
// candidate AUTO lane is admitted only after the reviewed binding resolved the
// request; every absent-only check below is the exact installed check.
func (m RouteManifest) validateKaminoInitializationPrestate(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, minimumSlot int64) (int64, error) {
	if err := m.validateInitializationRequest(r); err != nil {
		return 0, err
	}
	_, inner, err := initializerRouteForRequest(r)
	if err != nil {
		return 0, err
	}
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, selectorExitBound{}, false, inner)
}

// validateKaminoReentryForecastPrestateOnRoute is the manifest-aware reentry
// forecast: identical shape and gates, bounded by the exact exit bound.
func (m RouteManifest) validateKaminoReentryForecastPrestate(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound) (int64, error) {
	if err := m.validateInitializationRequest(r); err != nil {
		return 0, err
	}
	_, inner, err := initializerRouteForRequest(r)
	if err != nil {
		return 0, err
	}
	if bound.MaxCollateralRaw < 0 || bound.MaxDebtRaw < 0 {
		return 0, budgetHold("initializer_reentry_bound_invalid")
	}
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, bound, true, inner)
}

// initializerRouteForRequest resolves the lane topology: installed selector
// lanes through the unchanged public gate, the candidate AUTO lane through the
// checked route-aware core (already binding-gated by the caller).
func initializerRouteForRequest(r KaminoInitializationRequest) (RuntimeRoute, compiledInstruction, error) {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return RuntimeRoute{}, compiledInstruction{}, err
	}
	if selectorLane(route.Lane) {
		inner, err := kaminoMultiplyInitializer(route.Lane)
		return route, inner, err
	}
	inner, err := kaminoRouteInitializer(route)
	return route, inner, err
}

// Forecast-only prestate for the same-lane reentry quote. It prices recreation
// of an obligation the validated source exit is forecast to close, so it
// requires the exact observed funded lane obligation — same identity, decode
// evidence and exit-bound amounts — and weakens no other check. This never
// replaces the execution wrapper above, which still demands absence. An
// unreviewed lane keeps the installed typed hold.
func validateKaminoReentryForecastPrestate(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound) (int64, error) {
	if bound.MaxCollateralRaw < 0 || bound.MaxDebtRaw < 0 {
		return 0, budgetHold("initializer_reentry_bound_invalid")
	}
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, bound, true, inner)
}

func observeKaminoInitializationPrestate(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound, forecastExistingObligation bool, inner compiledInstruction) (int64, error) {
	if rpc == nil || minimumSlot <= 0 {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	policy, err := policySetupAddress(r.PolicySeed)
	if err != nil {
		return 0, err
	}
	addresses := []string{bridgeSettings, encodeBase58(policy[:]), bridgeDelegate}
	for _, a := range inner.accounts {
		addresses = append(addresses, encodeBase58(a.key[:]))
	}
	seen := map[string]bool{}
	unique := addresses[:0]
	for _, address := range addresses {
		if !seen[address] {
			unique = append(unique, address)
			seen[address] = true
		}
	}
	addresses = unique
	route, _ := runtimeRoute(r.RouteLane)
	slot, accounts, err := confirmedAccounts(ctx, rpc, addresses, minimumSlot, route.Kamino.Obligation)
	if err != nil {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	next, err := policySetupNextSeed(accountAt(accounts, bridgeSettings))
	if err != nil || next <= r.PolicySeed {
		return 0, budgetHold("initializer_settings_or_seed_changed")
	}
	p := accountAt(accounts, encodeBase58(policy[:]))
	if p.Owner != squads.ProgramID.String() || p.Executable || p.Lamports == 0 || sha256Bytes(p.Data) != r.PolicyAccountDataSHA256 {
		return 0, budgetHold("initializer_policy_changed")
	}
	for _, a := range []struct {
		address string
		minimum uint64
	}{{bridgeVault, r.RentLamports}, {bridgeDelegate, r.MaximumFeeLamports}} {
		account := accountAt(accounts, a.address)
		if account.Owner != "11111111111111111111111111111111" || account.Executable || len(account.Data) != 0 || account.Lamports < a.minimum {
			return 0, budgetHold("initializer_native_funding_unavailable")
		}
	}
	o := accountAt(accounts, route.Kamino.Obligation)
	if o.Address != route.Kamino.Obligation || o.Executable {
		return 0, budgetHold("initializer_obligation_already_present")
	}
	if !forecastExistingObligation {
		if o.Lamports != 0 || len(o.Data) != 0 {
			return 0, budgetHold("initializer_obligation_already_present")
		}
	} else {
		decoded, err := decodeKaminoObligation(o, route.Kamino)
		if err != nil || o.Lamports == 0 || len(o.Data) == 0 ||
			int64(decoded.collateralDepositedRaw) != bound.MaxCollateralRaw || decoded.debtRaw > uint64(bound.MaxDebtRaw) {
			return 0, budgetHold("initializer_reentry_obligation_unobserved")
		}
	}
	metadata := accountAt(accounts, encodeBase58(inner.accounts[6].key[:]))
	if metadata.Owner != kaminoProgram || metadata.Lamports == 0 || metadata.Executable || len(metadata.Data) != 1032 ||
		!bytes.Equal(metadata.Data[:8], []byte{157, 214, 220, 235, 98, 135, 171, 28}) ||
		!allZero(metadata.Data[8:40]) || !sameKey(metadata.Data[80:112], bridgeVault) {
		return 0, budgetHold("initializer_metadata_unavailable")
	}
	if emergency, err := decodeKaminoMarketEmergency(accountAt(accounts, route.Kamino.Market), route.Kamino); err != nil || emergency {
		return 0, budgetHold("initializer_market_unavailable")
	}
	// Each seed mint must exist under its route's own reviewed token program
	// and pass the execution-mint parser every admitted leg uses (extension
	// whitelist, transfer fee and transfer hook disabled). The parser's
	// decimals argument pins the observed decimals byte to the supported
	// scale, the same <=18 bound decodeKaminoReserve enforces elsewhere;
	// absolute decimals identity stays pinned by each lane's reserve reads.
	for _, mint := range []struct{ address, program string }{
		{route.Kamino.CollateralMint, route.CollateralTokenProgram},
		{route.Kamino.DebtMint, route.DebtTokenProgram},
	} {
		a := accountAt(accounts, mint.address)
		if a.Lamports == 0 || len(a.Data) < 82 || validateExecutionMint(a, mint.program, a.Data[44]) != nil {
			return 0, budgetHold("initializer_seed_mint_unavailable")
		}
	}
	rent := accountAt(accounts, "SysvarRent111111111111111111111111111111111")
	if rent.Owner != "Sysvar1111111111111111111111111111111111111" || rent.Executable || len(rent.Data) != 17 {
		return 0, budgetHold("initializer_rent_unavailable")
	}
	perByte := binary.LittleEndian.Uint64(rent.Data[:8])
	threshold := math.Float64frombits(binary.LittleEndian.Uint64(rent.Data[8:16]))
	if perByte == 0 || perByte > math.MaxUint64/(kaminoObligationLength+128) || !finite(threshold) || threshold <= 0 {
		return 0, budgetHold("initializer_rent_invalid")
	}
	minimum := float64(perByte*(kaminoObligationLength+128)) * threshold
	if !finite(minimum) || minimum >= float64(math.MaxInt64) || uint64(minimum) != r.RentLamports {
		return 0, budgetHold("initializer_rent_changed")
	}
	return slot, nil
}

// Read only the deployed Settings fields required by the pinned admin's
// synchronous PolicyCreate. This is not a membership-management decoder.
func policySetupNextSeed(account ConfirmedAccount) (uint64, error) {
	bad := budgetHold("policy_setup_settings_envelope_mismatch")
	owner, err := decodeKey(account.Owner)
	if err != nil || account.Address != bridgeSettings || account.Lamports == 0 {
		return 0, bad
	}
	settings, err := squads.DecodeSettings(&chain.Account{Key: solana.PublicKey(mustKey(bridgeSettings)), Owner: solana.PublicKey(owner), Lamports: account.Lamports, Data: account.Data, Executable: account.Executable})
	// Zero Settings authority means the installed signer/threshold flow, not a
	// different authority able to bypass it. Never silently adapt membership.
	if err != nil || !settings.SettingsAuthority.IsZero() || settings.Threshold != 1 || settings.TimeLock != 0 ||
		len(settings.Signers) != 1 || settings.Signers[0].Key != solana.PublicKey(mustKey(bridgeSettingsSigner)) || settings.Signers[0].Permissions != squads.FullPermissions {
		return 0, bad
	}
	// policySeed must already exist for forward repair.
	if settings.PolicySeed == nil || settings.Reserved != 0 || *settings.PolicySeed < 139 || *settings.PolicySeed == math.MaxUint64 {
		return 0, bad
	}
	return *settings.PolicySeed + 1, nil
}

func policySetupAddress(seed uint64) (publicKey, error) {
	key, _, err := policySetupAddressAndBump(seed)
	return key, err
}

func policySetupAddressAndBump(seed uint64) (publicKey, byte, error) {
	if seed == 0 {
		return publicKey{}, 0, budgetHold("invalid_policy_setup_seed")
	}
	key, bump, err := squads.PolicyAddress(solana.PublicKey(mustKey(bridgeSettings)), seed)
	if err != nil {
		return publicKey{}, 0, budgetHold("invalid_policy_setup_seed")
	}
	return publicKey(key), bump, nil
}
