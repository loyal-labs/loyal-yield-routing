package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Test shorthands over the registry's Prime/PRIME/USDC lane, the PRIME/USDC
// graph most fixtures are built on.
func pinnedKaminoObservationConfig() (KaminoObservationConfig, error) {
	return primePRIMEUSDC.Kamino, nil
}

func ComputeRouteNAV(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{primePRIMEUSDC})
}

func ComputeRouteNAVForRoute(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, route RuntimeRoute) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{route})
}

// fixtureRefresh is a valuation refresh over a fixture bank whose reserves
// the refresh leaves as they are: every requested address, absent ones as
// their address alone, at slot with the refresh's provenance.
func fixtureRefresh(slot int64, accounts []ConfirmedAccount) func(context.Context, []RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
	return func(_ context.Context, _ []RuntimeRoute, addresses []string, _ int64) (int64, []ConfirmedAccount, error) {
		out := make([]ConfirmedAccount, len(addresses))
		for i, address := range addresses {
			out[i] = ConfirmedAccount{Address: address}
			for _, account := range accounts {
				if account.Address == address {
					out[i] = account
					break
				}
			}
			out[i].ValuationSource, out[i].ValuationSlot = routeRefreshValuationSource, slot
		}
		return slot, out, nil
	}
}

// simulatedCapture answers a refresh simulateTransaction from accounts: the
// requested addresses, at slot, absent ones null.
func simulatedCapture(t testing.TB, params []json.RawMessage, slot int64, lookup func(string) (ConfirmedAccount, bool)) map[string]any {
	t.Helper()
	var options struct {
		Accounts struct {
			Addresses []string `json:"addresses"`
		} `json:"accounts"`
	}
	if len(params) < 2 || json.Unmarshal(params[1], &options) != nil {
		t.Fatal("simulateTransaction without an accounts capture")
	}
	values := make([]any, len(options.Accounts.Addresses))
	for i, address := range options.Accounts.Addresses {
		if a, ok := lookup(address); ok && a.Lamports > 0 {
			values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
		}
	}
	return map[string]any{"context": map[string]int64{"slot": slot}, "value": map[string]any{"err": nil, "accounts": values}}
}
