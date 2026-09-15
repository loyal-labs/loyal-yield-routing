package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestJournalOrderAfterCompositeIdentity pins the Go side of the journal
// ordering: rows confirmed in one slot are ordered by exactly the tie-break
// columns the SQL ordering uses, so "latest" never depends on which row the
// database happens to scan first.
func TestJournalOrderAfterCompositeIdentity(t *testing.T) {
	early := time.Unix(1_700_000_000, 0)
	later := early.Add(time.Second)
	if !journalOrderAfter(journalOrderKey{slot: 200, updatedAt: later, operationID: "b"}, journalOrderKey{slot: 200, updatedAt: early, operationID: "a"}) {
		t.Fatal("a later tie-break in the same slot was not the latest")
	}
	if journalOrderAfter(journalOrderKey{slot: 200, updatedAt: early, operationID: "a"}, journalOrderKey{slot: 200, updatedAt: later, operationID: "b"}) {
		t.Fatal("an earlier tie-break in the same slot was treated as latest")
	}
	if !journalOrderAfter(journalOrderKey{slot: 200, updatedAt: early, operationID: "b"}, journalOrderKey{slot: 200, updatedAt: early, operationID: "a"}) {
		t.Fatal("an equal-time tie was not resolved by the operation id")
	}
	if !journalOrderAfter(journalOrderKey{slot: 201, updatedAt: early, operationID: "a"}, journalOrderKey{slot: 200, updatedAt: later, operationID: "b"}) {
		t.Fatal("a higher confirmed slot was not the latest")
	}
	if journalOrderAfter(journalOrderKey{slot: 200, updatedAt: later, operationID: "b"}, journalOrderKey{}) {
		t.Fatal("a row without a confirmed slot was treated as latest")
	}
}

func TestReconciledBridgeJournalAgainstDatabase(t *testing.T) {
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
	defer db.Close()
	_, err = db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS loyal_yield;
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
	 route_key text PRIMARY KEY,state_version bigint NOT NULL DEFAULT 1,state jsonb NOT NULL,
	 lease_owner text,lease_expires_at timestamptz,fencing_token bigint NOT NULL DEFAULT 0,
	 updated_at timestamptz NOT NULL DEFAULT now(),
	 CHECK ((state->>'generation')::bigint=state_version),
	 CHECK ((lease_owner IS NULL)=(lease_expires_at IS NULL)));
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_operations (
	 operation_id text PRIMARY KEY,route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
	 status text NOT NULL,expected_effects jsonb NOT NULL,signed_wire bytea,broadcast_intent_at timestamptz,
	 updated_at timestamptz NOT NULL DEFAULT now());
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS recovery_reason text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS action text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS strategy_key text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS transaction_signature text,
	 ADD COLUMN IF NOT EXISTS confirmed_slot bigint,ADD COLUMN IF NOT EXISTS confirmation_status text,
	 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,ADD COLUMN IF NOT EXISTS reconciled_effects jsonb;`)
	if err != nil {
		t.Fatal(err)
	}

	routeKey := fmt.Sprintf("journal-facts-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, routeKey, "journal-facts-writer", time.Minute); err != nil {
		t.Fatal(err)
	}
	// insertOperation writes one journal row; later updates sort before earlier
	// ones because the offset shifts clock_timestamp() backwards.
	insertOperation := func(t *testing.T, suffix, status, action string, slot int64, effects string, ageSeconds int) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		 (operation_id,route_key,status,action,strategy_key,expected_effects,confirmed_slot,updated_at)
		 VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,clock_timestamp() - make_interval(secs => $8))`,
			routeKey+suffix, routeKey, status, action, routeKey, effects, slot, ageSeconds); err != nil {
			t.Fatal(err)
		}
	}
	armedNAV := func(slot, nav int64) string {
		return fmt.Sprintf(`{"decision":{"observationSlot":%d},"expectedEffects":{"returnData":{"dataBase64":%q}}}`,
			slot, base64.StdEncoding.EncodeToString(binary.LittleEndian.AppendUint64(nil, uint64(nav))))
	}
	journal := func(t *testing.T) ReconciledBridgeJournalState {
		t.Helper()
		state, err := db.ReconciledBridgeJournal(ctx, routeKey)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	// One reconciled report: the ticket sequence and the armed NAV come from it,
	// and no mutation is newer.
	insertOperation(t, "-report", "reconciled", "REPORT_NAV", 100, armedNAV(100, 42), 400)
	state := journal(t)
	if !state.TicketSequenceKnown || state.TicketSequenceRaw != 100 {
		t.Fatalf("the reconciled report did not set the ticket sequence: %+v", state)
	}
	if !state.ArmedNAVKnown || state.ArmedNAVRaw != 42 || state.ArmedNAVMalformed || state.ArmedNAVReturnDataMissing {
		t.Fatalf("the reconciled report did not arm the receipt NAV: %+v", state)
	}
	if state.MutationAfterReport || state.StageAfterTicket {
		t.Fatalf("a lone report looks like a mutation or a stage: %+v", state)
	}

	// A reconciled bridge mutation in a later slot is the accepted explanation.
	insertOperation(t, "-allocate", "reconciled", "VOLTR_ALLOCATE_TO_SQUADS", 101, `{"decision":{"observationSlot":101,"amountRaw":3}}`, 300)
	if state = journal(t); !state.MutationAfterReport {
		t.Fatalf("a reconciled mutation newer than the report was not explained: %+v", state)
	}
	if state.TicketSequenceRaw != 101 {
		t.Fatalf("the ticket-consuming sequence did not follow the later reconcile: %+v", state)
	}

	// An unreconciled row is not evidence at all.
	insertOperation(t, "-pending", "submitted", "VOLTR_RESTORE_IDLE", 102, `{"decision":{"observationSlot":102}}`, 200)
	if state = journal(t); !state.MutationAfterReport || state.TicketSequenceRaw != 101 {
		t.Fatalf("an unreconciled row was read as journal evidence: %+v", state)
	}

	// Two operations in one slot are ordered by the SQL tie-break: a stage whose
	// updated_at is older than the report's is not after the last ticket
	// consumption, and a mutation older than that report does not need reporting.
	insertOperation(t, "-stage-old", "reconciled", "STAGE_SQUADS_TO_VOLTR", 200, `{"decision":{"observationSlot":200,"amountRaw":7}}`, 60)
	insertOperation(t, "-report-200", "reconciled", "REPORT_NAV", 200, armedNAV(200, 99), 30)
	if state = journal(t); state.StageAfterTicket {
		t.Fatalf("a same-slot stage ordered before the report counted as after it: %+v", state)
	}
	if !state.ArmedNAVKnown || state.ArmedNAVRaw != 99 {
		t.Fatalf("the same-slot tie did not arm the latest report's NAV: %+v", state)
	}
	if state.MutationAfterReport {
		t.Fatalf("a same-slot mutation ordered before the report requested a report: %+v", state)
	}

	// The identical slot with the tie-break the other way round flips both.
	insertOperation(t, "-stage-new", "reconciled", "STAGE_SQUADS_TO_VOLTR", 200, `{"decision":{"observationSlot":200,"amountRaw":7}}`, 10)
	if state = journal(t); !state.StageAfterTicket {
		t.Fatalf("the SQL-latest stage in a shared slot was not after the ticket: %+v", state)
	}
	if !state.StagedAmountKnown || state.StagedAmountRaw != 7 {
		t.Fatalf("the staged amount did not come from the SQL-latest stage: %+v", state)
	}
	if !state.MutationAfterReport {
		t.Fatalf("the SQL-latest mutation in a shared slot did not require a report: %+v", state)
	}
}
