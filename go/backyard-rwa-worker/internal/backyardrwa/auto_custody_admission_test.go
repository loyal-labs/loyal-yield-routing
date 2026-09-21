package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
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

func TestSharedCustodyCurrentOperationValidation(t *testing.T) {
	cfg := custodyAttributionConfig()
	spend := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	spendBytes, err := jsonMarshalExpectedEffects(spend)
	if err != nil {
		t.Fatal(err)
	}
	build, delegate := custodyAdmissionSignedBuild(t, bytes.Repeat([]byte{11}, ed25519.SeedSize))
	cfg.Delegate = delegate
	signedRow := sharedCustodyCurrentOperationRow{
		Status: "signed", StrategyKey: cfg.Lane, SignedWirePresent: true, SignedWire: build.SignedWire,
		SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature,
		MessageSHA256: build.MessageSHA256, RecentBlockhash: build.RecentBlockhash,
		LastValidBlockHeight: build.LastValidBlockHeight, SimulationSlot: build.SimulationSlot,
		ExpectedEffects: spendBytes,
	}
	decidedRow := sharedCustodyCurrentOperationRow{Status: "decided", StrategyKey: cfg.Lane, ExpectedEffects: spendBytes}

	// Happy path: the exact persisted signed wire (bound digest AND
	// signature) at Signed — the ONLY excludable state.
	if err := validateSharedCustodyCurrentOperation(sharedCustodyCurrentOperation{
		OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature,
		ExpectedEffects: spend,
	}, signedRow, cfg); err != nil {
		t.Fatalf("exact persisted signed wire refused: %v", err)
	}

	invalid := func(name string, current sharedCustodyCurrentOperation, row sharedCustodyCurrentOperationRow, rowCfg sharedCustodyAttributionConfig) {
		t.Helper()
		err := validateSharedCustodyCurrentOperation(current, row, rowCfg)
		if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_current_operation_invalid" {
			t.Fatalf("%s: %s", name, reason)
		}
	}
	// At Signed the claim must be the exact persisted digest AND signature.
	invalid("wrong claimed digest", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: strings.Repeat("a", 64), TransactionSignature: build.TransactionSignature, ExpectedEffects: spend}, signedRow, cfg)
	invalid("wrong claimed signature", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: "3Zyv", ExpectedEffects: spend}, signedRow, cfg)
	// The persisted wire must bind together: digest, message, blockhash and
	// the sole-signer signature.
	invalid("tampered persisted wire", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature, ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow {
			row := signedRow
			row.SignedWire = append([]byte(nil), row.SignedWire...)
			row.SignedWire[len(row.SignedWire)-1] ^= 1
			return row
		}(), cfg)
	// A self-consistent wire from a DIFFERENT signer is refused by the
	// delegate pin, even though every digest binds.
	other, otherDelegate := custodyAdmissionSignedBuild(t, bytes.Repeat([]byte{12}, ed25519.SeedSize))
	_ = otherDelegate
	pinned := cfg.Delegate
	cfg.Delegate = publicKeyFromBytes(bytes.Repeat([]byte{13}, 32))
	invalid("delegate pin", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: other.SignedWireSHA256, TransactionSignature: other.TransactionSignature, ExpectedEffects: spend},
		sharedCustodyCurrentOperationRow{Status: "signed", StrategyKey: cfg.Lane, SignedWirePresent: true, SignedWire: other.SignedWire,
			SignedWireSHA256: other.SignedWireSHA256, TransactionSignature: other.TransactionSignature, MessageSHA256: other.MessageSHA256,
			RecentBlockhash: other.RecentBlockhash, LastValidBlockHeight: other.LastValidBlockHeight, SimulationSlot: other.SimulationSlot,
			ExpectedEffects: spendBytes}, cfg)
	cfg.Delegate = pinned
	// Pre-sign rows are NEVER excludable: recordDecisionTx persists only the
	// decision evidence (no built effects — DecodeExpectedEffects refuses
	// that state), so the custody walk runs before the row exists and after
	// signing, never in between.
	invalid("decided row", sharedCustodyCurrentOperation{OperationID: "op", ExpectedEffects: spend}, decidedRow, cfg)
	invalid("built row", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature, ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow { row := decidedRow; row.Status = "built"; return row }(), cfg)
	invalid("simulated row", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature, ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow { row := decidedRow; row.Status = "simulated"; return row }(), cfg)
	// Broadcast intent, post-broadcast states, foreign lanes sharing the
	// route key, and mismatched operation IDs are never excludable.
	invalid("broadcast intent", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature, ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow { row := signedRow; row.BroadcastIntent = true; return row }(), cfg)
	invalid("submitted status", sharedCustodyCurrentOperation{OperationID: "op", SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature, ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow { row := signedRow; row.Status = "submitted"; return row }(), cfg)
	invalid("manual recovery status", sharedCustodyCurrentOperation{OperationID: "op", ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow { row := decidedRow; row.Status = "manual_recovery"; return row }(), cfg)
	invalid("foreign lane on shared route key", sharedCustodyCurrentOperation{OperationID: "op", ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow {
			row := decidedRow
			row.StrategyKey = "Ethena/ETH/PYUSD"
			return row
		}(), cfg)
	invalid("mismatched operation id", sharedCustodyCurrentOperation{OperationID: "", ExpectedEffects: spend}, decidedRow, cfg)
	// The caller's spend intent must equal the persisted built effects
	// exactly: the same custody at a different amount is a different spend.
	drifted := custodyAttributionRepayExpected(3_100_000_000, 700_000_000, 6_000_000_000, 8_500_000_000)
	invalid("same custody different amount", sharedCustodyCurrentOperation{OperationID: "op", ExpectedEffects: drifted}, decidedRow, cfg)
	// Undecodable persisted effects and persisted non-spends are refused.
	invalid("undecodable persisted effects", sharedCustodyCurrentOperation{OperationID: "op", ExpectedEffects: spend},
		func() sharedCustodyCurrentOperationRow {
			row := decidedRow
			row.ExpectedEffects = []byte("{}")
			return row
		}(), cfg)
	invalid("persisted credit-only effects", sharedCustodyCurrentOperation{OperationID: "op",
		ExpectedEffects: custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil)},
		func() sharedCustodyCurrentOperationRow {
			funding, err := jsonMarshalExpectedEffects(custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil))
			if err != nil {
				t.Fatal(err)
			}
			row := decidedRow
			row.ExpectedEffects = funding
			return row
		}(), cfg)
	_ = spend
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

// The full lifecycle in the production Tick phase order: REAL journal rows,
// the REAL decision persistence (RecordDecisionOnManifest — which stores ONLY
// decision evidence, expectedEffects null), REAL build/simulation transitions
// (MarkBuilt, MarkSimulated), the production PersistSignedUpdate SQL with a
// genuinely signed wire, and the real candidate-AUTO planning state: a
// persisted candidate selector entry that only the explicit reviewed manifest
// decodes. The custody proofs bind at the exact persisted state of each
// phase: strict ownership proof BEFORE the row exists, send recheck only at
// Signed.
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
	manifest := autoInitializerFixtureManifest(t)
	// Both binding states of the manifest-scoped planning read: the explicit
	// absent fixture (the shipped pre-install state) still refuses the
	// persisted candidate entry, while the embedded manifest — which carries
	// the installed binding after the release — decodes it back exactly.
	if _, err := db.readRoutePlanningStateOnManifest(ctx, autoAbsentBindingManifest(t), key, true); err == nil {
		t.Fatal("absent binding decoded a persisted candidate AUTO selector entry")
	}
	decoded, err := db.readRoutePlanningStateOnManifest(ctx, requireEmbeddedInstalledBinding(t), key, true)
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
	build, delegate := custodyAdmissionSignedBuild(t, bytes.Repeat([]byte{11}, ed25519.SeedSize))
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
	if proof.SpendRaw != 2_500_000_000 || proof.ExcludedOperation != "" || proof.Generation != 1 ||
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
	record, err := db.RecordDecisionOnManifest(ctx, manifest, key, obs, decision, manifest.SHA256, sha256Bytes([]byte("policies")))
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
	// The send proof refuses a decided row — no blind exemption by route,
	// lane, or operation id.
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300,
		sharedCustodySignedSpend{OperationID: id, SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature}); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("decided row excluded: %v", err)
	}

	// (a3) REAL build persistence: MarkBuilt persists the built expected
	// effects (the first point at which DecodeExpectedEffects succeeds on
	// the row), then MarkSimulated.
	envelope, err := jsonMarshalExpectedEffects(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkBuilt(ctx, id, sha256Bytes([]byte("cleanup-build-message")), envelope); err != nil {
		t.Fatalf("real MarkBuilt failed: %v", err)
	}
	if err := db.MarkSimulated(ctx, id, SimulationResult{Slot: 500, UnitsConsumed: 42_000}); err != nil {
		t.Fatalf("real MarkSimulated failed: %v", err)
	}

	// REAL signed persistence: the production PersistSignedUpdate statement,
	// simulated -> signed, exactly as the production send path calls it (see
	// the fixture-scope note at the top of this file for the delegate
	// narrowing), then verify every signed column actually landed.
	signedUpdate, err := db.pool.Exec(ctx, PersistSignedUpdate, id, build.MessageSHA256, build.SignedWire,
		build.SignedWireSHA256, build.TransactionSignature, build.RecentBlockhash, build.LastValidBlockHeight)
	if err != nil {
		t.Fatalf("production PersistSignedUpdate failed: %v", err)
	}
	if signedUpdate.RowsAffected() != 1 {
		t.Fatalf("PersistSignedUpdate touched %d rows, want the one simulated operation", signedUpdate.RowsAffected())
	}
	var (
		persistedStatus, persistedDigest, persistedSignature, persistedBlockhash string
		persistedWire                                                            []byte
		persistedHeight                                                          int64
	)
	if err := db.pool.QueryRow(ctx, `SELECT status, COALESCE(signed_wire_sha256,''), COALESCE(transaction_signature,''),
		COALESCE(recent_blockhash,''), COALESCE(signed_wire,''), COALESCE(last_valid_block_height,0)
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&persistedStatus, &persistedDigest,
		&persistedSignature, &persistedBlockhash, &persistedWire, &persistedHeight); err != nil {
		t.Fatal(err)
	}
	if persistedStatus != "signed" || persistedDigest != build.SignedWireSHA256 || persistedSignature != build.TransactionSignature ||
		persistedBlockhash != build.RecentBlockhash || !bytes.Equal(persistedWire, build.SignedWire) || persistedHeight != build.LastValidBlockHeight {
		t.Fatalf("PersistSignedUpdate did not persist the exact signed wire identity: %+v", build)
	}

	// (b) Send recheck at the Signed phase: the exact persisted wire identity
	// — digest AND signature — admits the pre-broadcast proof.
	signed := sharedCustodySignedSpend{OperationID: id, SignedWireSHA256: build.SignedWireSHA256, TransactionSignature: build.TransactionSignature}
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed); err != nil {
		t.Fatalf("signed recheck proof refused: %v", err)
	}
	wrongDigest := signed
	wrongDigest.SignedWireSHA256 = strings.Repeat("a", 64)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, wrongDigest); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("wrong claimed wire digest accepted: %v", err)
	}
	wrongSignature := signed
	wrongSignature.TransactionSignature = "3Zyv"
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, wrongSignature); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("wrong claimed signature accepted: %v", err)
	}

	// (c) A tampered persisted wire breaks the persisted binding.
	other, otherDelegate := custodyAdmissionSignedBuild(t, bytes.Repeat([]byte{12}, ed25519.SeedSize))
	_ = otherDelegate
	exec(`UPDATE loyal_yield.multiply_operations SET signed_wire=$2 WHERE operation_id=$1`, id, other.SignedWire)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("tampered persisted wire accepted: %v", err)
	}
	exec(`UPDATE loyal_yield.multiply_operations SET signed_wire=$2 WHERE operation_id=$1`, id, build.SignedWire)

	// (d) A self-consistent wire from a DIFFERENT signer fails the delegate
	// pin even though every digest binds: the claim matches the persisted
	// (other) wire exactly, so ONLY the signer pin can refuse it.
	exec(`UPDATE loyal_yield.multiply_operations SET signed_wire=$2, signed_wire_sha256=$3, transaction_signature=$4,
		message_sha256=$5, recent_blockhash=$6, last_valid_block_height=$7 WHERE operation_id=$1`,
		id, other.SignedWire, other.SignedWireSHA256, other.TransactionSignature, other.MessageSHA256, other.RecentBlockhash, other.LastValidBlockHeight)
	otherSigned := sharedCustodySignedSpend{OperationID: id, SignedWireSHA256: other.SignedWireSHA256, TransactionSignature: other.TransactionSignature}
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, otherSigned); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("non-pinned delegate wire accepted: %v", err)
	}
	exec(`UPDATE loyal_yield.multiply_operations SET signed_wire=$2, signed_wire_sha256=$3, transaction_signature=$4,
		message_sha256=$5, recent_blockhash=$6, last_valid_block_height=$7 WHERE operation_id=$1`,
		id, build.SignedWire, build.SignedWireSHA256, build.TransactionSignature, build.MessageSHA256, build.RecentBlockhash, build.LastValidBlockHeight)

	// (e) Broadcast intent and post-broadcast states are never excludable.
	exec(`UPDATE loyal_yield.multiply_operations SET broadcast_intent_at=now() WHERE operation_id=$1`, id)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("broadcast-intent operation excluded: %v", err)
	}
	exec(`UPDATE loyal_yield.multiply_operations SET broadcast_intent_at=NULL WHERE operation_id=$1`, id)
	exec(`UPDATE loyal_yield.multiply_operations SET status='submitted' WHERE operation_id=$1`, id)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed); custodyAttributionHoldReason(t, err) != "custody_attribution_current_operation_invalid" {
		t.Fatalf("submitted operation excluded: %v", err)
	}
	exec(`UPDATE loyal_yield.multiply_operations SET status='signed' WHERE operation_id=$1`, id)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed); err != nil {
		t.Fatalf("proof did not recover after fixture reset: %v", err)
	}

	// (f) A foreign lane touching the SAME production route key is the newest
	// participant and refuses the proof; removing it restores the proof.
	foreign := custodyAttributionFundingRow(t, key+"-foreign", "sig-admission-foreign", 400)
	foreign.RouteKey, foreign.StrategyKey = key, "Ethena/ETH/PYUSD"
	insertRow(foreign)
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 500, signed); custodyAttributionHoldReason(t, err) != "custody_attribution_foreign_lane" {
		t.Fatalf("same-route foreign-lane touch not refused: %v", err)
	}
	exec(`DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1`, key+"-foreign")
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 500, signed); err != nil {
		t.Fatalf("proof did not recover after foreign row removal: %v", err)
	}

	// (g) Observed-amount drift with no journal row is a tip balance
	// mismatch, never a pass.
	if _, err = db.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 700_000_000, 600, signed); custodyAttributionHoldReason(t, err) != "custody_attribution_balance_mismatch" {
		t.Fatalf("amount drift accepted: %v", err)
	}

	// (h) Restart: a fresh worker handle re-acquiring the route lease
	// reconstructs the identical chain under the new fence. AcquireRouteLease
	// never treats an unexpired lease as re-entrant — even for the identical
	// owner — so the restart overlap is simulated by expiring the test-owned
	// lease row first: only an expired row may increment the fence.
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	exec(`UPDATE loyal_yield.multiply_route_states SET lease_expires_at = now() - interval '1 second' WHERE route_key=$1`, key)
	newLease, err := restarted.AcquireRouteLease(ctx, key, "admission-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	restartedProof, err := restarted.ObserveSharedCustodySendProof(ctx, manifest, cfg, cleanup, 3_100_000_000, 300, signed)
	if err != nil {
		t.Fatalf("restart proof refused: %v", err)
	}
	if !reflect.DeepEqual(restartedProof.Proof.Steps, proof.Proof.Steps) || restartedProof.Proof.Origin != proof.Proof.Origin {
		t.Fatal("restart did not reconstruct the identical chain")
	}
	if restartedProof.LeaseFencing == lease.FencingToken || restartedProof.LeaseFencing != newLease.FencingToken {
		t.Fatal("restart fence not carried")
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

// The build fence (doc 26 §3) at its exact boundary: a positive AUTO-PYUSD
// spend requires the persisted binding bound to the exact effects and the
// current generation; missing legacy proofs hold; non-AUTO lanes and
// zero-spend AUTO operations are untouched.
func TestSharedCustodyBuildBindingRequiresPersistedProof(t *testing.T) {
	cfg := custodyAttributionConfig()
	routeKey := "route"
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	proof := custodyAdmissionProofFixture(t, cfg, routeKey, effects, 3_100_000_000, 300, 1, 7, "worker")
	binding := sharedCustodyProofBindingFrom(proof)
	auth := phase3OperationAuthorization{CustodyProof: &binding}
	if err := requireSharedCustodyBuildBinding(cfg.Lane, routeKey, auth, effects, 1); err != nil {
		t.Fatalf("coherent persisted binding refused: %v", err)
	}
	drifted := func(name string, mutate func(*sharedCustodyProofBinding)) {
		t.Helper()
		b := sharedCustodyProofBindingFrom(proof)
		mutate(&b)
		if err := requireSharedCustodyBuildBinding(cfg.Lane, routeKey, phase3OperationAuthorization{CustodyProof: &b}, effects, 1); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	drifted("wrong spend", func(b *sharedCustodyProofBinding) { b.SpendRaw = 1 })
	drifted("wrong effects digest", func(b *sharedCustodyProofBinding) { b.EffectsSHA256 = strings.Repeat("a", 64) })
	drifted("stale generation", func(b *sharedCustodyProofBinding) { b.Generation = 2 })
	drifted("unfenced", func(b *sharedCustodyProofBinding) { b.LeaseFencing = 0 })
	drifted("foreign custody", func(b *sharedCustodyProofBinding) { b.Custody = "other" })
	drifted("no digest", func(b *sharedCustodyProofBinding) { b.Digest = "" })
	// A missing legacy proof on a real AUTO spend holds.
	if err := requireSharedCustodyBuildBinding(cfg.Lane, routeKey, phase3OperationAuthorization{}, effects, 1); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_missing" {
		t.Fatalf("missing proof accepted: %v", err)
	}
	// The same spend on a different lane is untouched.
	if err := requireSharedCustodyBuildBinding("Ethena/ETH/PYUSD", routeKey, phase3OperationAuthorization{}, effects, 1); err != nil {
		t.Fatalf("non-AUTO lane held: %v", err)
	}
	// Zero-spend AUTO operations bind nothing.
	if err := requireSharedCustodyBuildBinding(cfg.Lane, routeKey, phase3OperationAuthorization{}, custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil), 1); err != nil {
		t.Fatalf("zero-spend AUTO operation held: %v", err)
	}
}

// The Worker pre-decision seam (doc 26 §1): the strict proof is taken only
// for a prepared positive AUTO-PYUSD spend, carried as per-operation local
// data on the observation, and a missing proof producer holds fail-closed.
func TestSharedCustodyPreDecisionWorkerSeam(t *testing.T) {
	manifest := autoInitializerFixtureManifest(t)
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
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &observation, decision, effects); err != nil {
		t.Fatalf("pre-decision proof refused: %v", err)
	}
	if calls != 1 || observation.carriedCustodyOwnershipProof() == nil {
		t.Fatalf("proof not carried: calls=%d carried=%v", calls, observation.carriedCustodyOwnershipProof() != nil)
	}
	// Zero-spend and non-AUTO lanes never call the producer and carry nothing.
	zero := Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &zero, decision, custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil)); err != nil {
		t.Fatalf("zero-spend AUTO operation held: %v", err)
	}
	other := Observation{}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &other, Decision{Action: DeleverRouteStep, StrategyKey: "Ethena/ETH/PYUSD"}, effects); err != nil {
		t.Fatalf("non-AUTO lane held: %v", err)
	}
	if calls != 1 {
		t.Fatalf("producer called %d times for non-spending decisions", calls-1)
	}
	// No producer wired (e.g. an unwired test runtime) fails closed on a real
	// spend, never silently proceeds.
	bare := &Worker{routeKey: productionRouteKey, manifest: manifest}
	if err := bare.observePreDecisionCustodyOwnershipProof(context.Background(), &Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}, decision, effects); custodyAttributionHoldReason(t, err) != "custody_attribution_ownership_proof_unavailable" {
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

// The broadcast-intent lock seam (doc 26 §4): the fresh send proof is
// re-validated INSIDE the locked transaction against the PERSISTED built
// effects and the route lock row; missing, foreign, and drifted proofs hold
// before broadcast intent is recorded.
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
		VALUES($1,$2,'signed',$3,$4,$5::jsonb)`, opID, key, string(DeleverRouteStep), cfg.Lane, envelope); err != nil {
		t.Fatal(err)
	}
	proof := custodyAdmissionProofFixture(t, cfg, key, effects, 3_100_000_000, 300, 1, lease.FencingToken, "sendlock-worker")
	proof.ExcludedOperation = opID
	// The exclusion is part of the digested content: re-commit after naming
	// the signed operation, as ObserveSharedCustodySendProof does.
	proof.Digest = sharedCustodyAdmissionDigest(proof)

	cost := ValuedTransactionCost{ObservationSlot: 300, ValidThroughSlot: 400}
	validate := func(carried *sharedCustodyAdmissionProof, rowEffects []byte, validationCost ValuedTransactionCost) error {
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
			if _, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=$2::jsonb WHERE operation_id=$1`, opID, rowEffects); err != nil {
				t.Fatal(err)
			}
		}
		return validateSharedCustodySendProofOnBroadcastTx(ctx, tx, manifest, opID, validationCost, carried)
	}
	if err := validate(&proof, nil, cost); err != nil {
		t.Fatalf("coherent send proof refused at the broadcast lock: %v", err)
	}
	if err := validate(nil, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_missing" {
		t.Fatalf("missing send proof admitted: %v", err)
	}
	foreign := proof
	foreign.ExcludedOperation = key + "-other"
	foreign.Digest = sharedCustodyAdmissionDigest(foreign)
	if err := validate(&foreign, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("proof for another operation admitted: %v", err)
	}
	// The persisted built effects are the authority: a proof over DIFFERENT
	// (still decode-valid, conserved) built effects drifts even though the
	// route identity and custody spend bind — the supply leg moved.
	if err := validate(&proof, custodyBuiltEffectsEnvelope(t, custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_100_000_000, 8_600_000_000)), cost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("proof over different persisted effects admitted: %v", err)
	}
	// Undecodable persisted effects fail closed with a decode error, not a
	// typed hold: nothing broadcasts, and the failure is loud, not silent.
	if err := validate(&proof, []byte("{}"), cost); err == nil {
		t.Fatalf("undecodable persisted effects admitted at the broadcast lock")
	}
	// A proof mutated after digesting refuses on self-consistency: the digest
	// is recomputed from the carried content at the lock.
	mutated := proof
	mutated.Proof.ObservedRaw++
	if err := validate(&mutated, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("mutated send proof admitted: %v", err)
	}
	// The custody observation must sit inside the SAME valuation window the
	// broadcast cost was priced in.
	staleWindow := cost
	staleWindow.ObservationSlot = proof.ObservedSlot + 1
	if err := validate(&proof, nil, staleWindow); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("send proof observed outside the cost window admitted: %v", err)
	}
	// Generation drift under the lock holds.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state_version=2, state='{"generation":2}' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("stale-generation send proof admitted: %v", err)
	}
	// A proof observed under a superseded lease fence refuses: the route lease
	// was released and re-acquired between proof observation and this lock.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET fencing_token=fencing_token+1 WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_generation_drift" {
		t.Fatalf("stale-lease-fence send proof admitted: %v", err)
	}
	// An expired route lease refuses outright.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := validate(&proof, nil, cost); custodyAttributionHoldReason(t, err) != "custody_attribution_lease_stale" {
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
	if err := validateSharedCustodySendProofOnBroadcastTx(ctx, tx, manifest, zeroID, cost, nil); err != nil {
		t.Fatalf("zero-spend AUTO operation held at the broadcast lock: %v", err)
	}
}

// The measured locked admission -> build boundary (doc 26 §2/§3) exercised as
// the production chain: the REAL kamino repay request compiled by the reviewed
// candidate manifest, the REAL locked measured admission
// (persistPhase3ExitAdmissionOnManifest) creating the bounded AUTO reservation
// and persisting the custody binding under the ADMITTED generation, the REAL
// retry admission, and the REAL build authorization
// (authorizePhase3BuildOnManifest) passing at that post-admission generation.
// The composed fixture parts are the decided operation row with its journal
// evidence and the admission plan envelope (costs) — exactly how the seeded
// initializer authorization tests compose their rows; budget admission,
// generation, custody binding, build and retry logic are the production
// implementations with no test-only budget generation.
// TestSharedCustodyPersistedBindingAuthorizesRetryAndRestartedBuild exercises
// the REAL SQL boundaries — ObserveSharedCustodyOwnershipProof,
// persistPhase3ExitAdmissionOnManifest, authorizePhase3BuildOnManifest,
// writePhase3BudgetTx — against the disposable database. The plan envelope
// (cost micros, message digest, slot window, blockhash) and the carried
// pre-decision ownership proof are FIXTURE-VALUED: they are not outputs of a
// measured RPC producer, and no fixture composes budget state — reservations,
// bindings, and generations are read back from the persisted rows the
// production code wrote.
func TestSharedCustodyPersistedBindingAuthorizesRetryAndRestartedBuild(t *testing.T) {
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
	manifest := autoInitializerFixtureManifest(t)
	key := fmt.Sprintf("auto-measured-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range []string{key, key + "-legacy"} {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})

	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key)
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	rawEffects, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, 2_500_000_000,
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, cfg.Lane)
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{Action: DeleverRouteStep, StrategyKey: cfg.Lane, AmountRaw: 2_500_000_000,
		Reason: "auto-measured-repay", IdempotencyKey: "auto-measured"}

	// seedDecision seeds one decided candidate-AUTO operation with a journal
	// evidence row exactly as RecordDecision persists it, plus the AUTO family
	// exit reserve the recovery admission draws from, and returns the
	// operation id, lease and the observation the evidence was recorded for.
	seed := func(routeKey string) (string, RouteLease, Observation) {
		t.Helper()
		budget := emptyTestBudget()
		budget.Families["AUTO"] = FamilyBudget{ExitMicros: 900_000}
		budgetBytes, err := json.Marshal(budget)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2::jsonb,$3)`,
			routeKey, fmt.Sprintf(`{"generation":1,"phase3":%s}`, budgetBytes), 1); err != nil {
			t.Fatal(err)
		}
		lease, err := db.AcquireRouteLease(ctx, routeKey, "measured-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		observation := tickObservation(Snapshot{ObservationID: routeKey + "-obs", Slot: 42, Fresh: true,
			RouteKind: RouteKind, RouteLane: cfg.Lane, StrategyKey: cfg.Lane, DebtIdleRaw: 3_100_000_000})
		evidence := newDecisionEvidence(observation, decision, sha256Bytes([]byte("manifest")), sha256Bytes([]byte("catalog")))
		evidenceBytes, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		id := routeKey + "-op"
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
			VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, routeKey, string(DeleverRouteStep), cfg.Lane,
			fmt.Sprintf(`{"decision":%s}`, evidenceBytes)); err != nil {
			t.Fatal(err)
		}
		return id, lease, observation
	}
	planFor := func(observation Snapshot) phase3BridgeAdmission {
		input, err := encodePhase3BuildInput(request, rawEffects)
		if err != nil {
			t.Fatal(err)
		}
		return phase3BridgeAdmission{
			Snapshot: observation, Decision: decision, Input: input,
			CurrentCost: ValuedTransactionCost{TotalMicros: 400_000, MessageSHA256: sha256Bytes([]byte("measured")),
				ObservationSlot: 42, ValidThroughSlot: 74},
			ValidThroughSlot: 74,
		}
	}
	stateVersion := func() int64 {
		t.Helper()
		var version int64
		if err := db.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	persistedAuth := func(t *testing.T, id string) phase3OperationAuthorization {
		t.Helper()
		var authBytes []byte
		if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&authBytes); err != nil {
			t.Fatal(err)
		}
		var auth phase3OperationAuthorization
		if json.Unmarshal(authBytes, &auth) != nil {
			t.Fatal("undecodable persisted authorization")
		}
		return auth
	}
	rpc := budgetBuildRPC(t, 5_000, 42)

	// Measured admission: the carried pre-decision proof is consumed by the
	// shared locked admission, which creates the bounded AUTO reservation and
	// persists the binding under the ADMITTED generation (pre-admission 1 ->
	// admitted 2 through writePhase3BudgetTx's single increment).
	id, lease, observation := seed(key)
	carried := custodyAdmissionProofFixture(t, cfg, key, effects, 3_100_000_000, 42, 1, lease.FencingToken, "measured-worker")
	observation.custodyProof = &carried
	if err := db.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, id, observation, decision, planFor(observation.Snapshot)); err != nil {
		t.Fatalf("measured AUTO admission refused with carried proof: %v", err)
	}
	if version := stateVersion(); version != 2 {
		t.Fatalf("admission did not advance the generation to the admitted state: %d", version)
	}
	auth := persistedAuth(t, id)
	if auth.CustodyProof == nil {
		t.Fatalf("measured admission persisted no custody binding")
	}
	binding := *auth.CustodyProof
	if !binding.bindsGeneration(2) || binding.Generation != 2 || binding.LeaseFencing != lease.FencingToken ||
		binding.LeaseOwner != "measured-worker" ||
		binding.SpendRaw != 2_500_000_000 || binding.RouteKey != key || binding.Lane != cfg.Lane {
		t.Fatalf("persisted binding is not the admitted-generation binding: %+v", binding)
	}
	var budgetBytes []byte
	if err := db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&budgetBytes); err != nil {
		t.Fatal(err)
	}
	var budget Phase3Budget
	if json.Unmarshal(budgetBytes, &budget) != nil {
		t.Fatal("undecodable persisted budget")
	}
	reserved, ok := budget.Reservations[id]
	if !ok || reserved.Family != "AUTO" || reserved.UpperMicros != 400_000 || !reserved.Recovery {
		t.Fatalf("measured admission did not create the bounded AUTO reservation: %+v", reserved)
	}

	// REACHABILITY (real API, not fixture): once the decided row exists the
	// production ownership-proof API refuses the route outright and must keep
	// doing so — so a fresh carried proof at the admitted generation is NOT
	// obtainable for a retry. The retry's only honest input is the binding the
	// first measured admission persisted.
	if _, err := db.ObserveSharedCustodyOwnershipProof(ctx, manifest, cfg, effects, 3_100_000_000, 42); custodyAttributionHoldReason(t, err) != "custody_attribution_unresolved_operation" {
		t.Fatalf("ownership proof observed while the decided row exists (a retry can never obtain one): %v", err)
	}

	// The reachable retry carries NO proof: the admission re-validates the
	// PERSISTED admitted binding under the current lease and generation,
	// consumes no generation, and does not replenish the reservation.
	retryObservation := observation
	retryObservation.custodyProof = nil
	if err := db.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, id, retryObservation, decision, planFor(retryObservation.Snapshot)); err != nil {
		t.Fatalf("retry admission over the persisted admitted binding refused: %v", err)
	}
	if version := stateVersion(); version != 2 {
		t.Fatalf("retry incremented the generation: %d", version)
	}
	if retried := persistedAuth(t, id).CustodyProof; retried == nil || !retried.bindsGeneration(2) {
		t.Fatalf("retry did not preserve the admitted binding: %+v", retried)
	}

	// Build authorization passes at the post-admission generation from the
	// PERSISTED binding alone, with no built effects in the row, and the
	// build's own write advances the persisted binding with it (admitted 2 ->
	// post-build 3).
	buildCost := ValuedTransactionCost{TotalMicros: 400_000, MessageSHA256: sha256Bytes([]byte("measured")),
		ObservationSlot: 42, ValidThroughSlot: 74}
	if err := db.authorizePhase3BuildOnManifest(ctx, manifest, rpc, id, request, rawEffects, buildCost); err != nil {
		t.Fatalf("build authorization refused with persisted admitted binding: %v", err)
	}
	if version := stateVersion(); version != 3 {
		t.Fatalf("build did not advance the generation through its own write: %d", version)
	}
	if built := persistedAuth(t, id).CustodyProof; built == nil || built.Generation != 3 {
		t.Fatalf("build did not advance the persisted binding to its own post-write generation: %+v", built)
	}

	// A REPEATED build authorization under the SAME live lease — the
	// crash-after-build, before-MarkBuilt tick retry inside one worker
	// process — re-validates the binding at the CURRENT generation and
	// consumes the next one. This is the retry the un-advanced binding used
	// to brick with custody_attribution_proof_drift. It is NOT a process
	// restart; the restart case follows below.
	if err := db.authorizePhase3BuildOnManifest(ctx, manifest, rpc, id, request, rawEffects, buildCost); err != nil {
		t.Fatalf("repeated build authorization refused under the live lease: %v", err)
	}
	if version := stateVersion(); version != 4 {
		t.Fatalf("repeated build did not advance the generation through its own write: %d", version)
	}
	if rebuilt := persistedAuth(t, id).CustodyProof; rebuilt == nil || rebuilt.Generation != 4 {
		t.Fatalf("repeated build did not advance the persisted binding: %+v", rebuilt)
	}

	// PROCESS RESTART, not a repeat call: a new Database handle holds no
	// lease and must re-acquire one, and a re-acquire always issues a NEW
	// fencing token (leases are never re-entrant, same owner included).
	// Expire the old lease exactly as its TTL would and re-acquire it from
	// the restarted handle.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states
		SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	restartLease, err := restarted.AcquireRouteLease(ctx, key, "measured-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if restartLease.FencingToken == lease.FencingToken {
		t.Fatalf("restart re-acquired the lease without re-fencing: %d", restartLease.FencingToken)
	}

	// SEMANTIC LIMITATION, fail-closed by design: the admission retry under
	// the re-acquired lease refuses. The persisted binding carries the lease
	// identity it was admitted under (owner + old fencing token) and the
	// retry validator compares that identity against the current lock.
	// Same-worker retries recover; a restart cannot re-pass ADMISSION for an
	// already-admitted AUTO custody operation and must take the
	// terminal/manual path instead. Deliberately NOT relaxed here: the
	// coordinator rejected renewing stale custody authority (doc 28's generic
	// writePhase3BudgetTx restamping), and rebinding the lease identity in
	// the validator would be the same move in a different place.
	if err := restarted.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, id, retryObservation, decision, planFor(retryObservation.Snapshot)); custodyAttributionHoldReason(t, err) != "custody_attribution_generation_drift" {
		t.Fatalf("admission retry under the re-acquired lease did not hold on the stale lease binding: %v", err)
	}
	if version := stateVersion(); version != 4 {
		t.Fatalf("refused restart retry mutated the route state: %d", version)
	}

	// The BUILD seam is generation-scoped, not fence-scoped: the gate
	// re-validates the binding content and generation currency (still equal
	// after the restart), the write guard enforces the NEW lease, and the
	// narrow authorized advance stamps the binding through its own write. An
	// already-admitted operation therefore stays buildable across a genuine
	// process restart with a re-acquired lease.
	if err := restarted.authorizePhase3BuildOnManifest(ctx, manifest, rpc, id, request, rawEffects, buildCost); err != nil {
		t.Fatalf("build authorization under the re-acquired lease refused: %v", err)
	}
	if version := stateVersion(); version != 5 {
		t.Fatalf("restart build did not advance the generation through its own write: %d", version)
	}
	if rebuilt := persistedAuth(t, id).CustodyProof; rebuilt == nil || rebuilt.Generation != 5 {
		t.Fatalf("restart build did not advance the persisted binding: %+v", rebuilt)
	}

	// Drift refusal for an UNRELATED route-state change, under the CURRENT
	// (restarted) lease: a foreign generation increment leaves the persisted
	// binding behind the route lock. The build gate is generation-bound, so
	// its refusal attributes cleanly to the unrelated bump; the retry
	// admission holds on the lease-identity check regardless (limitation
	// above), so the BUILD refusal is the clean drift signal here.
	if _, err := restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states
		SET state_version=state_version+1, state=jsonb_set(state,'{generation}',to_jsonb((state->>'generation')::bigint+1))
		WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := restarted.authorizePhase3BuildOnManifest(ctx, manifest, rpc, id, request, rawEffects, buildCost); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("build authorized over unrelated route-state drift: %v", err)
	}
	if err := restarted.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, id, retryObservation, decision, planFor(retryObservation.Snapshot)); custodyAttributionHoldReason(t, err) != "custody_attribution_generation_drift" {
		t.Fatalf("retry admission authorized over drifted route state: %v", err)
	}

	// A LEGACY AUTO authorization (admitted before this feature, custody
	// binding absent from the persisted phase3 authorization) must hold at
	// build — the missing legacy proof is the documented backward-compat
	// refusal, produced here by stripping exactly that field from a real
	// admitted row, never by fabricating budget state.
	legacyID, legacyLease, legacyObservation := seed(key + "-legacy")
	legacyProof := custodyAdmissionProofFixture(t, autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key+"-legacy"), key+"-legacy",
		effects, 3_100_000_000, 42, 1, legacyLease.FencingToken, "measured-worker")
	legacyObservation.custodyProof = &legacyProof
	if err := db.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, legacyID, legacyObservation, decision, planFor(legacyObservation.Snapshot)); err != nil {
		t.Fatalf("legacy-route measured admission refused: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations
		SET expected_effects=jsonb_set(expected_effects,'{phase3}',(expected_effects->'phase3') - 'custodyProof')
		WHERE operation_id=$1`, legacyID); err != nil {
		t.Fatal(err)
	}
	// assertBudgetHold enforces exactly the missing-legacy-proof hold.
	assertBudgetHold(t, db.authorizePhase3BuildOnManifest(ctx, manifest, rpc, legacyID, request, rawEffects, buildCost),
		"custody_attribution_proof_missing")
}
