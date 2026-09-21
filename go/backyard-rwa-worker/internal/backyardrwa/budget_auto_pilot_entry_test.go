package backyardrwa

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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

// seedAutoInitializerPilotOperation seeds one decided candidate initializer on
// a test-owned route key under a real activated pilot budget (Pilot != nil),
// with a persisted candidate selector entry and a measured AUTO reservation.
// The legacy reservation boundary still refuses pilot rows and the measured
// admission producer remains a separately-gated seam, so admission runs the
// exact installed reducer (identical caps, authority identity and cost
// bounds) under the held route lease — the same isolation
// testInitializationDatabaseSettlement established.
func seedAutoInitializerPilotOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string, measured ValuedTransactionCost, price *BudgetPrice, entry *SelectorEntry, storeEntry bool) string {
	t.Helper()
	id := key + "-op"
	prior := emptyTestBudget()
	previous, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	flat := pilotFlatFixture(t)
	flatJSON, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	authority := pilotTestAuthority(prior)
	authority.Generation = 2
	authority.FinalizedSlot = flat.Slot
	authority.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	activated, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{"generation": 2, "phase3": activated, "pilotBudgetActivation": pilotBudgetActivation{authority, previous, flat}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "auto-pilot-entry-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(InitializeKaminoObligation), f.request.RouteLane, f.raw); err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(f.request, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.ReservePhase3(ctx, BudgetReservation{OperationID: id, Family: "AUTO", IntentSHA256: digest, UpperMicros: measured.TotalMicros}), "pilot_requires_measured_execution_admission")
	reservation := BudgetReservation{OperationID: id, Family: "AUTO", IntentSHA256: digest,
		UpperMicros: measured.TotalMicros, ExecutionCostUpperMicros: measured.NetworkFeeMicros}
	budget := activated
	if err = budget.Admit(reservation); err != nil {
		t.Fatal(err)
	}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PilotAuthorityID: pilotBudgetAuthorityID}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = db.lockOperationLease(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err = db.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		fresh := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, price)
		entry = &fresh
	}
	if storeEntry {
		storeTestSelectorEntry(t, ctx, db, key, *entry)
	}
	return id
}

