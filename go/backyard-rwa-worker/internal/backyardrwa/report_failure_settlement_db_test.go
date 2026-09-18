package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real PostgreSQL checks the shipped lifecycle SQL and atomic budget/proof
// persistence. This deliberately uses a synthetic signed fixture: the pure
// validator tests the cryptographic/effect boundary, not a production signer.
func TestFinalizedReportFailureMigrationAndAtomicSettlement(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Existing suites retain deliberately incomplete legacy fixtures. A unique
	// test schema runs the exact migrations without mutating those unrelated rows.
	schema := fmt.Sprintf("failure_settlement_%d", time.Now().UnixNano())
	qualified := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+qualified); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+qualified+" CASCADE") }()
	operations := qualified + ".multiply_operations"
	routes := qualified + ".multiply_route_states"
	_, err = pool.Exec(ctx, `CREATE TABLE `+routes+`(route_key text PRIMARY KEY,state jsonb NOT NULL);
 CREATE TABLE `+operations+`(
 operation_id text PRIMARY KEY,route_key text NOT NULL,status text NOT NULL,engine_version text NOT NULL DEFAULT 'backyard_rwa_v1',
 action text,strategy_key text,expected_effects jsonb NOT NULL DEFAULT '{}',signed_wire bytea,signed_wire_sha256 text,
 message_sha256 text,transaction_signature text,recent_blockhash text,last_valid_block_height bigint,
 simulation_slot bigint,simulation_result jsonb,broadcast_intent_at timestamptz,confirmed_slot bigint,
 confirmation_status text,reconciliation_sha256 text,reconciled_effects jsonb,recovery_reason text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp());`)
	if err != nil {
		t.Fatal(err)
	}
	migrate := func(file string) {
		t.Helper()
		raw, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, strings.ReplaceAll(string(raw), "loyal_yield.", qualified+".")); err != nil {
			t.Fatal(err)
		}
	}
	migrate("0075_backyard_rwa_setup_pre_simulation_wire.sql")
	_, err = pool.Exec(ctx, `INSERT INTO `+operations+`(operation_id,route_key,status,action,recovery_reason,expected_effects) VALUES('historic','historic','failed','REPORT_NAV','never_sent','{"historical":"retained"}')`)
	if err != nil {
		t.Fatal(err)
	}
	var historicalBefore []byte
	if err = pool.QueryRow(ctx, `SELECT to_jsonb(o) FROM `+operations+` o WHERE operation_id='historic'`).Scan(&historicalBefore); err != nil {
		t.Fatal(err)
	}
	migrate("0081_backyard_rwa_finalized_report_failure.sql")
	var historicalAfter []byte
	if err = pool.QueryRow(ctx, `SELECT to_jsonb(o) FROM `+operations+` o WHERE operation_id='historic'`).Scan(&historicalAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(historicalBefore, historicalAfter) {
		t.Fatal("migration rewrote historical operation")
	}
	receipt, wire, delegate, effects := failureSettlementFixture(t)
	wireHash, messageHash, signature := sha256Bytes(wire), sha256Bytes(wire[65:]), encodeBase58(wire[1:65])
	if err = validateFinalizedFailureReceipt(receipt, wire, signature, wireHash, messageHash, delegate, effects); err != nil {
		t.Fatal(err)
	}
	budget := emptyTestBudget()
	budget.Families["OnRe"] = FamilyBudget{SpentMicros: 100, ExitMicros: 2000}
	reservation := BudgetReservation{OperationID: "failed", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("intent")), UpperMicros: 1000, ExitAfterMicros: 500, Recovery: true}
	if err = budget.Admit(reservation); err != nil {
		t.Fatal(err)
	}
	cost := ValuedTransactionCost{MessageSHA256: messageHash, Fee: MessageFeeObservation{MessageSHA256: messageHash, Slot: 42, Lamports: 5000}, NativePrice: budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1), ObservationSlot: 42, NetworkFeeMicros: 500}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: reservation.IntentSHA256, SignedWireSHA256: wireHash, SendKnownCost: &cost}
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signature, SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: receipt.Slot, Reason: "adaptor_report_slot_refused", AtomicNoCapitalMovement: true, FeeLamports: 5000, Receipt: receipt}
	next, booked, err := settleFailedFeeBudget(budget, auth, "failed", &proof)
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	originalState := marshal(map[string]any{"generation": 1, "phase3": budget, "history": "preserved"})
	originalEffects := marshal(map[string]any{"phase3": auth, "decision": map[string]string{"reason": "report"}})
	_, err = pool.Exec(ctx, `INSERT INTO `+routes+` VALUES('route',$1::jsonb);
 `, originalState)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO `+operations+`(operation_id,route_key,status,action,strategy_key,expected_effects,signed_wire,signed_wire_sha256,message_sha256,transaction_signature,recent_blockhash,last_valid_block_height,simulation_slot,simulation_result,broadcast_intent_at)
 VALUES('failed','route','submitted','REPORT_NAV','OnRe/ONyc/USDC',$1::jsonb,$2,$3,$4,$5,$6,99,42,'{}',clock_timestamp())`, originalEffects, wire, wireHash, messageHash, signature, bridgeVault)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var result string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_array(r.state,to_jsonb(o))::text FROM `+routes+` r JOIN `+operations+` o ON o.route_key=r.route_key WHERE o.operation_id='failed'`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	// The update shape mirrors the durable method, including writing booked
	// authorization before the lifecycle row, under one transaction.
	attempt := func(proofJSON string, suffix string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err = tx.Exec(ctx, `UPDATE `+routes+` SET state=jsonb_set(state,'{phase3}',$1::jsonb) WHERE route_key='route'`, marshal(next)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE `+operations+` SET expected_effects=jsonb_set(expected_effects,'{phase3}',$1::jsonb) WHERE operation_id='failed'`, marshal(booked)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE `+operations+` SET status='failed',confirmation_status='finalized',confirmed_slot=$1,reconciliation_sha256=$2,reconciled_effects=$3::jsonb,recovery_reason=$4`+suffix+` WHERE operation_id='failed' AND status='submitted'`, proof.Slot, sha256Bytes([]byte(proofJSON)), proofJSON, proof.Reason)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		suffix string
	}{
		{"missing proof", func(p map[string]any) {
			for k := range p {
				delete(p, k)
			}
		}, ""},
		{"wrong signature", func(p map[string]any) { p["signature"] = "wrong" }, ""},
		{"wrong slot", func(p map[string]any) { p["slot"] = 1 }, ""},
		{"no rollback proof", func(p map[string]any) { delete(p, "atomicNoCapitalMovement") }, ""},
		{"unbooked fee", func(p map[string]any) { p["bookedFeeMicros"] = 501 }, ""},
		{"unfinalized", func(map[string]any) {}, ",confirmation_status='confirmed'"},
		{"null action", func(map[string]any) {}, ",action=NULL"},
		{"capital action", func(map[string]any) {}, ",action='VOLTR_ALLOCATE_TO_SQUADS'"},
		{"missing digest", func(map[string]any) {}, ",reconciliation_sha256=NULL"},
		{"missing signed wire", func(map[string]any) {}, ",signed_wire=NULL"},
		{"wrong raw receipt", func(p map[string]any) {
			r := p["receipt"].(map[string]any)
			r["transaction"] = []string{"wrong", "base64"}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var altered map[string]any
			if err := json.Unmarshal([]byte(marshal(proof)), &altered); err != nil {
				t.Fatal(err)
			}
			tc.mutate(altered)
			if err := attempt(marshal(altered), tc.suffix); err == nil {
				t.Fatal("malformed finalized failure admitted")
			}
			if snapshot() != before {
				t.Fatal("failed lifecycle update leaked fee booking or changed historical wire")
			}
		})
	}
	if err = attempt(marshal(proof), ""); err != nil {
		t.Fatal(err)
	}
	var finalBudget Phase3Budget
	var raw []byte
	if err = pool.QueryRow(ctx, `SELECT state->'phase3' FROM `+routes+` WHERE route_key='route'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &finalBudget) != nil || finalBudget.Families["OnRe"].SpentMicros != 600 || finalBudget.Families["OnRe"].ExitMicros != 2000 || len(finalBudget.Reservations) != 0 {
		t.Fatal("fee accounting not committed atomically")
	}
	var status, confirmed, savedSignature, savedHash, history, decision string
	var savedWire []byte
	var savedSlot int64
	err = pool.QueryRow(ctx, `SELECT o.status,o.confirmation_status,o.transaction_signature,o.signed_wire_sha256,o.signed_wire,o.confirmed_slot,r.state->>'history',o.expected_effects->'decision'->>'reason' FROM `+operations+` o JOIN `+routes+` r ON r.route_key=o.route_key WHERE o.operation_id='failed'`).Scan(&status, &confirmed, &savedSignature, &savedHash, &savedWire, &savedSlot, &history, &decision)
	if err != nil {
		t.Fatal(err)
	}
	if status != "failed" || confirmed != "finalized" || savedSignature != signature || savedHash != wireHash || !bytes.Equal(savedWire, wire) || savedSlot != proof.Slot || history != "preserved" || decision != "report" {
		t.Fatal("finalized history or identity lost")
	}
	if _, _, err = settleFailedFeeBudget(finalBudget, booked, "failed", &proof); err == nil {
		t.Fatal("replayed settlement admitted")
	}
}
