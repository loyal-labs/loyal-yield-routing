package backyardrwa

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
)

// Execution admission prestate: strictly absent-only. The target obligation
// must not exist when an initializer is admitted for execution.
func validateKaminoInitializationPrestate(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, minimumSlot int64) (int64, error) {
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, selectorExitBound{}, false)
}

// Forecast-only prestate for the same-lane reentry quote. It prices recreation
// of an obligation the validated source exit is forecast to close, so it
// requires the exact observed funded lane obligation — same identity, decode
// evidence and exit-bound amounts — and weakens no other check. This never
// replaces the execution wrapper above, which still demands absence.
func validateKaminoReentryForecastPrestate(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound) (int64, error) {
	if bound.MaxCollateralRaw < 0 || bound.MaxDebtRaw < 0 {
		return 0, budgetHold("initializer_reentry_bound_invalid")
	}
	return observeKaminoInitializationPrestate(ctx, rpc, r, minimumSlot, bound, true)
}

func observeKaminoInitializationPrestate(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound, forecastExistingObligation bool) (int64, error) {
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if rpc == nil || err != nil || minimumSlot <= 0 {
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
	slot, accounts, err := rpc.GetMultipleAccountsWithOptional(ctx, addresses, minimumSlot, route.Kamino.Obligation)
	if err != nil {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	next, err := policySetupNextSeed(accountAt(accounts, bridgeSettings))
	if err != nil || next <= r.PolicySeed {
		return 0, budgetHold("initializer_settings_or_seed_changed")
	}
	p := accountAt(accounts, encodeBase58(policy[:]))
	if p.Owner != bridgeSquadsProgram || p.Executable || p.Lamports == 0 || sha256Bytes(p.Data) != r.PolicyAccountDataSHA256 {
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
	for _, mint := range []string{route.Kamino.CollateralMint, route.Kamino.DebtMint} {
		a := accountAt(accounts, mint)
		if a.Owner != classicTokenProgram || a.Executable || a.Lamports == 0 || len(a.Data) != 82 || a.Data[45] != 1 {
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
