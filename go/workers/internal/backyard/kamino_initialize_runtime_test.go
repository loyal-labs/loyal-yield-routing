package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func initializationPlanningFixture(lane string) Observation {
	s := base()
	s.ObservationID = "initializer-planning"
	s.Slot = 42
	s.RouteLane = lane
	s.StrategyKey = lane
	s.ObligationPresenceKnown = true
	s.VoltrIdleRaw = 1_000_000
	s.SelectorEntryEquityRaw = 1_000_000
	s.CapacityRaw = 1_000_000
	s.PolicyLimitRaw = 1_000_000
	s.MaxTargetLTVEntryRaw = 1_000_000
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
			func(s *Snapshot) { s.ObligationPresenceKnown = false }, func(s *Snapshot) { s.ObligationPresent = true },
			func(s *Snapshot) { s.Nonterminal = Signed }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, func(s *Snapshot) { s.Unwind = true }, func(s *Snapshot) { s.SelectorEntryPaused = true },
			func(s *Snapshot) { s.SquadsIdleRaw = 1 }, func(s *Snapshot) { s.CollateralIdleRaw = 1 }, func(s *Snapshot) { s.PositionDebtRaw = 1 }, func(s *Snapshot) { s.CapacityRaw = 0 }, func(s *Snapshot) { s.LiquidationThresholdBPS = TargetLTVBPS + 1500 },
		} {
			s := o.Snapshot
			mutate(&s)
			if got := Decide(s); got.Action == InitializeKaminoObligation {
				t.Fatalf("initializer bypassed prior work or prerequisites: %+v", s)
			}
		}
	}
}

func initializationRuntimeRPC(t *testing.T) (*chain.Client, RouteManifest, KaminoInitializationRequest, map[string]ConfirmedAccount) {
	t.Helper()
	r, accounts := initializationPrestateFixture(t)
	m := embeddedTestManifest(t)
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			if len(addresses) == 0 || addresses[0] != bridgeDelegate {
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
	o.policies = testPolicies(t)
	d := Decide(o.Snapshot)
	observe := func(context.Context) (Observation, error) { return o, nil }
	got, r, err := prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	if err != nil || !reflect.DeepEqual(got, o) || r.RentLamports != template.RentLamports || r.MaximumFeeLamports != 5000 {
		t.Fatalf("unmeasured initialization: %+v %v", r, err)
	}
	route, _ := runtimeRoute(r.RouteLane)
	accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kamino.ProgramID.String(), Lamports: 1}
	_, _, err = prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	assertBudgetHold(t, err, "initializer_obligation_already_present")
	delete(accounts, route.Kamino.Obligation)
	o.Snapshot.WithdrawalDemandRaw = 1
	_, _, err = prepareKaminoInitialization(context.Background(), rpc, m, d, observe)
	assertBudgetHold(t, err, "initializer_decision_changed")
}