// The candidate AUTO initializer passes the real locked build authorization
// and the actual final-send fence under a real Pilot != nil budget with a
// persisted candidate selector entry: entry admission resolves the reviewed
// binding inside the explicit manifest, reservation accounting keeps the
// exact measured bounds and pilot authority identity, broadcast intent lands
// durably before the single refused broadcast, and every installed public
// path — embedded build gate, embedded final-send entrypoint, embedded entry
// validation — still refuses the same durable rows.
func TestAutoInitializerPilotEntryAuthorizesLockedBuildAndSend(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	t.Cleanup(func() { db.Close() })
	var routeKeys []string
	relaxInitializerScopeForSyntheticTest(t, ctx, db, func() []string { return routeKeys })
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	measured, err := f.manifest.observePhase3KnownBuildCost(ctx, rpc, f.request, f.effects)
	if err != nil {
		t.Fatal(err)
	}
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)

	newPilotOp := func(name string, entry *SelectorEntry, storeEntry bool) (string, string) {
		t.Helper()
		key := "auto-pilot-entry-" + name + "-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		id := seedAutoInitializerPilotOperation(t, ctx, db, f, key, measured, &price, entry, storeEntry)
		return id, key
	}
	assertStillDecided := func(id string) {
		t.Helper()
		var status string
		if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "decided" {
			t.Fatalf("refusal transitioned the journal: %s", status)
		}
		if auth := loadAutoInitializerAuth(t, ctx, db, id); auth.BuildInput != nil {
			t.Fatal("refused candidate persisted a build input")
		}
	}
	expectBuildHold := func(id string, reason string) {
		t.Helper()
		assertBudgetHold(t, f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw), reason)
		assertStillDecided(id)
	}

	// Positive candidate: measured reservation, admitted entry, locked build
	// authorization through the reviewed binding.
	id, opKey := newPilotOp("positive", nil, true)
	if err = f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw); err != nil {
		t.Fatalf("candidate pilot build authorization refused: %v", err)
	}
	var status string
	if err = db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "decided" {
		t.Fatalf("build authorization transitioned the journal: %s", status)
	}
	digest, err := Phase3IntentDigest(f.request, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	if auth.GoalID != Phase3GoalID || auth.IntentSHA256 != digest || auth.BuildInput == nil || auth.SignedWireSHA256 != "" || auth.PilotAuthorityID != pilotBudgetAuthorityID {
		t.Fatalf("pilot build authorization drift: goal=%s intent=%s buildInput=%v pilot=%q", auth.GoalID, auth.IntentSHA256, auth.BuildInput != nil, auth.PilotAuthorityID)
	}
	var encodedBudget []byte
	if err = db.pool.QueryRow(ctx, `SELECT s.state->'phase3' FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE o.operation_id=$1`, id).Scan(&encodedBudget); err != nil {
		t.Fatal(err)
	}
	var budget Phase3Budget
	if json.Unmarshal(encodedBudget, &budget) != nil {
		t.Fatal("budget decode")
	}
	reservation := budget.Reservations[id]
	if budget.Pilot == nil || reservation.Family != "AUTO" || reservation.UpperMicros != measured.TotalMicros ||
		reservation.ExecutionCostUpperMicros != measured.NetworkFeeMicros || reservation.ExitBeforeMicros != 0 {
		t.Fatalf("pilot reservation lost its measured bounds: %+v", reservation)
	}
	// The initializer fence runs with admission=false: the entry stays
	// unbound, exactly like the installed initializer authority.
	var allocationID string
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(s.state->'selectorEntry'->>'allocationOperationId','') FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE o.operation_id=$1`, id).Scan(&allocationID); err != nil {
		t.Fatal(err)
	}
	if allocationID != "" {
		t.Fatalf("initializer build authorization bound the entry allocation: %q", allocationID)
	}

	// The embedded public build gate keeps the candidate closed on the same
	// durable rows: the installed executable-debit identity check refuses the
	// AUTO request before the binding comparison runs (the same order the
	// drift case proves), and the journal never transitions. The admitted
	// build input from the manifest authorization above is retained.
	assertBudgetHold(t, authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw), "initializer_effects_request_mismatch")
	if status, _, _ := autoRecoverySettlementState(t, ctx, db, id); status != "decided" {
		t.Fatalf("public gate refusal transitioned the journal: %s", status)
	}

	// The actual Signed transition: the locked final-send fence revalues the
	// persisted wire through the same reviewed manifest, records broadcast
	// intent durably, and the transport refuses the single broadcast attempt.
	hash := sha256Bytes(f.wire)
	auth.SignedWireSHA256 = hash
	encodedAuth, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3,expected_effects=jsonb_set(expected_effects,'{phase3}',$4) WHERE operation_id=$1`,
		id, f.wire, hash, encodedAuth); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, RouteKey: opKey, StrategyKey: f.request.RouteLane,
		Decision: Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}},
		Status: Signed, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: hash,
		TransactionSignature: encodeBase58(f.wire[1:65]), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
	if err = db.RevalueAndMarkBroadcastIntent(ctx, rpc, op); err == nil {
		t.Fatal("embedded public final-send entrypoint admitted the AUTO pilot candidate")
	}
	if got, _, _ := autoRecoverySettlementState(t, ctx, db, id); got != "signed" {
		t.Fatalf("public send refusal transitioned the journal: %s", got)
	}
	if err = advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err == nil || !strings.Contains(err.Error(), "ambiguous send after durable broadcast intent") {
		t.Fatalf("expected the ambiguous-send fence, got %v", err)
	}
	got, reservations, _ := autoRecoverySettlementState(t, ctx, db, id)
	if got != "broadcast_intent" {
		t.Fatalf("durable broadcast intent missing: %s", got)
	}
	if reservations != 1 {
		t.Fatalf("broadcast intent released the reservation: %d", reservations)
	}
	sentAuth := loadAutoInitializerAuth(t, ctx, db, id)
	if sentAuth.SendKnownCost == nil || sentAuth.SendKnownCost.TotalMicros <= 0 {
		t.Fatalf("final-send fence did not persist its known cost: %+v", sentAuth.SendKnownCost)
	}
	if *sends != 1 {
		t.Fatalf("broadcast attempted %d times, exactly-once fence lost", *sends)
	}
	if err = db.RevalueAndMarkBroadcastIntentOnManifest(ctx, f.manifest, rpc, op); err == nil {
		t.Fatal("completed pilot send authorization replayed")
	}
	if *sends != 1 {
		t.Fatalf("replay attempted another broadcast: %d", *sends)
	}

	// Missing durable entry.
	missingID, _ := newPilotOp("missing-entry", nil, false)
	expectBuildHold(missingID, "selector_entry_authority_mismatch")
	// Slot-expired quote: the wall clock must not revive the window. The debt
	// price stays coherent with the earlier sample — observed at slot 30,
	// valued through the quote's own end at 41 — so the entry passes the exact
	// shape checks and the fence refuses it on slot currentness alone.
	slotExpired := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	slotExpired.Quote.SampleSlot, slotExpired.Quote.ValidThroughSlot = 30, 41
	earlyPrice := price
	earlyPrice.ObservedSlot, earlyPrice.ValidThroughSlot = 30, 30+budgetMaxObservationLagSlots
	slotExpired.Quote.DebtPrice = &earlyPrice
	slotID, _ := newPilotOp("slot-expired", &slotExpired, true)
	expectBuildHold(slotID, "selector_entry_quote_expired")
	// Wall-clock expired quote: validate still accepts the shape, the fence
	// refuses allocation.
	wallExpired := autoSelectorEntryFixture(time.Now().UTC().Add(-time.Minute), 3_000_000, &price)
	wallID, _ := newPilotOp("wall-expired", &wallExpired, true)
	expectBuildHold(wallID, "selector_entry_quote_expired")
	// Explicit pause.
	pausedID, _ := newPilotOp("paused", nil, true)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states s SET state=jsonb_set(s.state,'{selectorEntryPaused}','true',true) FROM loyal_yield.multiply_operations o WHERE o.operation_id=$1 AND s.route_key=o.route_key`, pausedID); err != nil {
		t.Fatal(err)
	}
	expectBuildHold(pausedID, "selector_entry_authority_mismatch")
	// Durable unwind intent.
	unwoundID, _ := newPilotOp("unwound", nil, true)
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states s SET state=jsonb_set(s.state,'{selectorUnwind}','{"reason":"economic_rotation"}',true) FROM loyal_yield.multiply_operations o WHERE o.operation_id=$1 AND s.route_key=o.route_key`, unwoundID); err != nil {
		t.Fatal(err)
	}
	expectBuildHold(unwoundID, "selector_entry_authority_mismatch")
	// A durable installed Maple entry is still valid — and its lane mismatches
	// the candidate journal lane, refusing the candidate on the installed
	// ownership check, never on a weakened one.
	maple := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 3_000_000)
	mapleID, _ := newPilotOp("foreign-lane", &maple, true)
	expectBuildHold(mapleID, "selector_entry_authority_mismatch")
	// An already allocated initializer cannot reuse the entry.
	allocated := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	allocated.AllocationOperationID = "prior-op"
	allocatedID, _ := newPilotOp("allocated", &allocated, true)
	expectBuildHold(allocatedID, "selector_entry_already_allocated")
	// Drifted request identity is refused by the executable-debit identity
	// check before the binding comparison, with the first candidate's journal
	// row untouched.
	drifted := f.request
	drifted.PolicySeed = autoFixtureSeed
	assertBudgetHold(t, f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, drifted, f.effects, f.raw), "initializer_effects_request_mismatch")
	if auth := loadAutoInitializerAuth(t, ctx, db, id); auth.BuildInput == nil {
		t.Fatal("drifted request erased the admitted build input")
	}
}

