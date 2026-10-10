package backyard

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// autoSelectorEntryFixture derives the shared quote fixture into a coherent
// candidate AUTO entry: the entry funds the AUTO-lane obligation out of the
// same lane's idle equity, so source and destination are the AUTO lane, and
// the PYUSD-debt borrow legs carry the observed bounded debt price evidence
// the installed quote checks already require on non-USDC debt lanes.
func autoSelectorEntryFixture(now time.Time, amount int64, price *BudgetPrice) SelectorEntry {
	entry := selectorEntryFixture(now, autoAUTOPYUSD.Lane, amount)
	entry.Quote.SourceLane = autoAUTOPYUSD.Lane
	entry.Quote.BorrowFeeRaw = 1_000
	entry.Quote.DebtPrice = copyDebtPrice(price)
	return entry
}

// seedAutoInitializerEntryOperation seeds one decided candidate initializer on
// a test-owned route key, recorded for the returned observation and decision
// exactly as RecordDecision persists it, with a persisted candidate selector
// entry when storeEntry is set.
func seedAutoInitializerEntryOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string, price *BudgetPrice, entry *SelectorEntry, storeEntry bool) (string, Observation, Decision) {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":2}',2)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "auto-entry-test", time.Minute); err != nil {
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
	if entry == nil {
		fresh := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, price)
		entry = &fresh
	}
	if storeEntry {
		storeTestSelectorEntry(t, ctx, db, key, *entry)
	}
	return id, observation, decision
}

// The candidate AUTO initializer binds through the selector-entry fence and
// the reviewed binding, reaches its signer, and passes the actual final-send
// fence with a persisted candidate selector entry: broadcast intent lands
// durably before the single refused broadcast, and every installed public
// path — embedded prestate gate, embedded final-send entrypoint, embedded
// entry validation — still refuses the same durable rows.
func TestAutoInitializerEntryAuthorizesBindAndSend(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_pilot_entry_test", 30*time.Second)
	defer cancel()
	var routeKeys []string
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)

	newOp := func(name string, entry *SelectorEntry, storeEntry bool) (string, string, Observation, Decision) {
		t.Helper()
		key := "auto-pilot-entry-" + name + "-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		id, observation, decision := seedAutoInitializerEntryOperation(t, ctx, db, f, key, &price, entry, storeEntry)
		return id, key, observation, decision
	}
	bind := func(id string, observation Observation, decision Decision) error {
		return db.bindOperation(ctx, rpc, f.manifest, id, observation, decision, f.request, f.effects)
	}
	expectBindHold := func(id string, observation Observation, decision Decision, reason string) {
		t.Helper()
		assertBudgetHold(t, bind(id, observation, decision), reason)
		if status := operationStatus(t, ctx, db, id); status != "decided" {
			t.Fatalf("refusal transitioned the journal: %s", status)
		}
		if auth := loadAutoInitializerAuth(t, ctx, db, id); auth.BuildInput != nil {
			t.Fatal("refused candidate persisted a build input")
		}
	}

	// Positive candidate: the entry admits the bind through the reviewed
	// binding, and the builder stops only at the absent signer.
	id, opKey, observation, decision := newOp("positive", nil, true)
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
	if err = BuildSimulateAndPersistKaminoInitialization(ctx, db, rpc, id, f.manifest, f.request, Credentials{}); err == nil || err.Error() != "Backyard signing capability is not configured" {
		t.Fatalf("bound candidate build did not reach the signer boundary: %v", err)
	}
	if status := operationStatus(t, ctx, db, id); status != "decided" {
		t.Fatalf("build gates transitioned the journal: %s", status)
	}
	// The initializer fence never binds the entry allocation: that bind
	// belongs to the funding operation.
	var allocationID string
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(state->'selectorEntry'->>'allocationOperationId','') FROM loyal_yield.multiply_route_states WHERE route_key=$1`, opKey).Scan(&allocationID); err != nil {
		t.Fatal(err)
	}
	if allocationID != "" {
		t.Fatalf("initializer bind bound the entry allocation: %q", allocationID)
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
		TransactionSignature: encodeBase58(f.wire[1:65]), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
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

	// Missing durable entry.
	missingID, _, missingObservation, missingDecision := newOp("missing-entry", nil, false)
	expectBindHold(missingID, missingObservation, missingDecision, "selector_entry_authority_mismatch")
	// A slot-expired quote may still initialize the empty obligation before
	// the entry's wall-clock expiry, but cannot allocate principal. Keep the
	// debt price coherent with the older sample so only slot currentness differs.
	slotExpired := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	slotExpired.Quote.SampleSlot, slotExpired.Quote.ValidThroughSlot = 30, 41
	earlyPrice := price
	earlyPrice.ObservedSlot, earlyPrice.ValidThroughSlot = 30, 30+budgetMaxObservationLagSlots
	slotExpired.Quote.DebtPrice = &earlyPrice
	slotID, slotKey, slotObservation, slotDecision := newOp("slot-expired", &slotExpired, true)
	if err = bind(slotID, slotObservation, slotDecision); err != nil {
		t.Fatalf("slot-late initializer refused before entry expiry: %v", err)
	}
	var slotAllocation string
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(state->'selectorEntry'->>'allocationOperationId','') FROM loyal_yield.multiply_route_states WHERE route_key=$1`, slotKey).Scan(&slotAllocation); err != nil {
		t.Fatal(err)
	}
	if slotAllocation != "" || operationStatus(t, ctx, db, slotID) != "decided" {
		t.Fatal("slot-late initializer changed the allocation or journal state")
	}
	// The same persisted quote must fail the real locked allocation fence.
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	allocation := BridgeBuildRequest{Action: VoltrAllocateToSquads, AmountRaw: uint64(slotExpired.EquityRaw)}
	assertBudgetHold(t, db.authorizeSelectorEntryTxOnManifest(ctx, f.manifest, tx, slotID, allocation, ExpectedEffects{}, 42, true), "selector_entry_quote_expired")
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Wall-clock expired quote: validate still accepts the shape, the fence
	// refuses it.
	wallExpired := autoSelectorEntryFixture(time.Now().UTC().Add(-time.Minute), 3_000_000, &price)
	wallID, _, wallObservation, wallDecision := newOp("wall-expired", &wallExpired, true)
	expectBindHold(wallID, wallObservation, wallDecision, "selector_entry_quote_expired")
	// Explicit pause.
	pausedID, pausedKey, pausedObservation, pausedDecision := newOp("paused", nil, true)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selectorEntryPaused}','true',true) WHERE route_key=$1`, pausedKey); err != nil {
		t.Fatal(err)
	}
	expectBindHold(pausedID, pausedObservation, pausedDecision, "selector_entry_authority_mismatch")
	// Durable unwind intent.
	unwoundID, unwoundKey, unwoundObservation, unwoundDecision := newOp("unwound", nil, true)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selectorUnwind}','{"reason":"economic_rotation"}',true) WHERE route_key=$1`, unwoundKey); err != nil {
		t.Fatal(err)
	}
	expectBindHold(unwoundID, unwoundObservation, unwoundDecision, "selector_entry_authority_mismatch")
	// A durable installed Maple entry is still valid — and its lane mismatches
	// the candidate journal lane, refusing the candidate on the installed
	// ownership check, never on a weakened one.
	maple := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 3_000_000)
	mapleID, _, mapleObservation, mapleDecision := newOp("foreign-lane", &maple, true)
	expectBindHold(mapleID, mapleObservation, mapleDecision, "selector_entry_authority_mismatch")
	// An already allocated initializer cannot reuse the entry.
	allocated := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	allocated.AllocationOperationID = "prior-op"
	allocatedID, _, allocatedObservation, allocatedDecision := newOp("allocated", &allocated, true)
	expectBindHold(allocatedID, allocatedObservation, allocatedDecision, "selector_entry_already_allocated")
}

