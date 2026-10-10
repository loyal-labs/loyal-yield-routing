package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Behavioral tests for the admission-phase helper (auto_custody_admission.go)
// and the validated current-operation exclusion. The signed fixture is a REAL
// wire produced by the production builder with a deterministic local delegate
// key (the repo-standard offline pattern — no live signing material): the
// fixture config pins THAT delegate, so the production delegate-binding path
// runs end to end, and a wire from any other signer is refused by the pin.
//
// SCOPE OF THE SIGNED FIXTURE (narrower than production, stated exactly):
// the repo holds no production delegate signing material, so the production
// PersistSigned function — which pins mustKey(bridgeDelegate) internally and
// cannot run offline — is NOT exercised. The signed persistence here runs the
// PRODUCTION PersistSignedUpdate SQL (the exact statement production calls)
// and the PRODUCTION BuildResult.validateForDelegate through the cfg.Delegate
// pin, i.e. the same validation path with different key material. Full
// delegate-pin coverage against the real production key is production-only.

// custodyAdmissionSignedBuild produces one self-consistent signed BuildResult
// exactly as the production signing persistence would persist it, signed by a
// deterministic local delegate. BuildResult.Validate() enforces every binding
// (wire digest, message digest, blockhash, sole-signer signature recovery).
func custodyAdmissionSignedBuild(t *testing.T, seed []byte) (BuildResult, publicKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(seed)
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignBridgeTransactionForDelegate(bridgeTestRequest(ReportNAV, 0), key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	build := BuildResult{
		MessageSHA256: signed.messageSHA256, SignedWire: signed.signedWire, SignedWireSHA256: signed.signedWireSHA256,
		TransactionSignature: signed.transactionSignature, RecentBlockhash: signed.recentBlockhash,
		LastValidBlockHeight: signed.lastValidBlockHeight, SimulationSlot: 500,
	}
	if build.Validate() != nil {
		t.Fatal("signed fixture wire is not self-consistent")
	}
	return build, delegate
}

func TestSharedCustodySpendRawGatesExactDebit(t *testing.T) {
	cfg := custodyAttributionConfig()
	// A partial repay debits the shared custody by the actual move.
	if spend := sharedCustodySpendRaw(custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000), cfg); spend != 2_500_000_000 {
		t.Fatalf("repay spend = %d", spend)
	}
	// A funding swap CREDITS the custody: touch, but no spend — NAV-style
	// recovery is never blocked by a positive balance alone.
	if spend := sharedCustodySpendRaw(custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil), cfg); spend != 0 {
		t.Fatalf("funding spend = %d", spend)
	}
	// The borrow edge credits the custody too.
	if spend := sharedCustodySpendRaw(custodyAttributionBorrowExpected(), cfg); spend != 0 {
		t.Fatalf("borrow spend = %d", spend)
	}
	// Effects with no account at the pinned custody identity at all.
	nav := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts: []ExpectedAccountEffect{{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram,
			Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 5, AfterRaw: 5}}}
	if spend := sharedCustodySpendRaw(nav, cfg); spend != 0 {
		t.Fatalf("non-custody spend = %d", spend)
	}
	// The custody ADDRESS under a DIFFERENT authority is not the pinned
	// identity: it neither proves nor spends the shared custody.
	if spend := sharedCustodySpendRaw(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true,
		Accounts: []ExpectedAccountEffect{{Address: cfg.Custody, Owner: cfg.Owner, Mint: cfg.Mint,
			Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 9, AfterRaw: 1}}}, cfg); spend != 0 {
		t.Fatalf("identity-drifted spend = %d", spend)
	}
}

