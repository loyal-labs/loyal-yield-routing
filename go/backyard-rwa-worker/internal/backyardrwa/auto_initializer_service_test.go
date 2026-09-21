package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// applyInitializerScopeMigrationFile executes one actual store migration file,
// verbatim, through the simple query protocol. The journal-table constraints
// under proof come from the shipped SQL, never from a test-local re-statement.
func applyInitializerScopeMigrationFile(t *testing.T, ctx context.Context, db *Database, file string) {
	t.Helper()
	raw, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Conn().PgConn().Exec(ctx, string(raw)).ReadAll(); err != nil {
		t.Fatalf("migration %s did not apply: %v", file, err)
	}
}

// assertInitializerAutoScopeMigrationContract proves the applied migration
// pair on a live journal table: the initializer scope is validated and admits
// exactly the three installed lanes plus the reviewed candidate AUTO lane,
// while the engine and remaining lane restrictions from migration 0079 keep
// refusing everything else.
func assertInitializerAutoScopeMigrationContract(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES('initializer-scope-probe','{"generation":1,"cycle":1}')`); err != nil {
		t.Fatal(err)
	}
	var validated bool
	if err := db.pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
	 WHERE conrelid='loyal_yield.multiply_operations'::regclass AND conname='multiply_operations_backyard_initializer_scope'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("initializer scope constraint not validated: %v %t", err, validated)
	}
	for index, row := range []struct {
		engine, lane string
		valid        bool
	}{
		{"backyard_rwa_v1", "Prime/PRIME/USDC", true},
		{"backyard_rwa_v1", SelectedRouteID, true},
		{"backyard_rwa_v1", "OnRe/ONyc/USDC", true},
		{"backyard_rwa_v1", autoAUTOPYUSD.Lane, true},
		{"backyard_rwa_v1", "Ethena/USDe/PYUSD", false},
		{"backyard_rwa_v1", "", false},
		{"earn_max_v2", autoAUTOPYUSD.Lane, false},
	} {
		// A unique operation id per probe and the NOT NULL status: a rejected
		// insert must never be confounded with a primary-key collision, and a
		// valid insert must actually commit the columns the stand-in schema
		// requires.
		_, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,action,engine_version,strategy_key,status)
		 VALUES($1,'initializer-scope-probe','INITIALIZE_KAMINO_OBLIGATION',$2,$3,'decided')`, fmt.Sprintf("initializer-scope-probe-%d", index), row.engine, row.lane)
		if (err == nil) != row.valid {
			t.Fatalf("engine=%s lane=%s admitted=%t err=%v", row.engine, row.lane, err == nil, err)
		}
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key='initializer-scope-probe'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key='initializer-scope-probe'`); err != nil {
		t.Fatal(err)
	}
}

// The shipped initializer migrations keep their exact installed boundaries
// when the candidate AUTO expansion lands after them: same transaction-scoped
// isolated schema as the migration 0079 proof, with 0082 applied verbatim on
// top. Only the initializer scope widens; every other row verdict is
// unchanged.
func TestInitializerAutoScopeMigrationExpandsOnlyInitializerScope(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The verbatim migrations only constrain the journal columns, so a minimal
	// stand-in table under the isolated schema is enough; the full migration
	// chain is applied by the coordinator, not here.
	if _, err = tx.Exec(ctx, `CREATE SCHEMA initializer_auto_scope_test;
	 CREATE TABLE initializer_auto_scope_test.multiply_operations(
	  operation_id text PRIMARY KEY,engine_version text NOT NULL DEFAULT 'backyard_rwa_v1',
	  action text,status text NOT NULL,strategy_key text)`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"0079_backyard_rwa_initializer_actions.sql", "0082_backyard_rwa_initializer_auto_scope.sql"} {
		raw, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, strings.ReplaceAll(string(raw), "loyal_yield.", "initializer_auto_scope_test.")); err != nil {
			t.Fatalf("migration %s did not apply: %v", file, err)
		}
	}
	var validated bool
	if err = tx.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
	 WHERE conrelid='initializer_auto_scope_test.multiply_operations'::regclass AND conname='multiply_operations_backyard_initializer_scope'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("isolated initializer scope constraint not validated: %v %t", err, validated)
	}
	for index, row := range []struct {
		action, engine, lane string
		valid                bool
	}{
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "Prime/PRIME/USDC", true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", SelectedRouteID, true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", autoAUTOPYUSD.Lane, true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "Ethena/USDe/PYUSD", false},
		// PhaseOneLaneID is Prime/PRIME/USDC: already inside the 0079 scope.
		{string(InitializeKaminoObligation), "backyard_rwa_v1", PhaseOneLaneID, true},
		{string(InitializeKaminoObligation), "earn_max_v2", autoAUTOPYUSD.Lane, false},
		{string(OpenRouteStep), "backyard_rwa_v1", "unregistered", false},
		{string(ReportNAV), "backyard_rwa_v1", SelectedRouteID, true},
		{"borrow_debt", "earn_max_v2", "legacy", true},
	} {
		nested, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = nested.Exec(ctx, `INSERT INTO initializer_auto_scope_test.multiply_operations(operation_id,action,engine_version,strategy_key,status) VALUES($1,$2,$3,NULLIF($4,''),'decided')`, fmt.Sprintf("initializer-scope-probe-%d", index), row.action, row.engine, row.lane)
		_ = nested.Rollback(ctx)
		if (err == nil) != row.valid {
			t.Fatalf("action=%s engine=%s lane=%s admitted=%t valid=%t err=%v", row.action, row.engine, row.lane, err == nil, row.valid, err)
		}
	}
}

// openInitializerAutoScopeServiceDatabase builds a dedicated disposable
// database on the already-running disposable Postgres instance and shapes its
// journal from the actual shipped migration files, so the candidate AUTO
// initializer rows below are journaled through the real expanded constraint
// while the shared test database — which stays on migration 0079 — is never
// touched. The database is dropped on cleanup.
func openInitializerAutoScopeServiceDatabase(t *testing.T, name string, timeout time.Duration) (context.Context, context.CancelFunc, *Database) {
	t.Helper()
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
	adminConfig := *config
	adminConfig.ConnConfig = config.ConnConfig.Copy()
	adminConfig.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(context.Background(), &adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name); err != nil {
			t.Errorf("cleanup: drop %s: %v", name, err)
		}
		admin.Close()
	}
	if _, err = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if _, err = admin.Exec(context.Background(), `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	// Register the drop before any further work so even a mid-setup failure
	// never leaves the disposable database behind.
	t.Cleanup(func() { drop() })
	// Substitute the database name in the reviewed URL string itself: the
	// parsed-config serialization does not round-trip the unix-socket DSN.
	serviceURL := strings.Replace(url, "/phase3_budget_test", "/"+name, 1)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	db, err := OpenDatabase(ctx, serviceURL)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// Prove the pool landed on the dedicated database before any schema
	// mutation: the shared test database is never touched by this chain.
	var current string
	if err = db.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil || current != name {
		db.Close()
		cancel()
		t.Fatalf("service pool connected to %q not %q: %v", current, name, err)
	}
	// Close the pool before the drop on every exit — including any setup fatal
	// below — so DROP DATABASE never waits on this test's own connections.
	t.Cleanup(func() {
		db.Close()
		cancel()
		drop()
	})
	if err = ensureManualRecoveryTestSchema(ctx, db); err != nil {
		db.Close()
		cancel()
		t.Fatal(err)
	}
	// The signed-wire fence columns the real nonterminal load reads, in the
	// shipped 0053 shape; the manual-recovery stand-in predates them.
	if _, err = db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
	 ADD COLUMN IF NOT EXISTS signed_wire_sha256 text CHECK (signed_wire_sha256 IS NULL OR signed_wire_sha256 ~ '^[0-9a-f]{64}$'),
	 ADD COLUMN IF NOT EXISTS recent_blockhash text,
	 ADD COLUMN IF NOT EXISTS last_valid_block_height bigint`); err != nil {
		db.Close()
		cancel()
		t.Fatal(err)
	}
	applyInitializerScopeMigrationFile(t, ctx, db, "0079_backyard_rwa_initializer_actions.sql")
	applyInitializerScopeMigrationFile(t, ctx, db, "0082_backyard_rwa_initializer_auto_scope.sql")
	// The position-snapshot journal, with the shipped 0054 columns and the
	// observed-slot uniqueness the insert's conflict target relies on, so the
	// real RecordPositionSnapshot write and its generation fence run against
	// the dedicated database.
	if _, err = db.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS loyal_yield.multiply_position_snapshots(
	 route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states(route_key) ON DELETE CASCADE,
	 generation bigint NOT NULL CHECK (generation > 0),observed_slot bigint NOT NULL CHECK (observed_slot > 0),
	 observed_at timestamptz NOT NULL,strategy_key text,claim_raw numeric(78,0) NOT NULL CHECK (claim_raw >= 0),
	 collateral_raw numeric(78,0) NOT NULL CHECK (collateral_raw >= 0),debt_raw numeric(78,0) NOT NULL CHECK (debt_raw >= 0),
	 equity_usd_micros numeric(78,0),collateral_value_usd_micros numeric(78,0),debt_value_usd_micros numeric(78,0),
	 ltv_bps bigint,forecast_apy_bps bigint,valuation_source text,valuation_slot bigint,valuation_observed_at timestamptz,
	 UNIQUE (route_key, observed_slot))`); err != nil {
		db.Close()
		cancel()
		t.Fatal(err)
	}
	assertInitializerAutoScopeMigrationContract(t, ctx, db)
	return ctx, cancel, db
}

