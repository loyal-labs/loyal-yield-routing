package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// The looping vault's one-transaction withdrawal (Squads cash to the holding
// account, then Voltr withdraw_strategy through the Trustful adaptor) lands
// and Voltr refuses it. Its finalized receipt proves the rollback, so the row
// ends failed on the migrated schema and the next tick plans again, instead
// of latching manual recovery and stopping the route's withdrawals.
func TestLoopingWithdrawRefusedByVoltrSettlesFailed(t *testing.T) {
	url := os.Getenv("BACKYARD_RWA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires the migrated BACKYARD_RWA_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, engine.DirectDSN(url))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	request := bridgeTestRequest(StageSquadsToVoltr, 25_000_000)
	request.AdaptorConfig, request.Report.NAVAfterRaw = looping.Strategy, 75_000_000
	message, err := compileBridgeMessageForDelegate(looping, request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(key, delegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := messageKeys(signed.message)
	if err != nil {
		t.Fatal(err)
	}
	pre := make([]uint64, len(keys))
	for i := range pre {
		pre[i] = 1_000_000_000
	}
	post := append([]uint64(nil), pre...)
	post[0] -= 5000
	owners := map[string]string{looping.cash: looping.squads, looping.holding: looping.holdingAuth, looping.idleATA: looping.idleAuth}
	var tokens []json.RawMessage
	for i, k := range keys {
		if owner := owners[encodeBase58(k[:])]; owner != "" {
			row, _ := json.Marshal(map[string]any{"accountIndex": i, "mint": bridgeUSDC, "owner": owner, "programId": bridgeTokenProgram,
				"uiTokenAmount": map[string]any{"amount": "25000000", "decimals": 6}})
			tokens = append(tokens, row)
		}
	}
	if len(tokens) != len(owners) {
		t.Fatalf("the withdrawal names %d of its custodies", len(tokens))
	}
	program, fee := voltr.ProgramID.String(), uint64(5000)
	receipt := finalizedFailureReceipt{Slot: 900, Transaction: []string{base64.StdEncoding.EncodeToString(signed.signedWire), "base64"}, Meta: &finalizedFailureMeta{
		Err: json.RawMessage(`{"InstructionError":[0,{"Custom":6004}]}`), Fee: &fee, PreBalances: pre, PostBalances: post,
		PreTokenBalances: tokens, PostTokenBalances: tokens,
		LogMessages: []string{
			"Program " + squads.ProgramID.String() + " invoke [1]",
			"Program " + program + " invoke [2]",
			"Program " + program + " failed: custom program error: 0x1774",
			"Program " + squads.ProgramID.String() + " failed: custom program error: 0x1774",
		},
	}}

	id := fmt.Sprintf("looping-withdraw-failure-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,cycle,engine_version,action,status,idempotency_key,expected_effects,
		signed_wire,signed_wire_sha256,message_sha256,transaction_signature,recent_blockhash,last_valid_block_height,simulation_slot,simulation_result,broadcast_intent_at)
		VALUES($1,$2,1,'backyard_rwa_v1','STAGE_SQUADS_TO_VOLTR','broadcast_intent',$1,'{}',$3,$4,$5,$6,$7,$8,800,'{}',clock_timestamp())`,
		id, looping.RouteKey, signed.signedWire, signed.signedWireSHA256, signed.messageSHA256, signed.transactionSignature, signed.recentBlockhash, signed.lastValidBlockHeight); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id)
	}()

	operation := PersistedOperation{Operation: Operation{ID: id, RouteKey: looping.RouteKey, Decision: Decision{Action: StageSquadsToVoltr}},
		Status: BroadcastIntent, SignedWire: signed.signedWire, SignedWireSHA256: signed.signedWireSHA256, TransactionSignature: signed.transactionSignature}
	settled, err := db.settleFinalizedFailure(ctx, operation, receipt, confirmedFailureReason(receipt.Meta.Err, receipt.Meta.LogMessages), delegate)
	if err != nil || !settled {
		t.Fatalf("a proven rolled-back withdrawal did not settle: settled=%t err=%v", settled, err)
	}
	var status, reason string
	var slot int64
	if err := db.pool.QueryRow(ctx, `SELECT status,recovery_reason,confirmed_slot FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &reason, &slot); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "program_error:"+program+":6004" || slot != 900 {
		t.Fatalf("settled row: status=%s reason=%q slot=%d", status, reason, slot)
	}
}

// Real PostgreSQL checks the shipped lifecycle SQL and proof persistence,
// through 0099, which stops requiring booked-cost fields, and 0104, which
// stops requiring the phase3 bind record. This deliberately
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
	receipt, wire, delegate := failureSettlementFixture(t)
	wireHash, messageHash, signature := sha256Bytes(wire), sha256Bytes(wireMsg(wire)), encodeBase58(wireSig(wire))
	if err = validateFinalizedFailureReceipt(receipt, wire, signature, wireHash, messageHash, delegate); err != nil {
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
	auth := map[string]any{"intentSha256": sha256Bytes([]byte("intent")), "signedWireSha256": wireHash}
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

	// Under 0099 a failure whose row carries no phase3 bind record is refused;
	// 0104 admits it on its own columns, as the one-writer binary writes it.
	if err = insert("unbound", "submitted", marshal(map[string]any{"decision": map[string]string{"reason": "report"}})); err != nil {
		t.Fatal(err)
	}
	if err = settle("unbound", marshal(proof), ""); err == nil {
		t.Fatal("0099 admitted a failure without its bind record")
	}
	migrate("0104_backyard_lifecycle_without_bind.sql")
	if err = settle("unbound", marshal(proof), ""); err != nil {
		t.Fatal("0104 refused a failure without its bind record:", err)
	}
	for _, mutation := range []string{",signed_wire=NULL", ",action='VOLTR_ALLOCATE_TO_SQUADS'", ",confirmation_status='confirmed'"} {
		if err = insert("unbound-"+mutation, "submitted", "{}"); err != nil {
			t.Fatal(err)
		}
		if err = settle("unbound-"+mutation, marshal(proof), mutation); err == nil {
			t.Fatal("0104 admitted a malformed failure:", mutation)
		}
	}
}
