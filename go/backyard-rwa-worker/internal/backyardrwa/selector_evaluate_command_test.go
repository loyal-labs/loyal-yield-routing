package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// stubSelectorEvaluateFeed is the minimal feed seam: a refresh failure plus
// whatever market inventory the scenario needs. An empty inventory after a
// failed refresh is exactly what production hands the evaluation on an outage.
type stubSelectorEvaluateFeed struct {
	refreshErr error
	markets    []LaneEconomics
}

func (f *stubSelectorEvaluateFeed) Refresh(context.Context) error { return f.refreshErr }
func (f *stubSelectorEvaluateFeed) Snapshot() ([]LaneEconomics, string) {
	return f.markets, ""
}
func (f *stubSelectorEvaluateFeed) Close() {}

// canaryFixtureInput is the priced AUTO candidate at the canary equity: the
// same admitted 0.9x price evidence the non-par review proved selectable, with
// the operator request installed. The quote equity equals the request equity,
// as the locked canary admission requires.
func canaryFixtureInput(t *testing.T) SelectorInput {
	t.Helper()
	route, price09, _ := autoDebtPriceFixture(t, 900_000)
	_, price11, _ := autoDebtPriceFixture(t, 1_100_000)
	coll10 := autoCollateralPriceFixture(t, 1_000_000)
	validThrough := int64(42 + budgetMaxObservationLagSlots)
	upper09, _ := nonParBounds(t, &price09, &coll10, route, validThrough)
	upper11, _ := nonParBounds(t, &price11, &coll10, route, validThrough)
	if upper09 >= nonParBorrowRaw || nonParBorrowRaw >= upper11 {
		t.Fatalf("price margins left no discriminator band: %d/%d around %d", upper09, upper11, nonParBorrowRaw)
	}
	equity := nonParBorrowRaw + (upper11-nonParBorrowRaw)/2
	in := nonParReviewFixture(t, &price09, &coll10, equity)
	in.canaryRequest = &pilotCanaryEntryRequest{
		ID: sha256Bytes([]byte("selector-evaluate-command")), Lane: testAutoLane,
		EquityRaw: equity, ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	return in
}

// canaryEvaluateClosure binds the command's evaluate seam to the REAL locked
// persistence on the *Database the command itself opened and leased — using
// the passed argument, never a captured connection, so the lease-fenced write
// path is the command's own. The row version is read first exactly as
// production reads it before collecting quotes. An empty market inventory
// after a failed feed refresh strips the economics, so the evaluation holds
// instead of entering.
func canaryEvaluateClosure(key string, manifest RouteManifest, input SelectorInput) func(context.Context, *Database, []LaneEconomics) (SelectorResult, error) {
	return func(ctx context.Context, db *Database, markets []LaneEconomics) (SelectorResult, error) {
		in := input
		if len(markets) == 0 {
			in.Markets, in.Quotes = nil, nil
		}
		var version int64
		if err := db.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version); err != nil {
			return SelectorResult{}, err
		}
		return db.recordSelectorEvaluationWithLanes(ctx, key, &manifest, in, in.Snapshot.Slot, version)
	}
}

