package backyardrwa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInitializationDatabaseSettlementBindsWireAndPreservesReservation(t *testing.T) {
	for _, pilot := range []bool{false, true} {
		t.Run(fmt.Sprintf("pilot=%t", pilot), func(t *testing.T) { testInitializationDatabaseSettlement(t, pilot) })
	}
}
func testInitializationDatabaseSettlement(t *testing.T, pilot bool) {
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
	budget := emptyTestBudget()
	budget.Families["Maple"] = FamilyBudget{}
	stateValue := map[string]any{"generation": 1, "phase3": budget}
	version := int64(1)
	if pilot {
		flat := pilotFlatFixture(t)
		flatJSON, _ := json.Marshal(flat)
		previous, _ := json.Marshal(budget)
		authority := pilotTestAuthority(budget)
		authority.Generation = 2
		authority.FinalizedSlot = flat.Slot
		authority.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
		var err error
		budget, err = activatePilotBudget(budget, authority)
		if err != nil {
			t.Fatal(err)
		}
		version = 2
		stateValue["generation"] = version
		stateValue["phase3"] = budget
		stateValue["pilotBudgetActivation"] = pilotBudgetActivation{authority, previous, flat}
	}
	state, _ := json.Marshal(stateValue)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,$3)`, key, state, version); err != nil {
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
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
 VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(InitializeKaminoObligation), SelectedRouteID, raw); err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(*expected.Initialization, raw)
	if err != nil {
		t.Fatal(err)
	}
	reservation := BudgetReservation{OperationID: id, Family: "Maple", IntentSHA256: digest, UpperMicros: 900000}
	if pilot {
		reservation.ExecutionCostUpperMicros = 1000
	}
	if err = db.ReservePhase3(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.bindPhase3WireTx(ctx, tx, id, receipt.Initialization.SignedWireSHA256); err != nil {
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
		var persisted []byte
		if err = db.pool.QueryRow(ctx, `SELECT o.status,s.state->'phase3' FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE operation_id=$1`, id).Scan(&status, &persisted); err != nil {
			t.Fatal(err)
		}
		var after Phase3Budget
		if json.Unmarshal(persisted, &after) != nil {
			t.Fatal("budget decode")
		}
		if drift != "" {
			if status != "reconciling" || len(after.Reservations) != 1 || after.Families["Maple"].SpentMicros != 0 || after.Families["Maple"].ExecutionCostSpentMicros != 0 {
				t.Fatal("invalid receipt released reservation")
			}
		} else if status != "reconciled" || len(after.Reservations) != 0 || after.Families["Maple"].SpentMicros != reservation.UpperMicros || after.Families["Maple"].ExecutionCostSpentMicros != reservation.ExecutionCostUpperMicros {
			t.Fatal("native finality did not settle exactly once")
		}
		if drift == "" {
			var authJSON []byte
			if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&authJSON); err != nil {
				t.Fatal(err)
			}
			var auth phase3OperationAuthorization
			if json.Unmarshal(authJSON, &auth) != nil || auth.BookedSpentMicros != reservation.UpperMicros || auth.BookedExecutionCostMicros != reservation.ExecutionCostUpperMicros {
				t.Fatal("journal settlement lost gross or expense bound")
			}
			if pilot && auth.PilotAuthorityID != pilotBudgetAuthorityID {
				t.Fatal("settlement lost pilot authority")
			}
		}
	}
}

func TestInitializationMigrationRetainsEngineAndLaneBoundaries(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	migration, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/0079_backyard_rwa_initializer_actions.sql")
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
