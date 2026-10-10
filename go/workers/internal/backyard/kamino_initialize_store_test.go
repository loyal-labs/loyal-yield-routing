package backyard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInitializationDatabaseSettlementBindsWire(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
 ADD COLUMN IF NOT EXISTS signed_wire bytea,
 ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
 ADD COLUMN IF NOT EXISTS confirmation_status text,
 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,
 ADD COLUMN IF NOT EXISTS reconciled_effects jsonb`); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("initializer-settlement-%d", time.Now().UnixNano())
	id := key + "-op"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "initializer-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	expected, receipt := initializationReconcileFixture(t)
	message, err := CompileKaminoInitializationMessage(*expected.Initialization)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	wire = append(wire, message...)
	receipt.Initialization.SignedWireSHA256 = sha256Bytes(wire)
	receipt.Signature = encodeBase58(wire[1:65])
	raw, _ := json.Marshal(expected)
	digest, err := Phase3IntentDigest(*expected.Initialization, raw)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(*expected.Initialization, raw)
	if err != nil {
		t.Fatal(err)
	}
	bound, _ := json.Marshal(phase3OperationAuthorization{IntentSHA256: digest, BuildInput: input})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
 VALUES($1,$2,'decided',$3,$4,$5::jsonb || jsonb_build_object('phase3',$6::jsonb))`, id, key, string(InitializeKaminoObligation), SelectedRouteID, string(raw), string(bound)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = bindSignedWireTx(ctx, tx, id, receipt.Initialization.SignedWireSHA256); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciling',signed_wire_sha256=$2,transaction_signature=$3,confirmed_slot=$4,signed_wire=$5 WHERE operation_id=$1`, id, receipt.Initialization.SignedWireSHA256, receipt.Signature, receipt.Slot, wire); err != nil {
		t.Fatal(err)
	}
	for _, drift := range []string{"wire", "rent", "unfinalized", ""} {
		candidate := receipt
		n := *receipt.Initialization
		candidate.Initialization = &n
		switch drift {
		case "wire":
			n.SignedWireSHA256 = sha256Bytes([]byte("unrelated wire"))
		case "rent":
			n.Obligation.Lamports++
		case "unfinalized":
			candidate.Finalized = false
		}
		checked, effects, reconcileErr := ReconcileConfirmedTransaction(expected, candidate)
		if reconcileErr == nil {
			reconcileErr = db.MarkReconciled(ctx, id, checked, effects, candidate)
		}
		if (reconcileErr == nil) != (drift == "") {
			t.Fatalf("drift=%s err=%v", drift, reconcileErr)
		}
		var status string
		if err = db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if (drift != "" && status != "reconciling") || (drift == "" && status != "reconciled") {
			t.Fatalf("drift=%s left status %s", drift, status)
		}
	}
}

func TestInitializationMigrationRetainsEngineAndLaneBoundaries(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	migration, err := os.ReadFile("../../../../migrations/yield/0079_backyard_rwa_initializer_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// A transaction-local schema isolates the actual migration from other test
	// families' deliberately incomplete journal rows. Rollback removes it all.
	if _, err = tx.Exec(ctx, `CREATE SCHEMA initializer_migration_test;
 CREATE TABLE initializer_migration_test.multiply_operations(action text NOT NULL,engine_version text NOT NULL,strategy_key text)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, strings.ReplaceAll(string(migration), "loyal_yield.", "initializer_migration_test.")); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		action, engine, lane string
		valid                bool
	}{
		{string(InitializeKaminoObligation), "backyard_rwa_v1", PhaseOneLaneID, true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", SelectedRouteID, true},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
		{string(OpenRouteStep), "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
		{string(SwapDebtToCollateralStep), "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
		{string(InitializeKaminoObligation), "earn_max_v2", SelectedRouteID, false},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "OnRe/ONyc/USDS", false},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "", false},
		{string(InitializeKaminoObligation), "backyard_rwa_v1", "AUTO/AUTO/PYUSD", false},
		{string(OpenRouteStep), "backyard_rwa_v1", "unregistered", false},
		{string(ReportNAV), "backyard_rwa_v1", SelectedRouteID, true},
		{"borrow_debt", "earn_max_v2", "legacy", true},
		{"borrow_debt", "backyard_rwa_v1", "legacy", false},
	} {
		nested, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = nested.Exec(ctx, `INSERT INTO initializer_migration_test.multiply_operations VALUES($1,$2,NULLIF($3,''))`, row.action, row.engine, row.lane)
		_ = nested.Rollback(ctx)
		if (err == nil) != row.valid {
			t.Fatalf("action=%s engine=%s lane=%s err=%v", row.action, row.engine, row.lane, err)
		}
	}
}