// The spend intent must be written against the custody prestate the fresh
// observation actually measured: BeforeRaw == observedRaw, one pinned
// custody account, and a spend that never exceeds that balance.
func TestSharedCustodySpendIntentBindsObservedPrestate(t *testing.T) {
	cfg := custodyAttributionConfig()
	// Coherent: the journal tip and the observation both say 3.1B.
	if err := validateSharedCustodySpendIntent(custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000), 3_100_000_000, cfg); err != nil {
		t.Fatalf("coherent intent refused: %v", err)
	}
	invalid := func(name string, expected ExpectedEffects, observedRaw uint64) {
		t.Helper()
		err := validateSharedCustodySpendIntent(expected, observedRaw, cfg)
		if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_intent_prestate_mismatch" {
			t.Fatalf("%s: %s", name, reason)
		}
	}
	// Stale prestate: 8.1B where the custody holds 3.1B.
	invalid("stale prestate", custodyAttributionRepayExpected(8_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000), 3_100_000_000)
	// Spend beyond the observed balance.
	invalid("beyond balance", custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000), 2_000_000_000)
	// No pinned custody account at all (a collateral-custody-only intent is
	// not a shared-custody spend intent). A credit-only funding intent is
	// NOT this case: it names the custody at BeforeRaw 0, matching an
	// observed 0, and is coherent.
	invalid("no custody account", ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts: []ExpectedAccountEffect{{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram,
			Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 5, AfterRaw: 1}}}, 5)
}

func TestSharedCustodyAdmissionProofBindsGeneration(t *testing.T) {
	proof := sharedCustodyAdmissionProof{SpendRaw: 1, Generation: 4, LeaseFencing: 9}
	if !proof.BindsGeneration(4, 9) {
		t.Fatal("matching generation and fence refused")
	}
	for _, tc := range []sharedCustodyAdmissionProof{
		{SpendRaw: 0, Generation: 4, LeaseFencing: 9},  // nothing was probed
		{SpendRaw: 1, Generation: 5, LeaseFencing: 9},  // generation drifted
		{SpendRaw: 1, Generation: 4, LeaseFencing: 10}, // fence drifted
		{SpendRaw: 1, Generation: 4, LeaseFencing: 0},  // unfenced
	} {
		if tc.BindsGeneration(4, 9) {
			t.Fatalf("drifted proof accepted: %+v", tc)
		}
	}
}

