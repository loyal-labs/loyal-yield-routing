package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func initializationPlanningFixture(lane string) Observation {
	s := base()
	s.ObservationID = "initializer-planning"
	s.Slot = 42
	s.RouteLane = lane
	s.StrategyKey = lane
	s.PilotActive = true
	s.InitializationPolicyReady = true
	s.ObligationPresenceKnown = true
	s.VoltrIdleRaw = 1_000_000
	return tickObservation(s)
}
func TestInitializationDecisionPreservesRecoveryWithdrawalAndEntryGuards(t *testing.T) {
	for _, lane := range selectorLanes {
		o := initializationPlanningFixture(lane)
		d := Decide(o.Snapshot)
		if d.Action != InitializeKaminoObligation || d.Validate() != nil {
			t.Fatalf("initializer not selected for %s: %+v", lane, d)
		}
		for _, mutate := range []func(*Snapshot){
			func(s *Snapshot) { s.PilotActive = false }, func(s *Snapshot) { s.InitializationPolicyReady = false }, func(s *Snapshot) { s.ObligationPresenceKnown = false }, func(s *Snapshot) { s.ObligationPresent = true },
			func(s *Snapshot) { s.Nonterminal = Signed }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, func(s *Snapshot) { s.Unwind = true }, func(s *Snapshot) { s.SelectorEntryPaused = true },
			func(s *Snapshot) { s.SquadsIdleRaw = 1 }, func(s *Snapshot) { s.CollateralIdleRaw = 1 }, func(s *Snapshot) { s.PositionDebtRaw = 1 }, func(s *Snapshot) { s.CapacityRaw = 0 }, func(s *Snapshot) { s.PolicyReady = false }, func(s *Snapshot) { s.LiquidationThresholdBPS = TargetLTVBPS + 1500 },
		} {
			s := o.Snapshot
			mutate(&s)
			if got := Decide(s); got.Action == InitializeKaminoObligation {
				t.Fatalf("initializer bypassed prior work or prerequisites: %+v", s)
			}
		}
	}
}

func initializationRuntimeRPC(t *testing.T) (*RPCClient, RouteManifest, KaminoInitializationRequest, map[string]ConfirmedAccount) {
	t.Helper()
	r, accounts := initializationPrestateFixture(t)
	m := initializerManifestFixture(t)
	policy, _ := policySetupAddress(r.PolicySeed)
	for i, b := range m.RuntimeBindings.MultiplyInitializers {
		if b.Lane == r.RouteLane {
			m.RuntimeBindings.MultiplyInitializers[i] = KaminoInitializerBinding{r.RouteLane, r.PolicySeed, encodeBase58(policy[:]), r.PolicyAccountDataSHA256}
		}
	}
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		var result any
		switch body.Method {
		case "getMinimumBalanceForRentExemption":
			result = r.RentLamports
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]any{"slot": 42}, "value": map[string]any{"blockhash": r.RecentBlockhash, "lastValidBlockHeight": r.LastValidBlockHeight}}
		case "getMultipleAccounts":
			var addresses []string
			json.Unmarshal(body.Params[0], &addresses)
			if len(addresses) == 0 || addresses[0] != bridgeSettings {
				return base.RoundTrip(req)
			}
			values := make([]any, len(addresses))
			for i, address := range addresses {
				if a, ok := accounts[address]; ok {
					values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
				}
			}
			result = map[string]any{"context": map[string]any{"slot": 42}, "value": values}
		default:
			return base.RoundTrip(req)
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(out)), Header: make(http.Header)}, nil
	})
	return rpc, m, r, accounts
}
func TestInitializerPreparationMeasuresNativeFundingAndRefusesAccountRace(t *testing.T) {
	rpc, m, template, accounts := initializationRuntimeRPC(t)
	o := initializationPlanningFixture(template.RouteLane)
	d := Decide(o.Snapshot)
	observe := func(context.Context) (Observation, error) { return o, nil }
	got, r, err := prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	if err != nil || got != o || r.RentLamports != template.RentLamports || r.MaximumFeeLamports != 5000 {
		t.Fatalf("unmeasured initialization: %+v %v", r, err)
	}
	route, _ := runtimeRoute(r.RouteLane)
	accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: 1}
	_, _, err = prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	assertBudgetHold(t, err, "initializer_obligation_already_present")
	delete(accounts, route.Kamino.Obligation)
	o.Snapshot.WithdrawalDemandRaw = 1
	_, _, err = prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	assertBudgetHold(t, err, "initializer_decision_changed")
}