// The lane authority itself, named for both binding states: the explicit
// absent-binding fixture (the shipped pre-install state) keeps AUTO closed at
// the same seam, the embedded manifest carries the installed binding and
// admits the candidate AUTO entry through it, and every installed lane keeps
// its prior behavior — Maple still funded, deferred installed lanes still
// deferred. Doc30's rollout scope funds the candidate AUTO lane for ordinary
// allocation through a manifest's complete reviewed binding (initializer=true
// additionally requires the reviewed initialize constraint); an absent
// binding keeps both paths closed, and the public embedded decode stays a
// compile-time closure that no manifest widens.
func TestSelectorEntryManifestLaneAuthorityKeepsInstalledClosure(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	entry := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	absent := autoAbsentBindingManifest(t)
	requireEmbeddedInstalledBinding(t)
	if err := entry.validate(); err == nil {
		t.Fatal("embedded entry validation admitted the candidate AUTO lane")
	}
	if err := absent.validateSelectorEntry(entry); err == nil {
		t.Fatal("absent binding admitted the candidate AUTO entry")
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeSelectorEntry(encoded); err == nil {
		t.Fatal("embedded public decode admitted the candidate AUTO entry")
	}
	if absent.selectorEntryLaneAllowed(autoAUTOPYUSD.Lane) {
		t.Fatal("absent binding resolved the AUTO lane authority")
	}
	// The installed state: the embedded manifest's binding admits the same
	// entry the reviewed initializer manifest admits, at the identical seam.
	if err = requireEmbeddedInstalledBinding(t).validateSelectorEntry(entry); err != nil {
		t.Fatalf("installed manifest lane authority refused the candidate AUTO entry: %v", err)
	}
	if !requireEmbeddedInstalledBinding(t).selectorEntryLaneAllowed(autoAUTOPYUSD.Lane) {
		t.Fatal("installed manifest did not resolve the AUTO lane authority")
	}
	if err = absent.validateSelectorEntry(selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 3_000_000)); err != nil {
		t.Fatalf("installed Maple entry refused by the absent authority: %v", err)
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
	if !selectorEntryLane(SelectedRouteID) || !candidate.selectorEntryFundingLane(SelectedRouteID, true) {
		t.Fatal("installed Maple rollout scope drifted")
	}
	if candidate.selectorEntryFundingLane(PhaseOneLaneID, true) {
		t.Fatal("deferred installed lane became funded through the candidate manifest")
	}
	if !candidate.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) {
		t.Fatal("reviewed binding did not admit funded AUTO allocation")
	}
	if absent.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) || absent.selectorEntryFundingLane(autoAUTOPYUSD.Lane, true) {
		t.Fatal("bindingless manifest admitted AUTO funding")
	}
	// The installed state funds the AUTO lane through the embedded manifest's
	// complete binding — the funding gate the absent fixture keeps closed.
	installed := requireEmbeddedInstalledBinding(t)
	if !installed.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) || !installed.selectorEntryFundingLane(autoAUTOPYUSD.Lane, true) {
		t.Fatal("installed manifest did not admit funded AUTO allocation")
	}
	if candidate.selectorEntryFundingLane("Ethena/USDe/PYUSD", true) {
		t.Fatal("unbound foreign lane admitted")
	}
}

