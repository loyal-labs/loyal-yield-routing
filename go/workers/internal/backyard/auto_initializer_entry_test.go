package backyard

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// entryQuoteFixture is a selector ENTER quote into lane: the shape target()
// reads from the selector's latest result.
func entryQuoteFixture(now time.Time, lane string, amount int64) MoveQuote {
	return MoveQuote{MinimumIdleRaw: uint64(amount), BorrowReceiveRaw: uint64(amount / 2), SourceLane: SelectedRouteID, DestinationLane: lane, ObservationID: "quoted-source", EquityRaw: amount, CostRaw: 1, ObservedAt: now.Add(-time.Second), EvidenceID: sha256Bytes([]byte("complete-recipe")), SampleSlot: 42, ValidThroughSlot: 74}
}

// autoEntryQuoteFixture is entryQuoteFixture into the AUTO lane: the PYUSD
// debt borrow legs carry the observed bounded debt price evidence the quote
// checks require on non-USDC debt lanes.
func autoEntryQuoteFixture(now time.Time, amount int64, price *BudgetPrice) MoveQuote {
	q := entryQuoteFixture(now, autoAUTOPYUSD.Lane, amount)
	q.SourceLane, q.BorrowFeeRaw, q.DebtPrice = autoAUTOPYUSD.Lane, 1_000, copyDebtPrice(price)
	return q
}

// seedAutoInitializerEntryOperation seeds one decided candidate initializer on
// a test-owned route key, recorded for the returned observation and decision
// exactly as RecordDecision persists it.
func seedAutoInitializerEntryOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string) (string, Observation, Decision) {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":2}',2)`, key); err != nil {
		t.Fatal(err)
	}
	observation := tickObservation(Snapshot{ObservationID: key + "-obs", Slot: 42, Fresh: true, RouteKind: RouteKind,
		RouteLane: f.request.RouteLane, StrategyKey: f.request.RouteLane})
	decision := Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}
	evidence, err := json.Marshal(newDecisionEvidence(observation, decision, sha256Bytes([]byte("manifest"))))
	if err != nil {
		t.Fatal(err)
	}
	id := key + "-op"
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5::jsonb || jsonb_build_object('decision',$6::jsonb))`, id, key, string(InitializeKaminoObligation), f.request.RouteLane, string(f.raw), string(evidence)); err != nil {
		t.Fatal(err)
	}
	return id, observation, decision
}

// The candidate AUTO initializer binds through the reviewed binding, reaches
// its signer, and passes the actual final-send fence: broadcast intent lands
// durably before the single broadcast.
func TestAutoInitializerEntryAuthorizesBindAndSend(t *testing.T) {
	// A dedicated disposable database: nothing shared with other tests.
	t.Parallel()
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_pilot_entry_test", 30*time.Second)
	defer cancel()
	var routeKeys []string
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	newOp := func(name string) (string, string, Observation, Decision) {
		t.Helper()
		key := "auto-pilot-entry-" + name + "-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		id, observation, decision := seedAutoInitializerEntryOperation(t, ctx, db, f, key)
		return id, key, observation, decision
	}
	bind := func(id string, observation Observation, decision Decision) error {
		return db.bindOperation(ctx, rpc, fixtureView(t, rpc), f.manifest, id, observation, decision, f.request, f.effects)
	}
	// The bind goes through the reviewed binding, and the builder stops only
	// at the absent signer.
	id, opKey, observation, decision := newOp("positive")
	if err := bind(id, observation, decision); err != nil {
		t.Fatalf("candidate bind refused: %v", err)
	}
	digest, err := Phase3IntentDigest(f.request, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	if auth.IntentSHA256 != digest || auth.BuildInput == nil || auth.SignedWireSHA256 != "" {
		t.Fatalf("bind drift: intent=%s buildInput=%v", auth.IntentSHA256, auth.BuildInput != nil)
	}
	if err = BuildSimulateAndPersistKaminoInitialization(ctx, db, rpc, fixtureView(t, rpc), id, f.manifest, f.request, Credentials{}); err == nil || err.Error() != "Backyard signing capability is not configured" {
		t.Fatalf("bound candidate build did not reach the signer boundary: %v", err)
	}
	if status := operationStatus(t, ctx, db, id); status != "decided" {
		t.Fatalf("build gates transitioned the journal: %s", status)
	}

	// The actual Signed transition: the locked final-send fence proves the
	// persisted wire through the same reviewed manifest, records broadcast
	// intent durably, and the wire lands after the single broadcast attempt.
	hash := sha256Bytes(f.wire)
	bindTestWire(t, ctx, db, id, hash)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3 WHERE operation_id=$1`, id, f.wire, hash); err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, RouteKey: opKey, StrategyKey: f.request.RouteLane, Decision: decision},
		Status: Signed, ExpectedEffects: persisted, SignedWire: f.wire, SignedWireSHA256: hash,
		TransactionSignature: encodeBase58(wireSig(f.wire)), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
	if err = advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, nil, op); err != nil {
		t.Fatal(err)
	}
	if got := operationStatus(t, ctx, db, id); got != "confirmed" {
		t.Fatalf("landed wire not confirmed: %s", got)
	}
	if *sends != 1 {
		t.Fatalf("broadcast attempted %d times before landing", *sends)
	}
	if err = markBroadcastIntent(ctx, db, f.manifest, op); err == nil {
		t.Fatal("completed send replayed")
	}
	if *sends != 1 {
		t.Fatalf("replay attempted another broadcast: %d", *sends)
	}

}

// storeTestSelectorEnter stores a selector result that enters quote's lane:
// target()'s flat-vault entry input.
func storeTestSelectorEnter(t *testing.T, ctx context.Context, db *Database, key string, quote MoveQuote) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"result": SelectorResult{Action: "ENTER", Reason: "persistent_net_benefit", DestinationLane: quote.DestinationLane, EquityRaw: quote.EquityRaw, SelectedQuote: &quote}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selector}',$2::jsonb,true) WHERE route_key=$1`, key, raw); err != nil {
		t.Fatal(err)
	}
}