func canaryRouteStateAssertions(t *testing.T, ctx context.Context, db *Database, key string) (entry *SelectorEntry, receipts map[string]pilotCanaryEntryReceipt, version int64) {
	t.Helper()
	var entryRaw, receiptsRaw []byte
	if err := db.pool.QueryRow(ctx, `SELECT state->'selectorEntry', state->'pilotCanaryEntries', state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).
		Scan(&entryRaw, &receiptsRaw, &version); err != nil {
		t.Fatal(err)
	}
	if len(entryRaw) != 0 && string(entryRaw) != "null" {
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			t.Fatal(err)
		}
	}
	receipts = map[string]pilotCanaryEntryReceipt{}
	if len(receiptsRaw) != 0 && string(receiptsRaw) != "null" {
		if err := json.Unmarshal(receiptsRaw, &receipts); err != nil {
			t.Fatal(err)
		}
	}
	return entry, receipts, version
}

func assertNoCanaryOperations(t *testing.T, ctx context.Context, db *Database, key string) {
	t.Helper()
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("evaluation created %d operations", count)
	}
}

func assertCanaryLeaseFree(t *testing.T, ctx context.Context, db *Database, key string) {
	t.Helper()
	if _, err := db.AcquireRouteLease(ctx, key, "canary-probe", time.Minute); err != nil {
		t.Fatal("route lease was not released by the command:", err)
	}
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
}

// canaryJSON extracts the JSON report line: execute prints a human action
// line first and the machine report last.
func canaryJSON(t *testing.T, out *bytes.Buffer) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "{") {
		t.Fatalf("output does not end in a JSON report: %q", out.String())
	}
	return []byte(last)
}

// TestSelectorEvaluateCommandCanaryBehavior drives the exact command core over
// one real candidate route row on the disposable database through every
// behavior root asked for: absent-binding refusal, dry-run no writes, failing
// feed no entry, one exact canary consumed under the command's own lease, and
// a repeat that consumes nothing further. No operation row is ever created and
// no selector core is modified: the execute seam binds the real locked
// persistence, the feed and observation seams are stubs.
func TestSelectorEvaluateCommandCanaryBehavior(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	input := canaryFixtureInput(t)
	// The command reads the operator request from the same environment
	// variable production does; the fixture input carries the identical
	// request so the locked consumption and the receipt identity line up.
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", fmt.Sprintf(`{"id":%q,"lane":%q,"equityRaw":%d,"expiresAt":%q}`,
		input.canaryRequest.ID, input.canaryRequest.Lane, input.canaryRequest.EquityRaw, input.canaryRequest.ExpiresAt.Format(time.RFC3339Nano)))
	// The command's lease and the locked evaluation are both on the fixed
	// production route, so the seeded candidate row carries that key.
	key := productionRouteKey
	resetManualRecoveryProductionRoute(t, ctx, db, "selector-evaluate-cmd-seed")
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	manifest := autoInitializerFixtureManifest(t)
	// The release manifest carries the installed binding; the refusing scope
	// below is the explicit absent fixture (the shipped pre-install state).
	requireEmbeddedInstalledBinding(t)
	// Real candidate DB state: an activated pilot budget carrying the AUTO
	// family. Spent history is preserved; ExitMicros stays zero because a new
	// entry explicitly refuses any outstanding family exit
	// (selector_entry_has_outstanding_exit).
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
	activated.Families["AUTO"] = FamilyBudget{SpentMicros: 7_000_000}
	state, err := json.Marshal(map[string]any{"generation": 2, "phase3": activated, "pilotBudgetActivation": pilotBudgetActivation{authority, mustJSON(t, prior), flat}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	evaluate := canaryEvaluateClosure(key, manifest, input)
	observe := func(ctx context.Context, db *Database, manifest RouteManifest) (Observation, error) {
		return Observation{ObservedAt: time.Now().UTC(), Snapshot: input.Snapshot}, nil
	}
	reviewedDeps := selectorEvaluateDeps{
		manifest: manifest, databaseURL: url,
		newFeed: func(context.Context, RouteManifest) (selectorEvaluateFeed, error) {
			return &stubSelectorEvaluateFeed{markets: input.Markets}, nil
		},
		observe: observe, evaluate: evaluate,
	}

	// Absent binding: the explicit absent fixture (the shipped pre-install
	// state) refuses the candidate request before anything connects and the
	// row is untouched. The embedded manifest's installed binding drives the
	// same locked evaluation through the read-only dry-run below.
	refusingDeps := reviewedDeps
	refusingDeps.manifest = autoAbsentBindingManifest(t)
	if err = runSelectorEvaluate(ctx, io.Discard, true, refusingDeps); err == nil || !strings.Contains(err.Error(), "invalid_pilot_canary_request") {
		t.Fatalf("absent binding did not refuse the candidate request: %v", err)
	}
	installedDeps := reviewedDeps
	installedDeps.manifest = requireEmbeddedInstalledBinding(t)
	if err = runSelectorEvaluate(ctx, io.Discard, false, installedDeps); err != nil {
		t.Fatal("installed manifest failed the read-only canary evaluation:", err)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	if _, _, version := canaryRouteStateAssertions(t, ctx, db, key); version != 2 {
		t.Fatalf("refusal mutated the route row: version %d", version)
	}

	// Dry-run: the read-only diagnostic never takes the lease and never writes.
	var out bytes.Buffer
	if err = runSelectorEvaluate(ctx, &out, false, reviewedDeps); err != nil {
		t.Fatal("dry-run failed:", err)
	}
	var dry struct {
		Mode                string                   `json:"mode"`
		Note                string                   `json:"note"`
		CanaryRequest       *pilotCanaryEntryRequest `json:"canaryRequest"`
		FeedFailure         string                   `json:"feedFailure,omitempty"`
		NextLifecycleAction Decision                 `json:"nextLifecycleAction"`
		Selection           SelectorResult           `json:"selection"`
	}
	if err = json.Unmarshal(out.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if dry.Mode != "no_quote_diagnostic" || !strings.Contains(dry.Note, "not an executable canary preflight") {
		t.Fatalf("dry-run is not labeled a diagnostic: mode=%q note=%q", dry.Mode, dry.Note)
	}
	if dry.CanaryRequest == nil || dry.CanaryRequest.ID != input.canaryRequest.ID {
		t.Fatalf("dry-run lost the canary request report: %+v", dry.CanaryRequest)
	}
	// The shared manifest predicates keep the reviewed candidate visible, and
	// the missing quote collection is exactly why it holds unexecutable: this
	// is the diagnostic's honest shape, not a preflight.
	if c := nonParCandidate(t, dry.Selection); c.Lane != testAutoLane || c.CostsKnown || c.BlockedReason != "bounded_move_cost_unavailable" {
		t.Fatalf("dry-run diagnostic lost the truthful no-quote shape: %+v", c)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); version != 2 || len(receipts) != 0 {
		t.Fatalf("dry-run wrote route state: version %d receipts %d", version, len(receipts))
	}
	assertCanaryLeaseFree(t, ctx, db, key)

	// Failing feed: execute completes without an entry, and the reported feed
	// failure is the sanitized fixed string, never the raw error.
	out.Reset()
	failingDeps := reviewedDeps
	failingDeps.newFeed = func(context.Context, RouteManifest) (selectorEvaluateFeed, error) {
		return &stubSelectorEvaluateFeed{refreshErr: errors.New("pq: DSN password hunter2@/private/creds")}, nil
	}
	if err = runSelectorEvaluate(ctx, &out, true, failingDeps); err != nil {
		t.Fatal("failing feed execute failed:", err)
	}
	var failed struct {
		Mode        string         `json:"mode"`
		FeedFailure string         `json:"feedFailure,omitempty"`
		Result      SelectorResult `json:"result"`
	}
	if err = json.Unmarshal(canaryJSON(t, &out), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.FeedFailure != "selector_economic_feed_refresh_unavailable" || strings.Contains(out.String(), "hunter2") {
		t.Fatalf("raw feed error leaked: %q", failed.FeedFailure)
	}
	if failed.Result.Action == "CANARY_ENTER" || failed.Result.Action == "ENTER" {
		t.Fatalf("failing feed still entered: %+v", failed.Result)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	if entry, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); entry != nil || len(receipts) != 0 || version != 2 {
		t.Fatalf("failing feed wrote an entry: entry=%+v receipts=%d version=%d", entry, len(receipts), version)
	}
	assertCanaryLeaseFree(t, ctx, db, key)

	// Execute: the one exact canary is consumed and the entry is committed
	// under the command's own lease, which is released.
	out.Reset()
	if err = runSelectorEvaluate(ctx, &out, true, reviewedDeps); err != nil {
		t.Fatal("execute failed:", err)
	}
	var done struct {
		Mode   string         `json:"mode"`
		Result SelectorResult `json:"result"`
	}
	if err = json.Unmarshal(canaryJSON(t, &out), &done); err != nil {
		t.Fatal(err)
	}
	if done.Result.Action != "CANARY_ENTER" || done.Result.EquityRaw != input.canaryRequest.EquityRaw {
		t.Fatalf("execute did not consume the canary: %+v", done.Result)
	}
	entry, receipts, version := canaryRouteStateAssertions(t, ctx, db, key)
	if entry == nil || entry.EquityRaw != input.canaryRequest.EquityRaw {
		t.Fatalf("committed entry missing or wrong equity: %+v", entry)
	}
	if len(receipts) != 1 || receipts[input.canaryRequest.ID].Request.EquityRaw != input.canaryRequest.EquityRaw {
		t.Fatalf("canary receipt not recorded exactly once: %+v", receipts)
	}
	if version != 3 {
		t.Fatalf("entry commit did not bump the version once: %d", version)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	assertCanaryLeaseFree(t, ctx, db, key)

	// Repeat: the same request is already consumed — no second entry, no
	// second receipt, no further version bump.
	out.Reset()
	if err = runSelectorEvaluate(ctx, &out, true, reviewedDeps); err != nil {
		t.Fatal("repeat execute failed:", err)
	}
	if err = json.Unmarshal(canaryJSON(t, &out), &done); err != nil {
		t.Fatal(err)
	}
	if done.Result.Action != "KEEP" || done.Result.Reason != "operator_canary_already_consumed" {
		t.Fatalf("repeat did not report the consumed request: %+v", done.Result)
	}
	entry, receipts, version = canaryRouteStateAssertions(t, ctx, db, key)
	if entry == nil || entry.EquityRaw != input.canaryRequest.EquityRaw || len(receipts) != 1 || version != 3 {
		t.Fatalf("repeat duplicated state: entry=%+v receipts=%d version=%d", entry, len(receipts), version)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	assertCanaryLeaseFree(t, ctx, db, key)
}