// Recognizing AUTO as a pilot accounting family changes nothing about the
// caps, the entry execution-cost bound or the legacy (non-pilot) closure:
// every reservation is still bounded by the same limits, and the legacy
// budget still refuses an execution-cost bound outright.
func TestPilotAdmissionRecognizesAutoFamilyWithoutActivation(t *testing.T) {
	prior := emptyTestBudget()
	flat := pilotFlatFixture(t)
	flatJSON, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	authority := pilotTestAuthority(prior)
	authority.Generation = 2
	authority.FinalizedSlot = flat.Slot
	authority.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	activated, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	reservation := func(upper, executionCost int64) BudgetReservation {
		return BudgetReservation{OperationID: "auto-entry", Family: "AUTO", IntentSHA256: strings.Repeat("a", 64), UpperMicros: upper, ExecutionCostUpperMicros: executionCost}
	}
	assertBudgetHold(t, activated.Admit(reservation(500_000_000_000_000, 1)), "transaction_cap_exceeded")
	assertBudgetHold(t, activated.Admit(reservation(900_000, 0)), "missing_or_invalid_execution_cost_bound")
	assertBudgetHold(t, activated.Admit(reservation(900_000, 900_001)), "missing_or_invalid_execution_cost_bound")
	assertBudgetHold(t, activated.Admit(reservation(PilotEntryExecutionCostCapMicros+1, PilotEntryExecutionCostCapMicros+1)), "pilot_entry_execution_cost_cap_exhausted")
	admitted := activated
	if err = admitted.Admit(reservation(900_000, 900_000)); err != nil {
		t.Fatalf("measured AUTO pilot reservation refused: %v", err)
	}
	stored := admitted.Reservations["auto-entry"]
	if stored.ExitBeforeMicros != 0 || stored.ExitAfterMicros != 0 || admitted.Families["AUTO"].ExitMicros != 0 {
		t.Fatalf("AUTO entry consumed an exit reserve: %+v", stored)
	}
	// The legacy budget keeps its exact installed closure.
	legacy := emptyTestBudget()
	assertBudgetHold(t, legacy.Admit(reservation(900_000, 900_000)), "execution_cost_requires_pilot_authority")
}
