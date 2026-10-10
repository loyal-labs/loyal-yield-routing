package backyard

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

// Real PostgreSQL checks the shipped lifecycle SQL and proof persistence,
// through 0099, which stops requiring booked-cost fields. This deliberately
// uses a synthetic signed fixture: the pure validator tests the
// cryptographic/effect boundary, not a production signer.
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
	_, err = pool.Exec(ctx, `CREATE TABLE `+operations+`(
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
		raw, err := os.ReadFile("../../../../migrations/yield/" + file)
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
	migrate("0083_backyard_rwa_finalized_restore_failure.sql")
	marshal := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signature, SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: receipt.Slot, Reason: "adaptor_report_slot_refused", AtomicNoCapitalMovement: true, FeeLamports: 5000, Receipt: receipt}
	insert := func(id, status, effects string) error {
		_, err := pool.Exec(ctx, `INSERT INTO `+operations+`(operation_id,route_key,status,action,strategy_key,expected_effects,signed_wire,signed_wire_sha256,message_sha256,transaction_signature,recent_blockhash,last_valid_block_height,simulation_slot,simulation_result,broadcast_intent_at)
 VALUES($1,'route',$2,'REPORT_NAV','OnRe/ONyc/USDC',$3::jsonb,$4,$5,$6,$7,$8,99,42,'{}',clock_timestamp())`, id, status, effects, wire, wireHash, messageHash, signature, bridgeVault)
		return err
	}
	settle := func(id, proofJSON, suffix string) error {
		_, err := pool.Exec(ctx, `UPDATE `+operations+` SET status='failed',confirmation_status='finalized',confirmed_slot=$2,reconciliation_sha256=$3,reconciled_effects=$4::jsonb,recovery_reason=$5`+suffix+` WHERE operation_id=$1 AND status='submitted'`, id, proof.Slot, sha256Bytes([]byte(proofJSON)), proofJSON, proof.Reason)
		return err
	}
	// A finalized failure written under 0083 with its booked fee stays valid:
	// 0099 re-validates every existing row.
	var legacyProof map[string]any
	if err = json.Unmarshal([]byte(marshal(proof)), &legacyProof); err != nil {
		t.Fatal(err)
	}
	legacyProof["bookedFeeMicros"] = 500
	legacyAuth := map[string]any{"goalId": "01a06b6c-8023-72b1-ad5d-c97c0662820e", "intentSha256": sha256Bytes([]byte("intent")), "signedWireSha256": wireHash, "bookedSpentMicros": 500}
	if err = insert("legacy", "submitted", marshal(map[string]any{"phase3": legacyAuth})); err != nil {
		t.Fatal(err)
	}
	if err = settle("legacy", marshal(legacyProof), ""); err != nil {
		t.Fatal("0083 refused its own booked failure:", err)
	}
	// Under 0083 a failure without booked cost is refused.
	auth := phase3OperationAuthorization{IntentSHA256: sha256Bytes([]byte("intent")), SignedWireSHA256: wireHash}
	if err = insert("unbooked", "submitted", marshal(map[string]any{"phase3": auth})); err != nil {
		t.Fatal(err)
	}
	if err = settle("unbooked", marshal(proof), ""); err == nil {
		t.Fatal("0083 admitted a failure without booked cost")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM `+operations+` WHERE operation_id='unbooked'`); err != nil {
		t.Fatal(err)
	}
	migrate("0099_backyard_rwa_failure_without_booked_cost.sql")
	originalEffects := marshal(map[string]any{"phase3": auth, "decision": map[string]string{"reason": "report"}})
	if err = insert("failed", "submitted", originalEffects); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var result string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM `+operations+` o WHERE o.operation_id='failed'`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
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
		{"no fee", func(p map[string]any) { delete(p, "feeLamports") }, ""},
		{"unfinalized", func(map[string]any) {}, ",confirmation_status='confirmed'"},
		{"null action", func(map[string]any) {}, ",action=NULL"},
		{"capital action", func(map[string]any) {}, ",action='VOLTR_ALLOCATE_TO_SQUADS'"},
		{"missing digest", func(map[string]any) {}, ",reconciliation_sha256=NULL"},
		{"missing signed wire", func(map[string]any) {}, ",signed_wire=NULL"},
		{"unbound wire", func(map[string]any) {}, `,expected_effects=jsonb_set(expected_effects,'{phase3,signedWireSha256}','"other"')`},
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
			if err := settle("failed", marshal(altered), tc.suffix); err == nil {
				t.Fatal("malformed finalized failure admitted")
			}
			if snapshot() != before {
				t.Fatal("failed lifecycle update changed the historical wire")
			}
		})
	}
	if err = settle("failed", marshal(proof), ""); err != nil {
		t.Fatal(err)
	}
	var status, confirmed, savedSignature, savedHash, decision string
	var savedWire []byte
	var savedSlot int64
	err = pool.QueryRow(ctx, `SELECT o.status,o.confirmation_status,o.transaction_signature,o.signed_wire_sha256,o.signed_wire,o.confirmed_slot,o.expected_effects->'decision'->>'reason' FROM `+operations+` o WHERE o.operation_id='failed'`).Scan(&status, &confirmed, &savedSignature, &savedHash, &savedWire, &savedSlot, &decision)
	if err != nil {
		t.Fatal(err)
	}
	if status != "failed" || confirmed != "finalized" || savedSignature != signature || savedHash != wireHash || !bytes.Equal(savedWire, wire) || savedSlot != proof.Slot || decision != "report" {
		t.Fatal("finalized history or identity lost")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM `+operations+` WHERE status='failed' AND operation_id IN ('failed','legacy')`).Scan(&count); err != nil || count != 2 {
		t.Fatal("terminal rows changed", count, err)
	}
}
