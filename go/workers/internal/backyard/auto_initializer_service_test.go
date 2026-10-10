package backyard

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

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// applyInitializerScopeMigrationFile executes one actual store migration file,
// verbatim, through the simple query protocol. The journal-table constraints
// under proof come from the shipped SQL, never from a test-local re-statement.
func applyInitializerScopeMigrationFile(t *testing.T, ctx context.Context, db *Database, file string) {
	t.Helper()
	raw, err := os.ReadFile("../../../../migrations/yield/" + file)
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
		raw, err := os.ReadFile("../../../../migrations/yield/" + file)
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
// without changing the shared test database. The database is dropped on cleanup.
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
	// The build, signed-wire and reconciliation columns the real lifecycle
	// reads and writes; the manual-recovery stand-in predates them.
	if _, err = db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
	 ADD COLUMN IF NOT EXISTS message_sha256 text CHECK (message_sha256 IS NULL OR message_sha256 ~ '^[0-9a-f]{64}$'),
	 ADD COLUMN IF NOT EXISTS signed_wire_sha256 text CHECK (signed_wire_sha256 IS NULL OR signed_wire_sha256 ~ '^[0-9a-f]{64}$'),
	 ADD COLUMN IF NOT EXISTS recent_blockhash text,
	 ADD COLUMN IF NOT EXISTS last_valid_block_height bigint,
	 ADD COLUMN IF NOT EXISTS confirmation_status text,
	 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,
	 ADD COLUMN IF NOT EXISTS reconciled_effects jsonb`); err != nil {
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

// seedAutoInitializerPilotRoute seeds a test-owned route key with a persisted
// candidate selector entry — and deliberately no operation row: it must come
// from the real producer and the real bind below.
func seedAutoInitializerPilotRoute(t *testing.T, ctx context.Context, db *Database, key string, price *BudgetPrice, equity int64) {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":2}',2)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "auto-initializer-service", 5*time.Minute); err != nil {
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

// readyInitializerManifest is the embedded manifest with the installed
// execution-readiness facts the production tick demands before dispatch.
func readyInitializerManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest := embeddedTestManifest(t)
	manifest.Status = "ready"
	manifest.Unresolved = nil
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("candidate execution readiness facts incomplete: %v", blocker)
	}
	return manifest
}

// autoInitializerServiceRPC serves the chain methods the initializer chain
// reads, with the candidate prestate filled only after the real preparation
// has produced its measured request. The transport refuses every broadcast:
// the proof is the durable reservation, never a send.
func autoInitializerServiceRPC(t *testing.T) (*chain.Client, map[string]ConfirmedAccount, *int, *bool) {
	t.Helper()
	rpc := newFakeChain(t, nil)
	prestate := map[string]ConfirmedAccount{}
	sends, expired := 0, false
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
		case "getEpochInfo":
			height := int64(99)
			if expired {
				height = 100
			}
			return serve(finalizedEpoch(height)), nil
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
			if squadsProgramAccounts(body.Params) {
				return serve(capturedPolicyProgramAccounts(42)), nil
			}
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
	candidate := embeddedTestManifest(t)
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
// constraint, prepares the measured request, and the real bind
// — never a seeded row — records its intent against
// the persisted entry, with the service tick stopping at the reported build
// boundary (doc21/doc22 and the recovery suite cover the downstream build,
// send and recovery stages on their own fixtures). Every public embedded
// entrypoint keeps the candidate closed on identical durable state.
func TestAutoInitializerServicePathThroughRealInitializerScopeMigration(t *testing.T) {
	const observationSlot = int64(42)
	const candidateEquity = int64(400_000)
	manifest := readyInitializerManifest(t)
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
		binary.LittleEndian.PutUint16(accountAt(batch, bridgeVoltrVault).Data[514:516], uint16(approvedAdminPerformanceFeeBPS))
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
	// The rest of the production fetch inventory: the inactive lanes' protocol
	// internals. The inactive lanes stay flat, so their accounts ride the
	// batch as present, flat identities exactly as the scan expects.
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
	rpc, prestate, _, _ := autoInitializerServiceRPC(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_doc23_service_test", 120*time.Second)
	defer cancel()
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	// The primary candidate route: real pilot activation, real lease, real
	// persisted entry — and deliberately no operation row and no reservation:
	// both must come from the production producers below.
	seedAutoInitializerPilotRoute(t, ctx, db, productionRouteKey, &price, candidateEquity)
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
	view, err := OpenView(ctx, rpc, nil)
	if err != nil {
		t.Fatal(err)
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
			observation, _, err := ObserveConfirmedRouteSnapshot(ctx, rpc, view, planning.observationManifest(manifest), planning.landedSlot)
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
	rt := productionTickRuntime(db, rpc, view, manifest, Credentials{})
	rt.observe = state.observe
	rt.prepareInitialization = func(ctx context.Context, m RouteManifest, dec Decision) (Observation, KaminoInitializationRequest, error) {
		return prepareKaminoInitialization(ctx, rpc, view, m, dec, state.observe)
	}
	rt.recordDecision = func(ctx context.Context, key string, obs Observation, dec Decision, manifestSHA256 string) (DecisionRecord, error) {
		return db.RecordDecisionOnManifest(ctx, manifest, key, obs, dec, manifestSHA256)
	}
	buildGatePending := errors.New("initializer build gate stops before the pinned policy signer")
	rt.buildInitialization = func(context.Context, string, KaminoInitializationRequest) error {
		return buildGatePending
	}
	o, err := rt.observe(ctx)
	if err != nil {
		t.Fatal("candidate observation through the production merge", err)
	}
	if !o.Snapshot.Fresh || o.Snapshot.RouteLane != autoAUTOPYUSD.Lane ||
		!o.Snapshot.ObligationPresenceKnown || o.Snapshot.ObligationPresent || o.Snapshot.HasPosition ||
		o.Snapshot.SelectorEntryEquityRaw != candidateEquity || o.Snapshot.StrategyNAVRaw != 0 ||
		o.Snapshot.CapacityRaw <= 0 || o.Snapshot.Nonterminal != "" {
		t.Fatalf("real candidate observation did not reach initializer readiness: %+v", o.Snapshot)
	}

	// The candidate observation resolves to the exact initializer decision
	// through the manifest.
	d := manifest.DecideOnManifest(o.Snapshot)
	if d.Action != InitializeKaminoObligation || d.Reason != "multiply_obligation_missing" || d.AmountRaw != 0 || d.StrategyKey != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate decision producer drifted: %+v snapshot: %+v", d, o.Snapshot)
	}
	if installedDecision := embeddedTestManifest(t).DecideOnManifest(o.Snapshot); installedDecision != d {
		t.Fatalf("installed decision producer drifted from the reviewed one: %+v vs %+v", installedDecision, d)
	}
	if err = d.Validate(); err == nil {
		t.Fatal("embedded decision validation admitted the candidate lane")
	}
	if err = manifest.validateDecision(d); err != nil {
		t.Fatal(err)
	}
	// Persistence is manifest-scoped: the public RecordDecision wrapper only
	// resolves the embedded manifest and forwards to RecordDecisionOnManifest;
	// the scoped admission is exercised by the real decision recording later
	// in this lifecycle.

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
		case bridgeSettings, bridgeVault, bridgeDelegate,
			autoAUTOPYUSD.Kamino.CollateralMint, autoAUTOPYUSD.Kamino.DebtMint:
			continue
		}
		prestate[address] = account
	}
	const rentAddress = "SysvarRent111111111111111111111111111111111"
	rent := prestate[rentAddress]
	binary.LittleEndian.PutUint64(rent.Data, r.RentLamports/(kamino.ObligationSize+128))
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
		return ConfirmedAccount{Address: address, Owner: classicTokenProgram, Lamports: 1, Data: data}
	}
	if _, ok := prestate[budgetWrappedSOLMint]; !ok {
		prestate[budgetWrappedSOLMint] = splMint(budgetWrappedSOLMint, 9)
	}
	if _, ok := prestate[bridgeUSDC]; !ok {
		prestate[bridgeUSDC] = splMint(bridgeUSDC, 6)
	}
	// The view takes its start-up read again, now with the candidate prestate.
	if err = view.seed(ctx); err != nil {
		t.Fatal(err)
	}
	_, r, err = rt.prepareInitialization(ctx, manifest, d)
	if err != nil {
		t.Fatal("measured preparation with the candidate prestate", err)
	}

	// Real service tick — productionTickRuntime's remaining closures exactly as
	// installed: observe -> decide -> record -> prepare -> bind,
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

	// The bind recorded exactly the prepared initializer request.
	var encodedAuth []byte
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&encodedAuth); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encodedAuth, &auth) != nil || auth.BuildInput == nil || auth.BuildInput.Kind != "kamino-initialize" {
		t.Fatalf("bind lost the initializer request: %s", encodedAuth)
	}
	if err = db.requireBoundIntent(ctx, id, r, auth.BuildInput.Effects); err != nil {
		t.Fatal("bound intent is not the prepared request", err)
	}
	// The initializer is the account-setup leg: the bind refuses a second
	// allocation but never binds allocationOperationId — that bind belongs to
	// the funding operation (selector_entry.go:465-487) — so the actual
	// recorded entry keeps the candidate lane, unallocated.
	var entryLane, allocationID string
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(state->'selectorEntry'->>'lane',''),COALESCE(state->'selectorEntry'->>'allocationOperationId','') FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&entryLane, &allocationID); err != nil {
		t.Fatal(err)
	}
	if entryLane != autoAUTOPYUSD.Lane || allocationID != "" {
		t.Fatalf("initializer bind drifted the persisted entry: lane=%q allocation=%q", entryLane, allocationID)
	}
	// A bound row is never bound again.
	assertBudgetHold(t, db.bindOperation(ctx, rpc, view, manifest, id, o, d, r, kaminoInitializationEffects(r)), "bind_journal_mismatch")

	// Installed-closure and hold regressions on the producer and bind.
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
	// The request builder resolves the installed AUTO policy.
	if installedRequest, installedErr := embedded.initializationRequest(testPolicies(t), autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}, r.RentLamports, 1); installedErr != nil || installedRequest.Policy != installedAutoPolicyKey {
		t.Fatalf("installed manifest did not resolve its own initializer request: %+v %v", installedRequest, installedErr)
	}

	// An expired entry refuses the bind itself, leaving the row unbound. The
	// initializer ignores only the quote's slot window (it moves no
	// principal), never the entry's own wall-clock expiry.
	expiredKey := "auto-init-expired-" + time.Now().Format("150405.000000000")
	seedAutoInitializerPilotRoute(t, ctx, db, expiredKey, &price, candidateEquity)
	slotExpired := autoSelectorEntryFixture(time.Now().UTC().Add(-time.Minute), candidateEquity, &price)
	storeTestSelectorEntry(t, ctx, db, expiredKey, slotExpired)
	expiredRecord, err := db.RecordDecisionOnManifest(ctx, manifest, expiredKey, o, d, manifest.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.bindOperation(ctx, rpc, view, manifest, expiredRecord.OperationID, o, d, r, kaminoInitializationEffects(r)), "selector_entry_quote_expired")
	var bound bool
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects ? 'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, expiredRecord.OperationID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound {
		t.Fatal("refused bind left a bound record behind")
	}
}