// The initializer binds its exact measured request, a second bind of the same
// operation is refused, and the send fence keeps the selector-entry expiry
// rule until the absent wire is retired.
func TestInitializerBindSendFenceAndExpiryRetirement(t *testing.T) {
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
	state, _ := json.Marshal(map[string]any{"generation": 2, "selectorEntry": selectorEntryFixture(time.Now().UTC(), template.RouteLane, o.Snapshot.SelectorEntryEquityRaw)})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, m.SHA256)})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, d.Action, d.StrategyKey, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "initializer-admission", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	if err = db.bindOperation(ctx, rpc, m, id, o, d, r, kaminoInitializationEffects(r)); err != nil {
		t.Fatal(err)
	}
	var encodedAuth []byte
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&encodedAuth); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encodedAuth, &auth) != nil || auth.BuildInput == nil {
		t.Fatal("decode")
	}
	if bound, _, _, err := auth.BuildInput.decodeWithManifest(m); err != nil || bound != r {
		t.Fatal("bind changed the measured initializer", err)
	}
	assertBudgetHold(t, db.bindOperation(ctx, rpc, m, id, o, d, r, kaminoInitializationEffects(r)), "bind_journal_mismatch")
	if err = db.requireBoundIntent(ctx, id, r, auth.BuildInput.Effects); err != nil {
		t.Fatal("pre-signing gate", err)
	}
	// Local unsigned wire fixture tests recovery only. No signer or send RPC
	// exists in this transport. Quote expiry must retain it until expiry and
	// a subsequent signature-absence observation prove it cannot land.
	_, _, message, err := auth.BuildInput.decode()
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	hash := sha256Bytes(wire)
	auth.SignedWireSHA256 = hash
	encodedAuth, _ = json.Marshal(auth)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,expected_effects=jsonb_set(expected_effects,'{phase3}',$3) WHERE operation_id=$1`, id, wire, encodedAuth); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, Decision: d}, Status: Signed, SignedWire: wire, SignedWireSHA256: hash, TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&op.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	// The initializer moves no principal: a quote past its slot window still
	// authorizes it until the entry's own expiry (2026-09-25). Only the send
	// fence is exercised here; the wall-clock expiry below still refuses.
	slotExpired := selectorEntryFixture(time.Now().UTC(), r.RouteLane, o.Snapshot.SelectorEntryEquityRaw)
	slotExpired.Quote.SampleSlot, slotExpired.Quote.ValidThroughSlot = 9, 41
	storeTestSelectorEntry(t, ctx, db, key, slotExpired)
	if err = markBroadcastIntent(ctx, db, m, op); err != nil && strings.Contains(err.Error(), "selector_entry_quote_expired") {
		t.Fatal("slot-late initializer refused before entry expiry", err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',broadcast_intent_at=NULL WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	storeTestSelectorEntry(t, ctx, db, key, selectorEntryFixture(time.Now().UTC().Add(-time.Minute), r.RouteLane, o.Snapshot.SelectorEntryEquityRaw))
	assertBudgetHold(t, markBroadcastIntent(ctx, db, m, op), "selector_entry_quote_expired")
	baseTransport := rpcOf(rpc).Transport
	finalizedExpired, absenceRead := false, false
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
		case "getEpochInfo":
			var config map[string]string
			if len(body.Params) > 0 {
				_ = json.Unmarshal(body.Params[0], &config)
			}
			if config["commitment"] != "finalized" {
				return baseTransport.RoundTrip(req)
			}
			result = finalizedEpoch(r.LastValidBlockHeight)
			if finalizedExpired {
				result = finalizedEpoch(r.LastValidBlockHeight + 1)
			}
		case "getSignatureStatuses":
			absenceRead = finalizedExpired
			result = map[string]any{"context": map[string]int{"slot": 5000}, "value": []any{nil}}
		default:
			return baseTransport.RoundTrip(req)
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		return response(string(out)), nil
	})
	assertBudgetHold(t, AdvanceNonterminal(ctx, db, rpc, nil, op), "selector_entry_quote_expired")
	var status string
	if err = db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil || status != "signed" || absenceRead {
		t.Fatal("unexpired wire retired", err, status)
	}
	finalizedExpired = true
	if err = AdvanceNonterminal(ctx, db, rpc, nil, op); err != nil {
		t.Fatal("expired absent wire not retired", err)
	}
	var storedWire []byte
	if err = db.pool.QueryRow(ctx, `SELECT status,signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &storedWire); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !absenceRead || !bytes.Equal(storedWire, wire) {
		t.Fatalf("expiry retirement: status=%s absence=%t wireEqual=%t", status, absenceRead, bytes.Equal(storedWire, wire))
	}
}

func TestWorkerDispatchesInitializationOnlyAfterPersistedBind(t *testing.T) {
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
		recordDecision: func(_ context.Context, _ string, _ Observation, got Decision, _ string) (DecisionRecord, error) {
			if got != d {
				t.Fatal("decision changed")
			}
			order = append(order, "record")
			return DecisionRecord{OperationID: "init", Status: Decided}, nil
		},
		bind: func(_ context.Context, _ string, _ Observation, _ Decision, request any, _ ExpectedEffects) error {
			if _, ok := request.(KaminoInitializationRequest); !ok {
				t.Fatal("bind received another request")
			}
			order = append(order, "bind")
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
	if !reflect.DeepEqual(order, []string{"prepare", "record", "bind", "build"}) {
		t.Fatal(order)
	}
	w.runtime.bind = func(context.Context, string, Observation, Decision, any, ExpectedEffects) error {
		return budgetHold("initializer_native_funding_unavailable")
	}
	w.runtime.buildInitialization = func(context.Context, string, KaminoInitializationRequest) error {
		t.Fatal("unbound native creation reached signer")
		return nil
	}
	journaled := false
	w.runtime.recordBudgetHold = func(context.Context, string, *BudgetHold) error { journaled = true; return nil }
	assertBudgetHold(t, w.Tick(context.Background()), "initializer_native_funding_unavailable")
	if !journaled {
		t.Fatal("bind hold not recorded")
	}
}

// A signed AUTO initializer must reload after a restart through the manifest
// that admitted it; the embedded validator refuses the candidate lane and
// left the live route unable to finish its own initializer (2026-09-24).
func TestNonterminalAutoInitializerRestoresOnManifest(t *testing.T) {
	d := Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: autoAUTOPYUSD.Lane, IdempotencyKey: "obs:initialize:AUTO/AUTO/PYUSD"}
	effects := []byte(`{"decision":{"reason":"multiply_obligation_missing","amountRaw":0,"strategyKey":"AUTO/AUTO/PYUSD"}}`)
	if _, err := restorePersistedDecision(effects, d.Action, d.IdempotencyKey, d.StrategyKey); err == nil {
		t.Fatal("embedded restore unexpectedly admits the candidate lane")
	}
	m := embeddedTestManifest(t)
	got, err := restorePersistedDecisionWith(effects, d.Action, d.IdempotencyKey, d.StrategyKey, m.validateDecision)
	if err != nil || got != d {
		t.Fatal("manifest restore refused its own AUTO initializer", got, err)
	}
}