// seedAutoInitializerPilotRoute activates a real pilot budget on a test-owned
// route key with a persisted candidate selector entry — and deliberately no
// operation row and no reservation: both must come from the real producer and
// the real measured admission below.
func seedAutoInitializerPilotRoute(t *testing.T, ctx context.Context, db *Database, key string, price *BudgetPrice, equity int64) {
	t.Helper()
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
	if _, err = db.AcquireRouteLease(ctx, key, "auto-initializer-service", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	storeTestSelectorEntry(t, ctx, db, key, autoSelectorEntryFixture(time.Now().UTC(), equity, price))
}

func upsertConfirmedAccount(accounts *[]ConfirmedAccount, account ConfirmedAccount) {
	for i := range *accounts {
		if (*accounts)[i].Address == account.Address {
			(*accounts)[i] = account
			return
		}
	}
	*accounts = append(*accounts, account)
}

// readyInitializerManifest is the reviewed initializer binding carrying the
// same installed execution-readiness facts the production tick demands before
// dispatch. The reviewed AUTO binding itself is untouched. The allocation
// bridge pin keeps the captured live evidence bytes and digest; the other
// three bridge pins follow the readiness fixture's precedent — their account
// bytes and NormalizedDigest are regenerated together, because the test tree
// holds captured live bytes only for the allocation policy. The returned map
// is the pin bytes the confirmed batch must carry.
func readyInitializerManifest(t *testing.T) (RouteManifest, map[string][]byte) {
	t.Helper()
	manifest := autoInitializerFixtureManifest(t)
	manifest.Status = "ready"
	manifest.Unresolved = nil
	stringPtr := func(value string) *string { return &value }
	if manifest.PolicyCatalog.SHA256 == nil {
		manifest.PolicyCatalog.SHA256 = stringPtr(strings.Repeat("c", 64))
	}
	for index := range manifest.PolicyCatalog.Policies {
		if manifest.PolicyCatalog.Policies[index].DataSHA256 == nil {
			manifest.PolicyCatalog.Policies[index].DataSHA256 = stringPtr(strings.Repeat("def0"[index%4:], 16))
		}
	}
	pinBytes := map[string][]byte{autoInitializerFixturePolicy: []byte(autoInitializerFixtureSyntheticAccountData)}
	for index := range manifest.RuntimeBindings.BridgePolicies {
		entry := &manifest.RuntimeBindings.BridgePolicies[index]
		if entry.Action == VoltrAllocateToSquads {
			if entry.NormalizedDigest != strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads] {
				t.Fatal("embedded allocation bridge digest drifted from the captured policy evidence")
			}
			pinBytes[entry.Account] = liveSquadsPolicy152Bytes(t)
			continue
		}
		data := autoReadinessAccountBytes(byte(0x30+index), 1548)
		entry.NormalizedDigest = autoMaskedDigest(t, data, entry.MaskedByteRanges)
		pinBytes[entry.Account] = data
	}
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("candidate execution readiness facts incomplete: %v", blocker)
	}
	return manifest, pinBytes
}