// The lane authority itself: the embedded manifest admits the candidate AUTO
// entry, every installed lane keeps its prior behavior — Maple still funded,
// deferred installed lanes still deferred — and the public embedded decode
// stays a compile-time closure that no manifest widens.
func TestSelectorEntryManifestLaneAuthorityKeepsInstalledClosure(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	entry := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	if err := entry.validate(); err == nil {
		t.Fatal("embedded entry validation admitted the candidate AUTO lane")
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	// An invalid stored entry decodes to "no entry" (paused), never an
	// authorized one, and never an error that stops the worker.
	if decoded, err := decodeSelectorEntry(encoded); err != nil || decoded != nil {
		t.Fatal("embedded public decode admitted the candidate AUTO entry")
	}
	if err = embeddedTestManifest(t).validateSelectorEntry(entry); err != nil {
		t.Fatalf("installed manifest lane authority refused the candidate AUTO entry: %v", err)
	}
	if !selectorOrAutoLane(autoAUTOPYUSD.Lane) {
		t.Fatal("installed manifest did not resolve the AUTO lane authority")
	}

	candidate := autoInitializerAuthorizationFixture(t).manifest
	if err = candidate.validateSelectorEntry(entry); err != nil {
		t.Fatalf("reviewed binding did not admit the candidate entry: %v", err)
	}
	// Installed behavior is unchanged through the candidate manifest too.
	maple := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 3_000_000)
	if err = maple.validate(); err != nil || candidate.validateSelectorEntry(maple) != nil {
		t.Fatalf("installed Maple entry drifted: %v / %v", maple.validate(), candidate.validateSelectorEntry(maple))
	}
	if !selectorEntryLane(SelectedRouteID) || !selectorEntryFundingLane(SelectedRouteID) {
		t.Fatal("installed Maple rollout scope drifted")
	}
	if selectorEntryFundingLane(PhaseOneLaneID) {
		t.Fatal("deferred installed lane became funded")
	}
	if !selectorEntryFundingLane(autoAUTOPYUSD.Lane) {
		t.Fatal("the AUTO lane is not funded")
	}
	if selectorEntryFundingLane("Ethena/USDe/PYUSD") {
		t.Fatal("unbound foreign lane admitted")
	}
}
