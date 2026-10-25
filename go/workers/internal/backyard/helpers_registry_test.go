package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// Test shorthands over the registry's Prime/PRIME/USDC lane, the PRIME/USDC
// graph most fixtures are built on.
func pinnedKaminoObservationConfig() (KaminoObservationConfig, error) {
	return primePRIMEUSDC.Kamino, nil
}

func ComputeRouteNAV(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{primePRIMEUSDC}, fixtureExitCosts())
}

func fixtureExitCosts() exitCosts {
	out := exitCosts{}
	for _, lane := range earnLanes {
		out[lane.route.Lane] = laneExitCosts{Realized: &ExitCost{Lane: lane.route.Lane, BPS: 1, QuotedCollateralRaw: 1, MarkSF: kaminoOne(), Source: exitCostRealized, Slot: 1, At: time.Unix(kaminoFixtureUnix, 0).UTC()}}
	}
	return out
}

func kaminoOne() (sf [16]byte) {
	sf[7] = 0x10
	return sf
}

func ComputeRouteNAVForRoute(slot int64, accounts []ConfirmedAccount, manifest RouteManifest, override *RouteNAVCustodies, route RuntimeRoute) (RouteNAVSnapshot, error) {
	return ComputeRouteNAVForRoutes(slot, accounts, manifest, override, []RuntimeRoute{route}, fixtureExitCosts())
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

// Test entry points over the production compilers and pricers.

// CompileBridgeMessage compiles a bridge request on the exact signing path
// without a key.
func CompileBridgeMessage(request BridgeBuildRequest) ([]byte, error) {
	return compileBridgeMessageForDelegate(rwa, request, mustKey(backyardDelegate))
}

// kaminoPrimeUSDCInstruction is a Kamino leg's instruction on its own lane.
func kaminoPrimeUSDCInstruction(request KaminoLegRequest) (compiledInstruction, kaminoLeg, error) {
	return kaminoRouteInstruction(rwa, request, request.RouteLane)
}

// boundedKaminoRepaymentEffects is a single-leg repay's effects: the debit
// between minimum and maximum, the persisted shape reconciliation checks.
func boundedKaminoRepaymentEffects(accounts []ConfirmedAccount, source, destination kaminoCustodyBoundary, minimum, maximum uint64) (ExpectedEffects, error) {
	effects, err := exactKaminoTokenEffects(accounts, source, destination, maximum)
	if err != nil {
		return ExpectedEffects{}, err
	}
	effects.Kind = "kamino-repay"
	effects.Repayment = &ExpectedRepayment{MinimumDebitRaw: minimum, MaximumDebitRaw: maximum}
	if err := validateRepaymentEffects(effects); err != nil {
		return ExpectedEffects{}, err
	}
	return effects, nil
}

func CompileJupiterMessage(request JupiterSwapRequest) ([]byte, error) {
	return compileJupiterMessageForDelegate(rwa, request, mustKey(backyardDelegate))
}

func CompileKaminoMessage(request KaminoLegRequest) ([]byte, error) {
	return compileKaminoMessageForDelegate(rwa, request, mustKey(backyardDelegate))
}

func selectRouteNAVAccountsForRoute(accounts []ConfirmedAccount, route RuntimeRoute) ([]ConfirmedAccount, error) {
	return selectRouteNAVAccountsForRoutes(rwa, accounts, []RuntimeRoute{route})
}

// Account layout offsets the fixtures write: LendingMarket.emergency_mode, and
// ReserveLiquidity.market_price_last_updated_ts.
const (
	kaminoMarketEmergencyModeOffset      = 122
	kaminoMarketPriceLastUpdatedTSOffset = 264
)

// bridgePolicyKeys are the Voltr bridge's four policies.
var bridgePolicyKeys = []policyKey{{action: VoltrAllocateToSquads}, {action: StageSquadsToVoltr}, {action: VoltrRestoreIdle}, {action: ReportNAV}}
