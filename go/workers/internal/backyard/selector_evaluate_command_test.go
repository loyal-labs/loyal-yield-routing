package backyard

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

// canaryFixtureInput is the priced AUTO candidate at the canary equity, with
// the operator request installed. The quote equity equals the request
// equity, as the locked canary admission requires.
func canaryFixtureInput(t *testing.T) SelectorInput {
	t.Helper()
	in := fundedAutoFixture(t)
	equity := in.Quotes[0].EquityRaw
	armFeeAuthorityFixture(t, &in.Snapshot)
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
		return db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot)
	}
}

// canaryRouteStateAssertions reads back the selector's entering quote (nil
// unless the latest result enters), the canary receipts and the version.
func canaryRouteStateAssertions(t *testing.T, ctx context.Context, db *Database, key string) (entry *MoveQuote, receipts map[string]pilotCanaryEntryReceipt, version int64) {
	t.Helper()
	var resultRaw, receiptsRaw []byte
	if err := db.pool.QueryRow(ctx, `SELECT state->'selector'->'result', state->'pilotCanaryEntries', state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).
		Scan(&resultRaw, &receiptsRaw, &version); err != nil {
		t.Fatal(err)
	}
	if len(resultRaw) != 0 && string(resultRaw) != "null" {
		var result SelectorResult
		if err := json.Unmarshal(resultRaw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Action == "ENTER" || result.Action == "CANARY_ENTER" {
			entry = result.SelectedQuote
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

// assertWriterLockFree proves the command released Backyard's writer lock.
func assertWriterLockFree(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var free bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('loyal-family:backyard',0))`).Scan(&free); err != nil || !free {
		t.Fatalf("the command left Backyard's writer lock held: %v", err)
	}
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('loyal-family:backyard',0))`); err != nil {
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
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	manifest := embeddedTestManifest(t)
	// The release manifest carries the installed binding; the refusing scope
	// below is the explicit absent fixture (the shipped pre-install state).
	embeddedTestManifest(t)
	// Real candidate DB state.
	state, err := json.Marshal(map[string]any{"generation": 2})
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

	// The embedded manifest drives the same locked evaluation through the
	// read-only dry-run below.
	installedDeps := reviewedDeps
	installedDeps.manifest = embeddedTestManifest(t)
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
	if c := laneCandidate(t, dry.Selection, testAutoLane); c.Lane != testAutoLane || c.CostsKnown || c.BlockedReason != "bounded_move_cost_unavailable" {
		t.Fatalf("dry-run diagnostic lost the truthful no-quote shape: %+v", c)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); version != 2 || len(receipts) != 0 {
		t.Fatalf("dry-run wrote route state: version %d receipts %d", version, len(receipts))
	}
	assertWriterLockFree(t, ctx, db)

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
	assertWriterLockFree(t, ctx, db)

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
	// The entry choice is an input the next tick reads: no version step.
	if version != 2 {
		t.Fatalf("the entry choice moved the route state version: %d", version)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	assertWriterLockFree(t, ctx, db)

	// Repeat while the vault is still flat: the same request keeps its lane
	// with a fresh quote — one receipt.
	out.Reset()
	if err = runSelectorEvaluate(ctx, &out, true, reviewedDeps); err != nil {
		t.Fatal("repeat execute failed:", err)
	}
	if err = json.Unmarshal(canaryJSON(t, &out), &done); err != nil {
		t.Fatal(err)
	}
	if done.Result.Action != "CANARY_ENTER" || done.Result.DestinationLane != input.canaryRequest.Lane {
		t.Fatalf("repeat did not keep the request's lane: %+v", done.Result)
	}
	entry, receipts, version = canaryRouteStateAssertions(t, ctx, db, key)
	if entry == nil || entry.EquityRaw != input.canaryRequest.EquityRaw || len(receipts) != 1 || version != 2 {
		t.Fatalf("repeat duplicated state: entry=%+v receipts=%d version=%d", entry, len(receipts), version)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	assertWriterLockFree(t, ctx, db)
}

// laneCandidate is result's candidate for lane.
func laneCandidate(t *testing.T, result SelectorResult, lane string) *CandidateForecast {
	t.Helper()
	for i := range result.Candidates {
		if result.Candidates[i].Lane == lane {
			return &result.Candidates[i]
		}
	}
	t.Fatal("candidate missing from", result.Candidates)
	return nil
}