// The ownership proof in the production Tick phase order: REAL journal rows,
// the REAL decision persistence (RecordDecisionOnManifest — which stores ONLY
// decision evidence, expectedEffects null) and the real candidate-AUTO
// planning state: a persisted candidate selector entry that only the explicit
// reviewed manifest decodes. The strict proof binds BEFORE the row exists and
// holds while it is in flight.
func TestSharedCustodyAdmissionSpendProofLifecycle(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	// Exactly the two route keys this test creates — never a prefix wildcard,
	// which could touch another concurrent run's rows. The pool is closed
	// INSIDE the cleanup callback (registered before the row cleanup), since
	// a deferred Close would run before t.Cleanup and leave the cleanup SQL
	// talking to a closed pool.
	key := fmt.Sprintf("auto-admission-%d", time.Now().UnixNano())
	navKey := key + "-nav"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range []string{key, navKey} {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Route state with a REAL persisted candidate AUTO selector entry — the
	// PYUSD-debt lane requires the observed bounded debt price evidence, so
	// the shared autoSelectorEntryFixture carries it. Only the explicit
	// candidate manifest decodes it (the embedded manifest's entry decode
	// rejects the candidate lane), so this test fails if the helper ever goes
	// back to the embedded-manifest planning read.
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	entry := autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price)
	state, err := json.Marshal(map[string]any{"generation": 1, "selectorEntry": entry})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,1)`, key, state)
	lease, err := db.AcquireRouteLease(ctx, key, "admission-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manifest := embeddedTestManifest(t)
	decoded, err := db.readRoutePlanningStateOnManifest(ctx, embeddedTestManifest(t), key, true)
	if err != nil {
		t.Fatalf("embedded planning read refused the persisted candidate entry: %v", err)
	}
	// Identity at the seam this guard owns: the candidate lane entry decoded
	// back with its observation and quoted equity (full wire round-trips have
	// their own dedicated tests).
	if decoded.entry == nil || decoded.entry.Lane != entry.Lane || decoded.entry.ObservationID != entry.ObservationID || decoded.entry.EquityRaw != entry.EquityRaw {
		t.Fatalf("embedded planning read lost the persisted candidate entry: %+v", decoded.entry)
	}

	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key)
	_, delegate := custodyAdmissionSignedBuild(t, bytes.Repeat([]byte{11}, ed25519.SeedSize))
	cfg.Delegate = delegate

	insertRow := func(row custodyAttributionRow) {
		t.Helper()
		exec(`INSERT INTO loyal_yield.multiply_operations
			(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects)
			VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11::jsonb)`,
			row.OperationID, row.RouteKey, row.Status, row.Action, row.StrategyKey, row.ExpectedEffects,
			row.TransactionSignature, row.ConfirmedSlot, row.ConfirmationStatus, row.ReconciliationSHA256, row.ReconciledEffects)
	}
	funding := custodyAttributionFundingRow(t, key+"-fund", "sig-admission-fund", 100)
	repay := custodyAttributionRepayRow(t, key+"-repay", "sig-admission-repay", 200)
	funding.RouteKey, repay.RouteKey, funding.StrategyKey, repay.StrategyKey = key, key, cfg.Lane, cfg.Lane
	insertRow(repay)
	insertRow(funding)

	// (a) PRE-DECISION ownership proof — the production Tick state after
	// prepare* built the evidence and BEFORE RecordDecision: no operation
	// row exists, so the strict gates are exactly right, and the proof
	// carries the generation/fence the locked admission persistence (A's
	// persistPhase3ExitAdmissionOnManifest) must validate. The observed
	// balance is the CURRENT custody residue — the journal tip's 3.1B —
	// because nothing has executed yet; the 0.6B in the effects is the
	// INTENDED residue, never the observation.
	cleanup := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	proof, err := db.ObserveSharedCustodyOwnershipProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300)
	if err != nil {
		t.Fatalf("pre-decision ownership proof refused: %v", err)
	}
	if proof.SpendRaw != 2_500_000_000 || proof.Generation != 1 ||
		proof.LeaseFencing != lease.FencingToken || proof.Digest == "" || len(proof.Proof.Steps) != 2 ||
		proof.Proof.Origin.Signature != "sig-admission-fund" {
		t.Fatalf("unexpected pre-decision proof: %+v", proof)
	}
	if !proof.BindsGeneration(1, lease.FencingToken) || proof.BindsGeneration(2, lease.FencingToken) {
		t.Fatal("generation binding semantics broken")
	}
	// The prepared intent must be written against the observed prestate.
	staleIntent := custodyAttributionRepayExpected(8_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	if _, err = db.ObserveSharedCustodyOwnershipProof(ctx, manifest, cfg, staleIntent, 3_100_000_000, 300); custodyAttributionHoldReason(t, err) != "custody_attribution_intent_prestate_mismatch" {
		t.Fatalf("stale-intent prestate accepted: %v", err)
	}

	// (a2) REAL decision recording — the exact production persistence, not a
	// fabricated row. The decided row stores ONLY the decision evidence with
	// expectedEffects explicitly null: this is the production state
	// DecodeExpectedEffects refuses, which is precisely why no exclusion can
	// bind built effects before MarkBuilt.
	obs := initializationPlanningFixture(autoAUTOPYUSD.Lane)
	decision := Decision{Action: DeleverRouteStep, Reason: "shared-custody-attribution-lifecycle",
		AmountRaw: 2_500_000_000, IdempotencyKey: key + "-cleanup", StrategyKey: cfg.Lane}
	record, err := db.RecordDecisionOnManifest(ctx, manifest, key, obs, decision, manifest.SHA256)
	if err != nil {
		t.Fatalf("real RecordDecisionOnManifest failed: %v", err)
	}
	if record.Status != "decided" || record.OperationID == "" {
		t.Fatalf("unexpected decision record: %+v", record)
	}
	id := record.OperationID
	var decidedEffects []byte
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&decidedEffects); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeExpectedEffects(decidedEffects); err == nil {
		t.Fatal("the real decided row unexpectedly decodes as built effects")
	}
	// Mid-flight, the strict ownership proof holds on the decided row: it is
	// an unresolved operation with NO exclusion at this phase.
	if _, err = db.ObserveSharedCustodyOwnershipProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300); custodyAttributionHoldReason(t, err) != "custody_attribution_unresolved_operation" {
		t.Fatalf("mid-flight strict proof did not hold: %v", err)
	}
	// (i) Zero spend skips the gate entirely: an unrelated unresolved row on
	// the route and a positive balance never block a credit-only operation.
	exec(`INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, navKey)
	if _, err = db.AcquireRouteLease(ctx, navKey, "admission-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
		VALUES($1,$2,'built','DECIDE',$3,'{}')`, navKey+"-unrelated", navKey, cfg.Lane)
	navCfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, navKey)
	navCfg.Delegate = delegate
	navProof, err := db.ObserveSharedCustodyOwnershipProof(ctx, manifest, navCfg, custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil), 8_100_000_000, 500)
	if err != nil {
		t.Fatalf("credit-only operation blocked: %v", err)
	}
	if navProof.SpendRaw != 0 || navProof.Digest != "" {
		t.Fatalf("skip must be an empty proof, not a silent pass: %+v", navProof)
	}
	// The same route with a POSITIVE custody debit is held by the unresolved
	// row — a positive spend is never admitted unproven.
	if _, err = db.ObserveSharedCustodyOwnershipProof(ctx, manifest, navCfg, cleanup, 3_100_000_000, 300); custodyAttributionHoldReason(t, err) != "custody_attribution_unresolved_operation" {
		t.Fatalf("positive spend admitted past an unresolved row: %v", err)
	}
}

// custodyAdmissionProofFixture fabricates a pre-decision-shaped proof whose
// binding content is internally consistent with the given effects/lease. Only
// the seam functions under test read it.
func custodyAdmissionProofFixture(t *testing.T, cfg sharedCustodyAttributionConfig, routeKey string, effects ExpectedEffects, observedRaw uint64, observedSlot, generation, fencing int64, owner string) sharedCustodyAdmissionProof {
	t.Helper()
	effectsSHA256, err := expectedEffectsSHA256(effects)
	if err != nil {
		t.Fatal(err)
	}
	proof := sharedCustodyAdmissionProof{
		Proof:    sharedCustodyProof{ObservedRaw: observedRaw},
		RouteKey: routeKey, Lane: cfg.Lane, Custody: cfg.Custody, Mint: cfg.Mint,
		ObservedSlot: observedSlot, SpendRaw: 2_500_000_000, EffectsSHA256: effectsSHA256,
		Generation: generation, LeaseOwner: owner, LeaseFencing: fencing,
	}
	// The honest self-consistent digest: the locked send boundary recomputes
	// it from the carried content, so a fixture with an arbitrary digest would
	// refuse even on the coherent path.
	proof.Digest = sharedCustodyAdmissionDigest(proof)
	return proof
}

// custodyBuiltEffectsEnvelope is the persisted operation-evidence envelope
// with BUILT effects, exactly the post-MarkBuilt row shape the broadcast
// lock decodes.
func custodyBuiltEffectsEnvelope(t *testing.T, effects ExpectedEffects) []byte {
	t.Helper()
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"schema":          "loyal-backyard-rwa-operation-evidence/v1",
		"expectedEffects": json.RawMessage(encoded),
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// The Worker pre-decision seam (doc 26 §1): the strict proof is taken only
// for a prepared positive AUTO-PYUSD spend, carried as per-operation local
// data on the observation, and a missing proof producer holds fail-closed.
func TestSharedCustodyPreDecisionWorkerSeam(t *testing.T) {
	manifest := embeddedTestManifest(t)
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, productionRouteKey)
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	decision := Decision{Action: DeleverRouteStep, StrategyKey: cfg.Lane, AmountRaw: 2_500_000_000, IdempotencyKey: "k", Reason: "r"}
	proof := custodyAdmissionProofFixture(t, cfg, productionRouteKey, effects, 3_100_000_000, 300, 1, 7, "worker")

	wantEffectsSHA256, err := expectedEffectsSHA256(effects)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	runtime := tickRuntime{custodyOwnershipProof: func(ctx context.Context, m RouteManifest, got sharedCustodyAttributionConfig, gotEffects ExpectedEffects, observedRaw uint64, observedSlot int64) (sharedCustodyAdmissionProof, error) {
		calls++
		if got.Custody != cfg.Custody || got.Lane != cfg.Lane {
			t.Fatalf("seam called with drifted config: %+v", got)
		}
		gotEffectsSHA256, err := expectedEffectsSHA256(gotEffects)
		if err != nil || gotEffectsSHA256 != wantEffectsSHA256 {
			t.Fatalf("seam called with drifted effects: %v", err)
		}
		if observedRaw != 3_100_000_000 || observedSlot != 300 {
			t.Fatalf("seam observation drifted: raw=%d slot=%d", observedRaw, observedSlot)
		}
		return proof, nil
	}}
	w := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: runtime}
	observation := Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &observation, decision, effects, nil); err != nil {
		t.Fatalf("pre-decision proof refused: %v", err)
	}
	if calls != 1 || observation.carriedCustodyOwnershipProof() == nil {
		t.Fatalf("proof not carried: calls=%d carried=%v", calls, observation.carriedCustodyOwnershipProof() != nil)
	}
	// Zero-spend and non-AUTO lanes never call the producer and carry nothing.
	zero := Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &zero, decision, custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil), nil); err != nil {
		t.Fatalf("zero-spend AUTO operation held: %v", err)
	}
	other := Observation{}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &other, Decision{Action: DeleverRouteStep, StrategyKey: "Ethena/ETH/PYUSD"}, effects, nil); err != nil {
		t.Fatalf("non-AUTO lane held: %v", err)
	}
	if calls != 1 {
		t.Fatalf("producer called %d times for non-spending decisions", calls-1)
	}
	// No producer wired (e.g. an unwired test runtime) fails closed on a real
	// spend, never silently proceeds.
	bare := &Worker{routeKey: productionRouteKey, manifest: manifest}
	if err := bare.observePreDecisionCustodyOwnershipProof(context.Background(), &Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}, decision, effects, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_ownership_proof_unavailable" {
		t.Fatalf("unwired producer did not hold: %v", err)
	}
}

// The shared locked-admission seam (doc 26 §2) at the REAL route lock: the
// carried proof must survive generation/lease re-validation under FOR UPDATE
// and exact effects/observation binding; missing and drifted proofs hold.
func TestSharedCustodyAdmissionBindingAtRouteLock(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	key := fmt.Sprintf("auto-binding-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range []string{key} {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireRouteLease(ctx, key, "binding-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key)
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	proof := custodyAdmissionProofFixture(t, cfg, key, effects, 3_100_000_000, 300, 1, lease.FencingToken, "binding-worker")
	observation := Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}, custodyProof: &proof}

	bind := func(carried *sharedCustodyAdmissionProof, lane string, bindEffects ExpectedEffects) (sharedCustodyProofBinding, error) {
		t.Helper()
		tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rollbackCancel()
			_ = tx.Rollback(rollbackCtx)
		}()
		obs := observation
		obs.custodyProof = carried
		return bindSharedCustodyAdmissionProofOnManifest(ctx, tx, key, lane, obs, bindEffects)
	}
	binding, err := bind(&proof, cfg.Lane, effects)
	if err != nil {
		t.Fatalf("coherent proof refused at the route lock: %v", err)
	}
	if binding.Generation != 1 || binding.LeaseFencing != lease.FencingToken || binding.SpendRaw != 2_500_000_000 || binding.Digest != proof.Digest {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	if _, err = bind(nil, cfg.Lane, effects); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_missing" {
		t.Fatalf("missing proof admitted: %v", err)
	}
	driftedEffects := custodyAttributionRepayExpected(3_100_000_000, 700_000_000, 6_000_000_000, 8_500_000_000)
	if _, err = bind(&proof, cfg.Lane, driftedEffects); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("proof over different effects admitted: %v", err)
	}
	driftedObservation := observation
	driftedObservation.Snapshot.DebtIdleRaw = 700_000_000
	driftedProof := proof
	driftedProof.Proof.ObservedRaw = 700_000_000
	driftedObservation.custodyProof = &driftedProof
	if _, err = bind(&driftedProof, cfg.Lane, effects); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("proof over a different observation admitted: %v", err)
	}
	// A different lane (even sharing the route key) binds nothing.
	if other, err := bind(&proof, "Ethena/ETH/PYUSD", effects); err != nil || other.SpendRaw != 0 {
		t.Fatalf("non-AUTO lane bound: %+v %v", other, err)
	}
	// Generation drift at the lock (the proof was taken under generation 1;
	// the route is now at 2 with a new fence) holds.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state_version=2, state='{"generation":2}' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = bind(&proof, cfg.Lane, effects); custodyAttributionHoldReason(t, err) != "custody_attribution_generation_drift" {
		t.Fatalf("stale-generation proof admitted: %v", err)
	}
}

// The broadcast-intent lock seam (doc 26 §4): the bound proof is
// re-validated INSIDE the locked transaction against the PERSISTED built
// effects and the route lock row; missing and drifted proofs hold before
// broadcast intent is recorded.
func TestSharedCustodySendProofAtBroadcastLock(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("auto-sendlock-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range []string{key, key + "-zero-route"} {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireRouteLease(ctx, key, "sendlock-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key)
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	opID := key + "-op"
	envelope := custodyBuiltEffectsEnvelope(t, effects)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
		VALUES($1,$2,'signed',$3,$4,$5::jsonb || '{"decision":{"observationSlot":290}}'::jsonb)`, opID, key, string(DeleverRouteStep), cfg.Lane, envelope); err != nil {
		t.Fatal(err)
	}
	proof := sharedCustodyProofBindingFrom(custodyAdmissionProofFixture(t, cfg, key, effects, 3_100_000_000, 300, 1, lease.FencingToken, "sendlock-worker"))

	validate := func(carried *sharedCustodyProofBinding, rowEffects []byte) error {
		t.Helper()
		tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rollbackCancel()
			_ = tx.Rollback(rollbackCtx)
		}()
		if rowEffects != nil {
			if _, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=$2::jsonb || '{"decision":{"observationSlot":290}}'::jsonb WHERE operation_id=$1`, opID, rowEffects); err != nil {
				t.Fatal(err)
			}
		}
		return validateSharedCustodySendProofOnBroadcastTx(ctx, tx, manifest, opID, carried)
	}
	if err := validate(&proof, nil); err != nil {
		t.Fatalf("coherent bound proof refused at the broadcast lock: %v", err)
	}
	if err := validate(nil, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_missing" {
		t.Fatalf("missing bound proof admitted: %v", err)
	}
	// The persisted built effects are the authority: a proof over DIFFERENT
	// (still decode-valid, conserved) built effects drifts even though the
	// route identity and custody spend bind — the supply leg moved.
	if err := validate(&proof, custodyBuiltEffectsEnvelope(t, custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_100_000_000, 8_600_000_000))); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("proof over different persisted effects admitted: %v", err)
	}
	// Undecodable persisted effects fail closed with a decode error, not a
	// typed hold: nothing broadcasts, and the failure is loud, not silent.
	if err := validate(&proof, []byte("{}")); err == nil {
		t.Fatalf("undecodable persisted effects admitted at the broadcast lock")
	}
	// Generation drift under the lock holds.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state_version=2, state='{"generation":2}' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("stale-generation send proof admitted: %v", err)
	}
	// A proof observed under a superseded lease fence refuses: the route lease
	// was released and re-acquired between proof observation and this lock.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET fencing_token=fencing_token+1 WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_generation_drift" {
		t.Fatalf("stale-lease-fence send proof admitted: %v", err)
	}
	// An expired route lease refuses outright.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_lease_stale" {
		t.Fatalf("send proof admitted on an expired lease: %v", err)
	}
	// A zero-spend AUTO operation binds nothing: nil proof passes. It sits on
	// its OWN route — a route holds exactly one nonterminal operation — and
	// that route has NEVER been leased: the skip must not depend on lease
	// columns being set.
	zeroKey := key + "-zero-route"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, zeroKey); err != nil {
		t.Fatal(err)
	}
	zeroID := key + "-zero"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
		VALUES($1,$2,'signed',$3,$4,$5::jsonb)`, zeroID, zeroKey, string(ReportNAV), cfg.Lane,
		custodyBuiltEffectsEnvelope(t, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
			Accounts: []ExpectedAccountEffect{{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram,
				Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 5, AfterRaw: 5}}})); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Unconditional rollback on an independent context: a Fatal below must
	// never leave this transaction idle on the shared test database.
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rollbackCancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err := validateSharedCustodySendProofOnBroadcastTx(ctx, tx, manifest, zeroID, nil); err != nil {
		t.Fatalf("zero-spend AUTO operation held at the broadcast lock: %v", err)
	}
}

