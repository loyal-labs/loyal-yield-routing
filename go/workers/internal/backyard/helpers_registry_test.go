package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
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

// Test entry points over the production compilers and pricers.

// MeasureExecutableDebit shares the production compilers and custody graph.
// It cannot accept an arbitrary caller-supplied action-to-value mapping.
// A withdrawal's wire amount may be receipt units: charge its underlying
// liquidity debit from the checked effect graph, not that receipt amount.
func MeasureExecutableDebit(request any, effects ExpectedEffects) (ExecutableDebit, error) {
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		return ExecutableDebit{}, err
	}
	return m.measureExecutableDebit(request, effects)
}

func CompileJupiterMessage(request JupiterSwapRequest) ([]byte, error) {
	return compileJupiterMessageForDelegate(request, mustKey(bridgeDelegate))
}

func CompileKaminoMessage(request KaminoPrimeUSDCRequest) ([]byte, error) {
	return compileKaminoMessageForDelegate(request, mustKey(bridgeDelegate))
}

// Classify only compiler-supported movements. The existing prestate and final
// reconciliation gates establish conservation, fees and actual rent ownership.
// Missing bounds are a refusal, never permission to recycle the entire debit.
func classifyPilotExecutionCost(request any, effects ExpectedEffects, cost ValuedTransactionCost, credit *BudgetPrice) (PilotExecutionCost, error) {
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		return PilotExecutionCost{}, err
	}
	return m.classifyPilotExecutionCost(request, effects, cost, credit)
}

// collectSelectorQuotes prices a source once, then at most three independent
// destinations, each with a biggest-first sizing ladder of at most three
// exact-size quotes. Nothing here writes a journal row or signs a transaction.
func collectSelectorQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	return collectSelectorQuotesForLane(ctx, rpc, view, client, manifest, o, markets, policy, "", canaryMaximum...)
}

// Price every compiled message, including repeated NAV reports. Share only
// observed prices within this bounded sample; fees still bind exact messages.
// The production build/send path keeps its actual custody/prestate validators.
func priceSelectorRecipe(ctx context.Context, rpc *chain.Client, view *View, lane string, inputs []*phase3BuildInput, minimumSlot int64) (selectorRecipe, error) {
	return priceSelectorRecipeWithFloor(ctx, rpc, view, lane, inputs, minimumSlot, minimumSlot)
}

// Keep the sample start fixed while later prerequisites advance the minimum
// slot accepted for fees/prices and final collection.
func priceSelectorRecipeWithFloor(ctx context.Context, rpc *chain.Client, view *View, lane string, inputs []*phase3BuildInput, minimumSlot, observationFloor int64) (selectorRecipe, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return selectorRecipe{Inputs: inputs}, err
	}
	return manifest.priceSelectorRecipeWithFloor(ctx, rpc, view, lane, inputs, minimumSlot, observationFloor)
}

func selectRouteNAVAccountsForRoute(accounts []ConfirmedAccount, route RuntimeRoute) ([]ConfirmedAccount, error) {
	return selectRouteNAVAccountsForRoutes(accounts, []RuntimeRoute{route})
}

// A complete move includes the existing source exit and destination entry.
// Its evidence authorizes no transaction; runtime still reserves and rebuilds
// each leg against actual balances. A destination is reselected after unwind.
func observeSelectorMove(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation, market LaneEconomics, requestedEquity, idleBuffer uint64) (MoveQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var empty MoveQuote
	if o.ObservedAt.IsZero() || !freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) || requestedEquity == 0 || requestedEquity > strategyTwoBridgeLegCapRaw {
		return empty, budgetHold("invalid_selector_move")
	}
	source, err := observeSelectorSource(ctx, rpc, view, client, m, o)
	if err != nil {
		return empty, err
	}
	if source.MinimumIdleRaw <= idleBuffer {
		return empty, budgetHold("selector_move_has_no_entry_cash")
	}
	equity := min(requestedEquity, source.MinimumIdleRaw-idleBuffer)
	if equity == 0 {
		return empty, budgetHold("selector_move_has_no_entry_cash")
	}
	destination, err := observeSelectorDestinationForecast(ctx, rpc, view, client, m, o.policies, market, equity, o.Snapshot.Slot, false)
	if err != nil {
		return empty, err
	}
	return composeSelectorMove(ctx, view, o, source, destination)
}

// Account layout offsets the fixtures write: LendingMarket.emergency_mode, and
// ReserveLiquidity.market_price_last_updated_ts.
const (
	kaminoMarketEmergencyModeOffset      = 122
	kaminoMarketPriceLastUpdatedTSOffset = 264
)

// bridgePolicyKeys are the Voltr bridge's four policies.
var bridgePolicyKeys = []policyKey{{action: VoltrAllocateToSquads}, {action: StageSquadsToVoltr}, {action: VoltrRestoreIdle}, {action: ReportNAV}}
