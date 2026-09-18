package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// reentryObservation is tickObservation with a current wall clock, as the
// live selector evaluation provides.
func reentryObservation(snapshot Snapshot) Observation {
	o := tickObservation(snapshot)
	o.ObservedAt = time.Now().UTC()
	return o
}

func reentryRPCResult(id any, result any) *http.Response {
	encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return response(string(encoded))
}

// reentryPrestateTransport answers the obligation-recreation rent read and the
// initializer prestate batch — which carries the funded obligation this exit is
// forecast to close — on top of the controlled destination fixture transport.
func reentryPrestateTransport(t *testing.T, base http.RoundTripper, rent uint64, prestate map[string]ConfirmedAccount) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     any               `json:"id"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		switch body.Method {
		case "getMinimumBalanceForRentExemption":
			return reentryRPCResult(body.ID, rent), nil
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			if len(addresses) > 0 && addresses[0] == bridgeSettings {
				values := make([]any, len(addresses))
				for i, address := range addresses {
					if a, ok := prestate[address]; ok {
						values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
					}
				}
				return reentryRPCResult(body.ID, map[string]any{"context": map[string]int{"slot": 42}, "value": values}), nil
			}
		}
		return base.RoundTrip(req)
	})
}

// reentryFundedFixture rewrites the flat destination into the funded same-lane
// position, prices its complete source exit with the production observer, then
// overlays the initializer prestate graph and exact rent read. The source is
// produced before the overlay so it stays an independent completed quote.
func reentryFundedFixture(t *testing.T) (RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount, Observation, selectorSourceQuote, uint64) {
	t.Helper()
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	obligation := obligationFixture(t, 42, 15_000_000, 5_000_000)
	putKey(t, obligation.Data[32:64], route.Kamino.Market)
	putKey(t, obligation.Data[96:128], route.Kamino.CollateralReserve)
	putKey(t, obligation.Data[1208:1240], route.Kamino.DebtReserve)
	copy(accountAt(accounts, route.Kamino.Obligation).Data, obligation.Data)
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "reentry", RouteKind: RouteKind, RouteLane: route.Lane, StrategyKey: route.Lane, VoltrIdleRaw: 90_000_000,
		HasPosition: true, ObligationPresent: true, ObligationPresenceKnown: true, PositionCollateralRaw: 15_000_000, PositionCollateralValueRaw: 15_000_000, PositionDebtRaw: 5_000_000, PositionDebtValueRaw: 5_000_000, StrategyNAVRaw: 10_000_000, TotalVaultNAVRaw: 100_000_000, ReportSnapshotDigest: sha256Bytes([]byte("reentry-nav"))}
	o := reentryObservation(s)
	source, err := observeSelectorSource(context.Background(), rpc, client, m, o)
	if err != nil {
		t.Fatal(err)
	}
	if source.ExitBound == nil || source.ExitBound.MaxCollateralRaw != s.PositionCollateralRaw || source.ExitBound.MaxDebtRaw < s.PositionDebtRaw {
		t.Fatal("incomplete same-lane source exit", source.ExitBound)
	}
	request, prestate := initializationPrestateFixture(t)
	if request.RouteLane != route.Lane {
		t.Fatalf("initializer fixture lane %s does not match destination lane", request.RouteLane)
	}
	policy, _ := policySetupAddress(request.PolicySeed)
	// Replace any installed release pins with the controlled three-lane set.
	m.RuntimeBindings.MultiplyInitializers = nil
	for _, b := range initializerManifestFixture(t).RuntimeBindings.MultiplyInitializers {
		if b.Lane == route.Lane {
			m.RuntimeBindings.MultiplyInitializers = append(m.RuntimeBindings.MultiplyInitializers, KaminoInitializerBinding{route.Lane, request.PolicySeed, encodeBase58(policy[:]), request.PolicyAccountDataSHA256})
		} else {
			m.RuntimeBindings.MultiplyInitializers = append(m.RuntimeBindings.MultiplyInitializers, b)
		}
	}
	prestate[route.Kamino.Obligation] = accountAt(accounts, route.Kamino.Obligation)
	rpc.client.Transport = reentryPrestateTransport(t, rpc.client.Transport, request.RentLamports, prestate)
	return m, rpc, client, accounts, o, source, request.RentLamports
}

func TestSelectorReentryForecastIncludesObligationRecreation(t *testing.T) {
	m, rpc, client, _, o, source, rent := reentryFundedFixture(t)
	q, err := observeSelectorReentryDestinationSize(context.Background(), rpc, client, m, o, source, 10_000_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if q.Lane != SelectedRouteID || q.EquityRaw == 0 || q.EquityRaw > 10_000_000 || q.AccountSlot != 42 || q.Recipe.EvidenceID == "" {
		t.Fatal("incomplete reentry quote", q)
	}
	// Recreation rent and the exact initializer fee must be in the recipe even
	// though the obligation is observed present: the full exit closes it.
	if q.Recipe.SetupLamports != rent || q.Recipe.NetworkLamports != 65_000 {
		t.Fatal("obligation recreation rent/fee missing", q.Recipe.SetupLamports, q.Recipe.NetworkLamports)
	}
	if len(q.Recipe.Inputs) != 13 {
		t.Fatal("recipe step count", len(q.Recipe.Inputs))
	}
	request, _, _, err := q.Recipe.Inputs[0].decode()
	if err != nil {
		t.Fatal(err)
	}
	init, ok := request.(KaminoInitializationRequest)
	if !ok || init.RouteLane != SelectedRouteID || init.RentLamports != rent || init.MaximumFeeLamports != 5_000 {
		t.Fatal("initializer not priced first", request)
	}
	if q.PayoffUpperRaw < q.BorrowReceiveRaw+q.BorrowFeeRaw || q.PayoffSwap.Request.MinimumOutputRaw < q.PayoffUpperRaw {
		t.Fatal("reentry payoff unquoted", q.PayoffUpperRaw, q.PayoffSwap.Request.MinimumOutputRaw)
	}
	// The ordinary flat destination wrapper keeps refusing the funded lane.
	_, err = observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 1_000_000, 42)
	assertBudgetHold(t, err, "selector_destination_not_flat")
}

func TestSelectorReentryRejectsMissingSourceBoundAndUnrelatedQuote(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	idle := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "idle", RouteLane: route.Lane, StrategyKey: route.Lane, VoltrIdleRaw: 10_000_000}
	o := reentryObservation(idle)
	source, err := observeSelectorSource(context.Background(), rpc, client, m, o)
	if err != nil || source.ExitBound != nil {
		t.Fatal("idle source unexpectedly bound", err, source.ExitBound)
	}
	// An unbound idle source cannot forecast reentry, and its observation is
	// not a completed funded one either.
	_, err = observeSelectorReentryDestinationSize(context.Background(), rpc, client, m, o, source, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_destination_unavailable")

	fundedM, fundedRPC, fundedClient, _, fundedO, fundedSource, _ := reentryFundedFixture(t)
	ctx := context.Background()
	// A completed funded observation with a boundless source is still rejected.
	boundless := fundedSource
	boundless.ExitBound = nil
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, boundless, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_exit_bound_unavailable")
	unrelated := fundedO
	unrelated.Snapshot.ObservationID = "changed"
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, unrelated, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_exit_bound_unavailable")
	other := PhaseOneLaneID
	if other == SelectedRouteID {
		t.Fatal("fixture lanes collide")
	}
	switched := fundedO
	switched.Snapshot.RouteLane, switched.Snapshot.StrategyKey = other, other
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, switched, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_destination_unavailable")
	// Only a completed funded observation — obligation, position, NAV — may
	// forecast reentry of its own position.
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.HasPosition = false },
		func(s *Snapshot) { s.ObligationPresent = false },
		func(s *Snapshot) { s.ObligationPresenceKnown = false },
		func(s *Snapshot) { s.PositionCollateralRaw = 0 },
		func(s *Snapshot) { s.StrategyNAVRaw = 0 },
	} {
		incomplete := fundedO
		mutate(&incomplete.Snapshot)
		_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, incomplete, fundedSource, 1_000_000, true)
		assertBudgetHold(t, err, "selector_reentry_destination_unavailable")
	}
	// The source bound must describe exactly the observation's position.
	mismatched := fundedSource
	mismatched.ExitBound = &selectorExitBound{MaxCollateralRaw: 14_999_999, MaxDebtRaw: 5_000_000}
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, mismatched, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_exit_bound_unavailable")
	shortDebt := fundedSource
	shortDebt.ExitBound = &selectorExitBound{MaxCollateralRaw: 15_000_000, MaxDebtRaw: 4_999_999}
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, shortDebt, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_exit_bound_unavailable")
}

func TestSelectorReentryRejectsFlatDestinationAndDriftedBound(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	// A structurally valid exit bound over a flat, incomplete observation must
	// not open the reentry forecast: the ordinary quote owns that case.
	source := selectorSourceQuote{Lane: route.Lane, ObservationID: "flat", MinimumIdleRaw: 2_000_000,
		Recipe: selectorRecipe{ValidThroughSlot: 74, EvidenceID: sha256Bytes([]byte("flat"))}, ExitBound: &selectorExitBound{}}
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "flat", RouteLane: route.Lane, StrategyKey: route.Lane, VoltrIdleRaw: 2_000_000}
	_, err := observeSelectorReentryDestinationSize(context.Background(), rpc, client, m, reentryObservation(s), source, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_destination_unavailable")

	fundedM, fundedRPC, fundedClient, fundedAccounts, fundedO, fundedSource, _ := reentryFundedFixture(t)
	fundedRoute, _ := runtimeRoute(SelectedRouteID)
	ctx := context.Background()
	// Source price fence: reentry equity cannot exceed the exit's minimum cash.
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, fundedSource.MinimumIdleRaw+1, true)
	assertBudgetHold(t, err, "selector_reentry_equity_unavailable")
	// Approved tranche cap fence.
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, 100_000_000_001, true)
	assertBudgetHold(t, err, "selector_reentry_equity_unavailable")
	// Observation freshness fence.
	stale := fundedO
	stale.ObservedAt = time.Now().UTC().Add(-time.Minute)
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, stale, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_destination_unavailable")
	// Source recipe slot window fence.
	expired := fundedSource
	expired.Recipe.ValidThroughSlot = fundedO.Snapshot.Slot - 1
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, expired, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_exit_bound_unavailable")
	// Collateral left the bound position.
	obligation := accountAt(fundedAccounts, fundedRoute.Kamino.Obligation)
	binary.LittleEndian.PutUint64(obligation.Data[96+32:96+40], 14_999_999)
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_position_drifted")
	// Collateral custody residue beyond the idle amount the exit swaps back.
	binary.LittleEndian.PutUint64(obligation.Data[96+32:96+40], 15_000_000)
	binary.LittleEndian.PutUint64(accountAt(fundedAccounts, fundedRoute.CollateralCustody).Data[64:72], 1_000)
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_position_drifted")
	// Debt beyond the payoff upper the exit reservation covers.
	binary.LittleEndian.PutUint64(accountAt(fundedAccounts, fundedRoute.CollateralCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(obligation.Data[1208+88:1208+96], 50_000_000)
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_position_drifted")
	// A position that went flat underneath the bound no longer matches it.
	binary.LittleEndian.PutUint64(obligation.Data[96+32:96+40], 0)
	_, err = observeSelectorReentryDestinationSize(ctx, fundedRPC, fundedClient, fundedM, fundedO, fundedSource, 1_000_000, true)
	assertBudgetHold(t, err, "selector_reentry_position_drifted")
}

func TestSelectorReentryForecastPrestateRejectsInvalidBoundsAndUnreviewedLanes(t *testing.T) {
	request, _ := initializationPrestateFixture(t)
	rpc := budgetBuildRPC(t, 5000, 42)
	_, err := validateKaminoReentryForecastPrestate(context.Background(), rpc, request, 42, selectorExitBound{MaxCollateralRaw: -1, MaxDebtRaw: 5_000_000})
	assertBudgetHold(t, err, "initializer_reentry_bound_invalid")
	_, err = validateKaminoReentryForecastPrestate(context.Background(), rpc, request, 42, selectorExitBound{MaxCollateralRaw: 15_000_000, MaxDebtRaw: -1})
	assertBudgetHold(t, err, "initializer_reentry_bound_invalid")
	unreviewed := request
	unreviewed.RouteLane = "Not/ALane/USDC"
	_, err = validateKaminoReentryForecastPrestate(context.Background(), rpc, unreviewed, 42, selectorExitBound{MaxCollateralRaw: 15_000_000, MaxDebtRaw: 5_000_000})
	assertBudgetHold(t, err, "initializer_prestate_unavailable")
	// The execution admission wrapper keeps demanding obligation absence even
	// against the same funded fixture the forecast wrapper accepts.
	_, fundedRPC, _, _, _, _, _ := reentryFundedFixture(t)
	_, err = validateKaminoInitializationPrestate(context.Background(), fundedRPC, request, 42)
	assertBudgetHold(t, err, "initializer_obligation_already_present")
}