// The bind (doc 26 §2) against real PostgreSQL: the REAL kamino repay request
// compiled by the reviewed candidate manifest and the REAL bindOperation
// persisting the carried pre-decision proof under the route lock it holds.
// Once the decided row exists no fresh ownership proof is obtainable and no
// second bind is possible; a missing or stale-generation proof holds.
func TestBindPersistsTheCarriedCustodyProof(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	manifest := embeddedTestManifest(t)
	key := fmt.Sprintf("auto-bind-%d", time.Now().UnixNano())
	routes := []string{key, key + "-missing", key + "-drift"}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range routes {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), DeleverRouteStep, kaminoLegRepay, 2_500_000_000,
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{Action: DeleverRouteStep, StrategyKey: autoAUTOPYUSD.Lane, AmountRaw: 2_500_000_000,
		Reason: "auto-bind-repay", IdempotencyKey: "auto-bind"}
	// seed records one decided candidate-AUTO operation exactly as
	// RecordDecision persists it and carries a proof taken under its lease.
	seed := func(routeKey string) (string, RouteLease, Observation) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, routeKey); err != nil {
			t.Fatal(err)
		}
		lease, err := db.AcquireRouteLease(ctx, routeKey, "bind-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		observation := tickObservation(Snapshot{ObservationID: routeKey + "-obs", Slot: 42, Fresh: true,
			RouteKind: RouteKind, RouteLane: autoAUTOPYUSD.Lane, StrategyKey: autoAUTOPYUSD.Lane, DebtIdleRaw: 3_100_000_000})
		evidence, err := json.Marshal(newDecisionEvidence(observation, decision, sha256Bytes([]byte("manifest"))))
		if err != nil {
			t.Fatal(err)
		}
		id := routeKey + "-op"
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
			VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, routeKey, string(DeleverRouteStep), autoAUTOPYUSD.Lane,
			fmt.Sprintf(`{"decision":%s}`, evidence)); err != nil {
			t.Fatal(err)
		}
		proof := custodyAdmissionProofFixture(t, autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, routeKey), routeKey, effects, 3_100_000_000, 42, 1, lease.FencingToken, "bind-worker")
		observation.custodyProof = &proof
		return id, lease, observation
	}
	rpc := budgetBuildRPC(t, 5_000, 42)

	id, lease, observation := seed(key)
	if err := db.bindOperation(ctx, rpc, manifest, id, observation, decision, request, effects); err != nil {
		t.Fatalf("bind refused the carried proof: %v", err)
	}
	var authBytes []byte
	var version int64
	if err := db.pool.QueryRow(ctx, `SELECT o.expected_effects->'phase3',s.state_version FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE o.operation_id=$1`, id).Scan(&authBytes, &version); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(authBytes, &auth) != nil || auth.CustodyProof == nil {
		t.Fatalf("bind persisted no custody binding: %s", authBytes)
	}
	if binding := *auth.CustodyProof; binding.Generation != 1 || binding.LeaseFencing != lease.FencingToken || binding.LeaseOwner != "bind-worker" ||
		binding.SpendRaw != 2_500_000_000 || binding.RouteKey != key || binding.Lane != autoAUTOPYUSD.Lane || version != 1 {
		t.Fatalf("persisted binding is not the bound proof, or the bind consumed a generation: %+v version=%d", binding, version)
	}
	// Once the decided row exists the production ownership-proof API refuses
	// the route, and the row cannot be bound a second time with any proof.
	if _, err := db.ObserveSharedCustodyOwnershipProof(ctx, manifest, autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key), effects, 3_100_000_000, 42); custodyAttributionHoldReason(t, err) != "custody_attribution_unresolved_operation" {
		t.Fatalf("ownership proof observed while the decided row exists: %v", err)
	}
	assertBudgetHold(t, db.bindOperation(ctx, rpc, manifest, id, observation, decision, request, effects), "bind_journal_mismatch")

	// A real AUTO spend without its carried proof holds.
	missingID, _, missing := seed(key + "-missing")
	missing.custodyProof = nil
	assertBudgetHold(t, db.bindOperation(ctx, rpc, manifest, missingID, missing, decision, request, effects), "custody_attribution_proof_missing")

	// A proof taken under an earlier route generation holds.
	driftID, _, drift := seed(key + "-drift")
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state_version=2, state='{"generation":2}' WHERE route_key=$1`, key+"-drift"); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.bindOperation(ctx, rpc, manifest, driftID, drift, decision, request, effects), "custody_attribution_generation_drift")
}