// autoInitializerServiceRPC serves the chain methods the initializer chain
// reads, with the candidate prestate filled only after the real preparation
// has produced its measured request. The transport refuses every broadcast:
// the proof is the durable reservation, never a send.
func autoInitializerServiceRPC(t *testing.T) (*RPCClient, map[string]ConfirmedAccount, *int, *bool) {
	t.Helper()
	rpc, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	prestate := map[string]ConfirmedAccount{}
	sends, expired := 0, false
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     any               `json:"id"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		serve := func(result any) *http.Response {
			encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header)}
		}
		switch body.Method {
		case "getLatestBlockhash":
			return serve(map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": bridgeVault, "lastValidBlockHeight": 99}}), nil
		case "getSlot":
			return serve(int64(42)), nil
		case "getFeeForMessage":
			return serve(map[string]any{"context": map[string]int{"slot": 42}, "value": uint64(5000)}), nil
		case "getBlockHeight":
			height := int64(99)
			if expired {
				height = 100
			}
			return serve(height), nil
		case "getMinimumBalanceForRentExemption":
			var size int
			if err = json.Unmarshal(body.Params[0], &size); err != nil {
				return nil, err
			}
			return serve(uint64((128 + size) * 3480 * 2)), nil
		case "getMultipleAccounts":
			var addresses []string
			if err = json.Unmarshal(body.Params[0], &addresses); err != nil {
				return nil, err
			}
			if err = json.Unmarshal(body.Params[0], &addresses); err != nil {
				return nil, err
			}
			values := make([]any, len(addresses))
			for index, address := range addresses {
				if account, ok := prestate[address]; ok && !(account.Owner == "" && account.Lamports == 0 && len(account.Data) == 0) {
					values[index] = map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": account.Executable,
						"data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
				}
			}
			return serve(map[string]any{"context": map[string]int{"slot": 42}, "value": values}), nil
		case "sendTransaction":
			sends++
			return &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Header: make(http.Header)}, nil
		case "getProgramAccounts":
			// No open Voltr withdrawal receipts in this fixture chain.
			return serve(map[string]any{"context": map[string]int{"slot": 42}, "value": []any{}}), nil
		case "getSignatureStatuses":
			return serve(map[string]any{"context": map[string]int{"slot": 42}, "value": []any{nil}}), nil
		case "simulateTransaction":
			t.Fatal("authorization must not simulate: no signer exists in this chain")
			return nil, nil
		default:
			t.Fatalf("unexpected RPC %s", body.Method)
			return nil, nil
		}
	})
	return rpc, prestate, &sends, &expired
}

// The candidate lane joins the same confirmed batch on both sides of the
// observation seam: the requested address inventory and the ownership scan
// are scoped by the same reviewed manifest, the installed closure is
// unchanged, and a simultaneously funded candidate and Maple tranche holds
// instead of picking one.
func TestCandidateObservationInventoryCoversTheCandidateLane(t *testing.T) {
	candidate := autoInitializerFixtureManifest(t)
	maplePreferred := candidate
	maplePreferred.selectorObservation = true
	maplePreferred.observationLane = SelectedRouteID
	requested := map[string]bool{}
	for _, address := range routeFixedAddresses(maplePreferred) {
		requested[address] = true
	}
	if !requested[autoAUTOPYUSD.Kamino.Obligation] || !requested[autoAUTOPYUSD.CollateralCustody] {
		t.Fatal("preferred-Maple batch inventory misses the candidate AUTO ownership accounts")
	}
	installed := RouteManifest{}
	installed.selectorObservation = true
	installed.observationLane = SelectedRouteID
	closed := map[string]bool{}
	for _, address := range routeFixedAddresses(installed) {
		closed[address] = true
	}
	if closed[autoAUTOPYUSD.Kamino.Obligation] || closed[autoAUTOPYUSD.CollateralCustody] {
		t.Fatal("installed inventory grew without a reviewed binding")
	}

	_, _, accounts := autoObservationBatch(t, 77, nil)
	// The installed lanes join the batch exactly as the production inventory
	// requests them: a present obligation envelope and a valid zero custody.
	for _, lane := range selectorLanes {
		laneRoute, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: laneRoute.Kamino.Obligation})
		upsertConfirmedAccount(&accounts, tokenAccountFixture(t, laneRoute.CollateralCustody, laneRoute.Kamino.CollateralMint, bridgeVault, 0))
	}
	// Candidate-only ownership selects the candidate lane even with a Maple
	// preference.
	selected, err := observedSelectorRouteForManifest(accounts, SelectedRouteID, candidate)
	if err != nil || selected.Lane != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate ownership was not selected: %q %v", selected.Lane, err)
	}
	// The same batch and preference through the installed closure refuses the
	// candidate lane instead.
	if _, err = observedSelectorRouteForManifest(accounts, autoAUTOPYUSD.Lane, RouteManifest{}); err == nil {
		t.Fatal("installed scan admitted a candidate preference")
	}
	// Combined candidate and Maple exposure holds instead of picking one.
	mapleRoute, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	upsertConfirmedAccount(&accounts, kaminoObligationImage(t, mapleRoute, 77, autoFixtureDepositReceiptRaw, autoFixtureDebtRaw))
	if _, err = observedSelectorRouteForManifest(accounts, SelectedRouteID, candidate); err == nil || !strings.Contains(err.Error(), "multiple_selector_lanes_have_exposure") {
		t.Fatalf("combined candidate and Maple exposure did not hold: %v", err)
	}
}

// The whole candidate initializer service path runs through the same real
// routines as the installed lanes, on a journal shaped by the actual
// migration pair: the real production observation merge (planning read,
// confirmed AUTO batch, journal and identity enrichment over the real
// database) produces the exact initializer decision from the persisted
// candidate entry, records it durably through the real expanded scope
// constraint, prepares the measured request, and the real measured admission
// — never a seeded row — creates the bounded AUTO reservation and allocates
// the persisted entry, with the service tick stopping at the reported build
// boundary (doc21/doc22 and the recovery suite cover the downstream build,
// send and recovery stages on their own fixtures). Every public embedded
// entrypoint keeps the candidate closed on identical durable state.
func TestAutoInitializerServicePathThroughRealInitializerScopeMigration(t *testing.T) {
	const observationSlot = int64(42)
	const candidateEquity = int64(400_000)
	manifest, initializerPinBytes := readyInitializerManifest(t)
	_, _, accounts := autoObservationBatch(t, observationSlot, func(batch []ConfirmedAccount) {
		flattenAutoPosition(batch)
		// The initializer's empty-obligation precondition, in the production
		// wire form of an absent optional account.
		upsertConfirmedAccount(&batch, ConfirmedAccount{Address: autoAUTOPYUSD.Kamino.Obligation})
		// Entry capacity: reserve deposit and borrow limits with headroom.
		binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:kaminoReserveConfigOffset+168], 100_000_000_000)
		binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+168:kaminoReserveConfigOffset+176], 1_000_000_000)
		// Idle candidate funding and an empty Squads cash lane keep the flat
		// state the initializer demands. The Voltr book identity moves with the
		// idle balance: total value = idle + prior reported NAV (42) + tracked 0.
		// The report ticket sits exactly at the activation's archived flat
		// baseline: nothing has been consumed since.
		binary.LittleEndian.PutUint64(accountAt(batch, bridgeIdleATA).Data[64:72], 2_000_000)
		binary.LittleEndian.PutUint64(accountAt(batch, bridgeVoltrVault).Data[168:176], 53-11+2_000_000)
		binary.LittleEndian.PutUint64(accountAt(batch, bridgeSquadsATA).Data[64:72], 0)
		binary.LittleEndian.PutUint64(accountAt(batch, reportTicketPDA).Data[48:56], 0)
		// The strategy receipt reports the just-now book the real chain books at
		// rest: the production observer compares this ts against wall clock, so
		// the fixture receipt must carry a current instant, not the frozen
		// fixture-chain clock.
		binary.LittleEndian.PutUint64(accountAt(batch, bridgeStrategyReceipt).Data[112:120], uint64(time.Now().Unix()))
	})
	// The installed lanes join the confirmed batch exactly as the production
	// inventory requests them — present obligation envelopes, valid zero
	// custody — so the manifest-scoped scan sees the same batch the transport
	// serves.
	for _, lane := range selectorLanes {
		laneRoute, laneErr := runtimeRoute(lane)
		if laneErr != nil {
			t.Fatal(laneErr)
		}
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: laneRoute.Kamino.Obligation})
		upsertConfirmedAccount(&accounts, tokenAccountFixture(t, laneRoute.CollateralCustody, laneRoute.Kamino.CollateralMint, bridgeVault, 0))
	}
	// The five reviewed policy pins this manifest digests — the candidate
	// policy plus the four bridge policies — added outside the batch builder
	// so the append is visible to the caller.
	for address, data := range initializerPinBytes {
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: address, Owner: bridgeSquadsProgram, Lamports: 1, Data: append([]byte(nil), data...)})
	}
	// The rest of the production fetch inventory: the inactive lanes' protocol
	// internals and every runtime policy account. Only the active lane's pins
	// are digest-checked; the inactive lanes stay flat, so their accounts ride
	// the batch as present, flat identities exactly as the scan expects.
	peg := new(big.Int).Lsh(big.NewInt(1), 60)
	for _, lane := range selectorLanes {
		laneRoute, laneErr := runtimeRoute(lane)
		if laneErr != nil {
			t.Fatal(laneErr)
		}
		upsertConfirmedAccount(&accounts, marketFixture(t, laneRoute.Kamino.Market))
		upsertConfirmedAccount(&accounts, kaminoReserveImage(t, laneRoute.Kamino.Market, laneRoute.Kamino.CollateralReserve, laneRoute.Kamino.CollateralMint, observationSlot, peg, 1_000_000, 0, 1_000_000, 6))
		upsertConfirmedAccount(&accounts, kaminoReserveImage(t, laneRoute.Kamino.Market, laneRoute.Kamino.DebtReserve, laneRoute.Kamino.DebtMint, observationSlot, peg, 1_000_000, 0, 1_000_000, 6))
		upsertConfirmedAccount(&accounts, tokenAccountFixture(t, laneRoute.CollateralLiquiditySupply, laneRoute.Kamino.CollateralMint, laneRoute.Kamino.MarketAuthority, 1_000_000))
		upsertConfirmedAccount(&accounts, tokenAccountFixture(t, laneRoute.DebtLiquiditySupply, laneRoute.Kamino.DebtMint, laneRoute.Kamino.MarketAuthority, 1_000_000))
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: laneRoute.DebtFeeReceiver, Owner: "11111111111111111111111111111111", Lamports: 1})
	}
	for address := range manifest.runtimePolicyObservationSet() {
		if accountAt(accounts, address).Address == address {
			continue
		}
		accounts = append(accounts, ConfirmedAccount{Address: address, Owner: "11111111111111111111111111111111", Lamports: 1})
	}
	rpc, prestate, _, _ := autoInitializerServiceRPC(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_doc23_service_test", 120*time.Second)
	defer cancel()
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	// The primary candidate route: real pilot activation, real lease, real
	// persisted entry — and deliberately no operation row and no reservation:
	// both must come from the production producers below.
	seedAutoInitializerPilotRoute(t, ctx, db, productionRouteKey, &price, candidateEquity)
	policyHash := sha256Bytes([]byte("policies"))
	embedded, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	// The transport serves exactly this confirmed batch, so the production
	// observation and the initializer preparation read the same accounts.
	for _, account := range accounts {
		prestate[account.Address] = account
	}
	batchImage := map[string]ConfirmedAccount{}
	for _, account := range accounts {
		batchImage[account.Address] = account
	}

	// Production observation, with one explicitly reported narrower seam: the
	// M6 program-identity watcher verifies the pinned mainnet ProgramData image
	// hash, which no local fixture can serve, so the identity observation stays
	// pinned exactly as in every other producer test in this suite. The batch
	// closure below is productionTickRuntime's own: the real planning read
	// through the candidate manifest — the persisted candidate entry's lane must
	// resolve as the observation route — and the real confirmed-batch RPC
	// observer over the transport above.
	state := productionObserveState{
		manifest: manifest, routeKey: productionRouteKey, journal: db,
		batch: func(ctx context.Context) (Observation, error) {
			planning, err := db.readRoutePlanningStateOnManifest(ctx, manifest, productionRouteKey, true)
			if err != nil {
				return Observation{}, err
			}
			observation, err := ObserveConfirmedRouteSnapshot(ctx, rpc, planning.observationManifest(manifest))
			if err != nil {
				return Observation{}, err
			}
			observation.planning = planning
			return observation, nil
		},
		identity: pinnedIdentityObservation,
	}
	// productionTickRuntime wiring for every closure the tick uses; only the
	// observe-bound wrappers and the build gate bind to the seams reported
	// above — build stops at the pinned-signer boundary, never a broadcast.
	rt := productionTickRuntime(db, rpc, manifest)
	rt.observe = state.observe
	rt.prepareInitialization = func(ctx context.Context, m RouteManifest, dec Decision) (Observation, KaminoInitializationRequest, error) {
		return prepareKaminoInitialization(ctx, rpc, m, dec, state.observe)
	}
	rt.recordDecision = func(ctx context.Context, key string, obs Observation, dec Decision, manifestSHA256, policyCatalogSHA256 string) (DecisionRecord, error) {
		return db.RecordDecisionOnManifest(ctx, manifest, key, obs, dec, manifestSHA256, policyCatalogSHA256)
	}
	buildGatePending := errors.New("initializer build gate stops before the pinned policy signer")
	rt.buildInitialization = func(context.Context, string, KaminoInitializationRequest) error {
		return buildGatePending
	}
	o, err := rt.observe(ctx)
	if err != nil {
		t.Fatal("candidate observation through the production merge", err)
	}
	if !o.Snapshot.Fresh || o.Snapshot.RouteLane != autoAUTOPYUSD.Lane || !o.Snapshot.PilotActive ||
		!o.Snapshot.ObligationPresenceKnown || o.Snapshot.ObligationPresent || o.Snapshot.HasPosition ||
		!o.Snapshot.InitializationPolicyReady || !o.Snapshot.PolicyReady || !o.Snapshot.ExitBuildable ||
		o.Snapshot.SelectorEntryEquityRaw != candidateEquity || o.Snapshot.StrategyNAVRaw != 0 ||
		o.Snapshot.CapacityRaw <= 0 || o.Snapshot.Nonterminal != "" {
		t.Fatalf("real candidate observation did not reach initializer readiness: %+v", o.Snapshot)
	}

	// The candidate observation resolves to the exact initializer decision only
	// through a manifest whose initializer binding resolves — named for both
	// states: the explicit absent fixture (the shipped pre-install state)
	// keeps the decision producer closed, while the embedded manifest's
	// installed binding produces the identical decision the reviewed
	// initializer fixture produces.
	d := manifest.DecideOnManifest(o.Snapshot)
	if d.Action != InitializeKaminoObligation || d.Reason != "multiply_obligation_missing" || d.AmountRaw != 0 || d.StrategyKey != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate decision producer drifted: %+v snapshot: %+v", d, o.Snapshot)
	}
	if got := autoAbsentBindingManifest(t).DecideOnManifest(o.Snapshot); got.Action == InitializeKaminoObligation {
		t.Fatalf("absent binding admitted the candidate decision: %+v", got)
	}
	if installedDecision := requireEmbeddedInstalledBinding(t).DecideOnManifest(o.Snapshot); installedDecision != d {
		t.Fatalf("installed decision producer drifted from the reviewed one: %+v vs %+v", installedDecision, d)
	}
	if err = d.Validate(); err == nil {
		t.Fatal("embedded decision validation admitted the candidate lane")
	}
	if err = manifest.validateDecision(d); err != nil {
		t.Fatal(err)
	}
	// Persistence is manifest-scoped: the public RecordDecision wrapper only
	// resolves the embedded manifest and forwards to RecordDecisionOnManifest.
	// Both binding states, named: the explicit absent fixture (the shipped
	// pre-install state) rejects the candidate decision before any row is
	// inserted, and the installed admission is exercised by the real scoped
	// decision recording later in this lifecycle.
	if _, err = db.RecordDecisionOnManifest(ctx, autoAbsentBindingManifest(t), productionRouteKey, o, d, manifest.SHA256, policyHash); err == nil {
		t.Fatal("absent binding decision persistence admitted the candidate lane")
	}

	// Real measured preparation through the production closure: a first pass
	// proves the request identity, the candidate prestate for that exact request
	// is then served, and the real preparation succeeds against the same
	// transport.
	_, r, probeErr := rt.prepareInitialization(ctx, manifest, d)
	if probeErr == nil {
		t.Fatal("preparation succeeded before its candidate prestate was served")
	}
	if r.RouteLane != autoAUTOPYUSD.Lane || r.RecentBlockhash != bridgeVault || r.RentLamports == 0 || r.MaximumFeeLamports != 5000 {
		t.Fatalf("measured preparation did not produce the candidate request: %+v (%v)", r, probeErr)
	}
	for address, account := range autoInitializerPrestateAccounts(t, r) {
		prestate[address] = account
	}
	// The preparation prestate must not clobber the confirmed batch bytes the
	// observer decodes: only the accounts the initializer validation itself
	// reads keep their preparation form; everything the batch proves is
	// restored, so the next real observation still decodes its reserves,
	// custody and vault bytes.
	for address, account := range batchImage {
		switch address {
		case bridgeSettings, autoInitializerFixturePolicy, bridgeVault, bridgeDelegate,
			autoAUTOPYUSD.Kamino.CollateralMint, autoAUTOPYUSD.Kamino.DebtMint:
			continue
		}
		prestate[address] = account
	}
	const rentAddress = "SysvarRent111111111111111111111111111111111"
	rent := prestate[rentAddress]
	binary.LittleEndian.PutUint64(rent.Data, r.RentLamports/(kaminoObligationLength+128))
	prestate[rentAddress] = rent
	// The build-cost native valuation read (ObserveNativeSOLBudgetPrice) adds
	// the wSOL reserve price batch. Every account the confirmed batch already
	// publishes — the clock, the USDC reference reserve and mints — keeps its
	// batch bytes, and the SOL price timestamp agrees with the fixture chain
	// clock those bytes carry, so the re-observation and the valuation read see
	// one coherent chain.
	solReserve := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, observationSlot, new(big.Int).Lsh(big.NewInt(1), 60+7), 1, 1)
	putKey(t, solReserve.Data[32:64], budgetSOLMarket)
	binary.LittleEndian.PutUint64(solReserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(solReserve.Data[264:272], uint64(kaminoFixtureUnix))
	prestate[budgetSOLReserve] = solReserve
	splMint := func(address string, decimals byte) ConfirmedAccount {
		data := make([]byte, 82)
		data[44], data[45] = decimals, 1
		return ConfirmedAccount{Address: address, Owner: classicTokenProgram, Data: data}
	}
	if _, ok := prestate[budgetWrappedSOLMint]; !ok {
		prestate[budgetWrappedSOLMint] = splMint(budgetWrappedSOLMint, 9)
	}
	if _, ok := prestate[bridgeUSDC]; !ok {
		prestate[bridgeUSDC] = splMint(bridgeUSDC, 6)
	}
	_, r, err = rt.prepareInitialization(ctx, manifest, d)
	if err != nil {
		t.Fatal("measured preparation with the candidate prestate", err)
	}

	// Real service tick — productionTickRuntime's remaining closures exactly as
	// installed: observe -> decide -> record -> prepare -> measured admission,
	// stopping only at the reported signer boundary.
	w := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: rt}
	if err = w.Tick(ctx); err == nil || err.Error() != buildGatePending.Error() {
		t.Fatalf("service tick did not stop at the signer boundary: %v", err)
	}
	// The producer chose the journal identity: query the actually recorded row
	// instead of assuming its format, and require it to be the route's only
	// decided initializer.
	var ids []string
	idRows, err := db.pool.Query(ctx, `SELECT operation_id FROM loyal_yield.multiply_operations
	 WHERE route_key=$1 AND status='decided' AND action=$2`, productionRouteKey, InitializeKaminoObligation)
	if err != nil {
		t.Fatal(err)
	}
	for idRows.Next() {
		var recorded string
		if err = idRows.Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, recorded)
	}
	idRows.Close()
	if len(ids) != 1 {
		t.Fatalf("expected exactly one decided candidate initializer, got %v", ids)
	}
	id := ids[0]
	var status, action, lane string
	if err = db.pool.QueryRow(ctx, `SELECT status,COALESCE(action,''),COALESCE(strategy_key,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &action, &lane); err != nil {
		t.Fatal(err)
	}
	if status != "decided" || action != string(InitializeKaminoObligation) || lane != autoAUTOPYUSD.Lane {
		t.Fatalf("producer did not journal the candidate decision: %s %s %s", status, action, lane)
	}

	// The reservation exists because the actual measured admission created it.
	var encodedBudget, encodedAuth []byte
	if err = db.pool.QueryRow(ctx, `SELECT s.state->'phase3',o.expected_effects->'phase3' FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE o.operation_id=$1`, id).Scan(&encodedBudget, &encodedAuth); err != nil {
		t.Fatal(err)
	}
	var budget Phase3Budget
	var auth phase3OperationAuthorization
	if json.Unmarshal(encodedBudget, &budget) != nil || json.Unmarshal(encodedAuth, &auth) != nil {
		t.Fatal("admission state decode")
	}
	cost := auth.BridgeAdmission.CurrentCost
	reservation := budget.Reservations[id]
	if budget.Pilot == nil || reservation.Family != "AUTO" || reservation.Recovery || reservation.ExitAfterMicros != 0 ||
		reservation.UpperMicros != cost.TotalMicros || reservation.ExecutionCostUpperMicros != cost.NetworkFeeMicros ||
		cost.SetupLamports == 0 || auth.PilotAuthorityID != pilotBudgetAuthorityID {
		t.Fatalf("measured admission lost its bounds: %+v cost=%+v", reservation, cost)
	}
	// The initializer is the account-setup leg: admission refuses a second
	// allocation but never binds allocationOperationId — that bind belongs to
	// the funding operation (selector_entry.go:465-487) — so the actual
	// recorded entry keeps the candidate lane, unallocated.
	var entryLane, allocationID string
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(state->'selectorEntry'->>'lane',''),COALESCE(state->'selectorEntry'->>'allocationOperationId','') FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&entryLane, &allocationID); err != nil {
		t.Fatal(err)
	}
	if entryLane != autoAUTOPYUSD.Lane || allocationID != "" {
		t.Fatalf("initializer admission drifted the persisted entry: lane=%q allocation=%q", entryLane, allocationID)
	}
	// Admission is idempotent through the existing authorization.
	if err = db.admitKaminoInitialization(ctx, rpc, manifest, id, o, d, r); err != nil {
		t.Fatal("admission replay", err)
	}

	// Installed-closure and hold regressions on the producer and admission.
	for _, closure := range []struct {
		name string
		mut  func(*Snapshot)
	}{
		{"unknown-obligation", func(s *Snapshot) { s.ObligationPresenceKnown = false }},
		{"present-obligation", func(s *Snapshot) { s.ObligationPresent = true }},
		{"withdrawal-demand", func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }},
		{"non-flat-state", func(s *Snapshot) { s.PositionDebtRaw = 1 }},
		{"pending-operation", func(s *Snapshot) { s.Nonterminal = Signed }},
		{"stale-observation", func(s *Snapshot) { s.Fresh = false }},
	} {
		s := o.Snapshot
		closure.mut(&s)
		if held := manifest.DecideOnManifest(s); held.Action == InitializeKaminoObligation {
			t.Fatalf("%s still produced the initializer: %+v", closure.name, held)
		}
	}
	drifted := r
	drifted.PolicySeed = autoFixtureSeed
	assertBudgetHold(t, manifest.validateInitializationRequest(drifted), "initializer_request_manifest_mismatch")
	// Both binding states on the request builder: the explicit absent fixture
	// (the shipped pre-install state) stays held, while the embedded
	// manifest's installed binding resolves its own installed request.
	if _, absentErr := autoAbsentBindingManifest(t).initializationRequest(autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}, r.RentLamports, 1); absentErr == nil {
		t.Fatal("absent binding resolved the AUTO initializer request")
	}
	if installedRequest, installedErr := embedded.initializationRequest(autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}, r.RentLamports, 1); installedErr != nil || installedRequest.PolicySeed != installedAutoPolicySeed {
		t.Fatalf("installed manifest did not resolve its own initializer request: %+v %v", installedRequest, installedErr)
	}

	// An expired quote refuses the measured admission itself, leaving no
	// reservation behind.
	expiredKey := "auto-init-expired-" + time.Now().Format("150405.000000000")
	seedAutoInitializerPilotRoute(t, ctx, db, expiredKey, &price, candidateEquity)
	slotExpired := autoSelectorEntryFixture(time.Now().UTC(), candidateEquity, &price)
	slotExpired.Quote.SampleSlot, slotExpired.Quote.ValidThroughSlot = 30, 41
	earlyPrice := price
	earlyPrice.ObservedSlot, earlyPrice.ValidThroughSlot = 30, 30+budgetMaxObservationLagSlots
	slotExpired.Quote.DebtPrice = &earlyPrice
	storeTestSelectorEntry(t, ctx, db, expiredKey, slotExpired)
	expiredRecord, err := db.RecordDecisionOnManifest(ctx, manifest, expiredKey, o, d, manifest.SHA256, policyHash)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.admitKaminoInitialization(ctx, rpc, manifest, expiredRecord.OperationID, o, d, r), "selector_entry_quote_expired")
	var expiredReservations int
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE((SELECT count(*)::int FROM jsonb_object_keys(state->'phase3'->'reservations')),0) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, expiredKey).Scan(&expiredReservations); err != nil {
		t.Fatal(err)
	}
	if expiredReservations != 0 {
		t.Fatalf("refused admission left a reservation behind: %d", expiredReservations)
	}
}