func TestInitializerProductionAdmissionReservesMeasuredRentAndExpense(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("initializer-admission-%d", time.Now().UnixNano())
	id := key + "-op"
	rpc, m, template, _ := initializationRuntimeRPC(t)
	o := initializationPlanningFixture(template.RouteLane)
	d := Decide(o.Snapshot)
	_, r, err := prepareKaminoInitialization(ctx, rpc, m, d, func(context.Context) (Observation, error) { return o, nil })
	if err != nil {
		t.Fatal(err)
	}
	prior := emptyTestBudget()
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	a := pilotTestAuthority(prior)
	a.Generation = 2
	a.FinalizedSlot = flat.Slot
	a.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	b, err := activatePilotBudget(prior, a)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"generation": 2, "phase3": b, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, m.SHA256, sha256Bytes([]byte("policies")))})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, d.Action, d.StrategyKey, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "initializer-admission", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	if err = db.admitKaminoInitialization(ctx, rpc, m, id, o, d, r); err != nil {
		t.Fatal(err)
	}
	var encodedBudget, encodedAuth []byte
	if err = db.pool.QueryRow(ctx, `SELECT s.state->'phase3',o.expected_effects->'phase3' FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE operation_id=$1`, id).Scan(&encodedBudget, &encodedAuth); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encodedBudget, &b) != nil || json.Unmarshal(encodedAuth, &auth) != nil {
		t.Fatal("decode")
	}
	reservation := b.Reservations[id]
	cost := auth.BridgeAdmission.CurrentCost
	if reservation.Recovery || reservation.ExitAfterMicros != 0 || reservation.ExecutionCostUpperMicros != cost.NetworkFeeMicros || reservation.UpperMicros != cost.TotalMicros || cost.SetupLamports != r.RentLamports || cost.SetupLamportsMicros == 0 {
		t.Fatal("initializer omitted rent/expense or consumed exit")
	}
	if err = db.admitKaminoInitialization(ctx, rpc, m, id, o, d, r); err != nil {
		t.Fatal("retry", err)
	}
	if err = authorizePhase3ProductionBuild(ctx, db, rpc, id, r, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}, auth.BuildInput.Effects); err != nil {
		t.Fatal("pre-signing authorization", err)
	}
}

func TestWorkerDispatchesInitializationOnlyAfterPersistedAdmission(t *testing.T) {
	o := initializationPlanningFixture(SelectedRouteID)
	d := Decide(o.Snapshot)
	m := readyWorkerManifest(t)
	var order []string
	w := &Worker{routeKey: productionRouteKey, manifest: m, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil }, observe: func(context.Context) (Observation, error) { return o, nil },
		prepareInitialization: func(context.Context, RouteManifest, Decision) (Observation, KaminoInitializationRequest, error) {
			order = append(order, "prepare")
			return o, KaminoInitializationRequest{RouteLane: SelectedRouteID}, nil
		},
		recordDecision: func(_ context.Context, _ string, _ Observation, got Decision, _, _ string) (DecisionRecord, error) {
			if got != d {
				t.Fatal("decision changed")
			}
			order = append(order, "record")
			return DecisionRecord{OperationID: "init", Status: Decided}, nil
		},
		admitInitialization: func(context.Context, string, Observation, Decision, KaminoInitializationRequest) error {
			order = append(order, "admit")
			return nil
		},
		buildInitialization: func(context.Context, string, KaminoInitializationRequest) error {
			order = append(order, "build")
			return nil
		},
	}}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"prepare", "record", "admit", "build"}) {
		t.Fatal(order)
	}
	w.runtime.admitInitialization = func(context.Context, string, Observation, Decision, KaminoInitializationRequest) error {
		return budgetHold("initializer_native_funding_unavailable")
	}
	w.runtime.buildInitialization = func(context.Context, string, KaminoInitializationRequest) error {
		t.Fatal("unadmitted native creation reached signer")
		return nil
	}
	journaled := false
	w.runtime.recordBudgetHold = func(context.Context, string, *BudgetHold) error { journaled = true; return nil }
	assertBudgetHold(t, w.Tick(context.Background()), "initializer_native_funding_unavailable")
	if !journaled {
		t.Fatal("admission hold not recorded")
	}
}
