package backyardrwa

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAllocationWindowCountsEveryBroadcastState(t *testing.T) {
	counted := map[string]bool{}
	for _, status := range allocationCountedStatuses {
		counted[status] = true
	}
	// Everything from the broadcast intent onward proves bytes left the
	// worker, so each state must consume the daily window. A wire can rest in
	// manual_recovery indefinitely without ever being proven to have failed
	// before broadcast, so excluding it would let a parked route spend the
	// limit a second time.
	for _, status := range []OperationStatus{BroadcastIntent, Submitted, Confirmed, Reconciling, Reconciled, ManualRecovery, Failed} {
		if !counted[string(status)] {
			t.Fatalf("broadcast state %s is not counted by the daily allocation window", status)
		}
	}
	// Pre-broadcast states prove nothing was sent; counting them would hold
	// the route on amounts that never left.
	for _, status := range []OperationStatus{Decided, Built, Simulated, Signed, Held} {
		if counted[string(status)] {
			t.Fatalf("pre-broadcast state %s is counted by the daily allocation window", status)
		}
	}
}

// The window must age allocations out on the broadcast intent stamp - never
// on the mutable updated_at - and must count rows a stuck wire parked in
// manual_recovery. Same disposable phase3 database slice as the rest of the
// store tests.
func TestAllocationSentWindowKeysIntentTimeAndCountsManualRecovery(t *testing.T) {
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
	defer db.Close()
	_, err = db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS loyal_yield;
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
	 route_key text PRIMARY KEY,state_version bigint NOT NULL DEFAULT 1,state jsonb NOT NULL DEFAULT '{"generation":1}',
	 lease_owner text,lease_expires_at timestamptz,fencing_token bigint NOT NULL DEFAULT 0,
	 updated_at timestamptz NOT NULL DEFAULT now());
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_operations (
	 operation_id text PRIMARY KEY,route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
	 status text NOT NULL,expected_effects jsonb NOT NULL DEFAULT '{}',broadcast_intent_at timestamptz,
	 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now());
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS cycle bigint NOT NULL DEFAULT 1;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS engine_version text NOT NULL DEFAULT 'linus_v1';
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS idempotency_key text NOT NULL DEFAULT '';
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS action text;`)
	if err != nil {
		t.Fatal(err)
	}
	route := fmt.Sprintf("phase3-window-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key) VALUES($1)`, route); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []struct {
		name                    string
		status                  string
		amount                  string
		intent, created, update *time.Time
	}{
		// Parked in manual_recovery an hour after its broadcast: counts.
		{"parked", "manual_recovery", "100", ptrTime(now.Add(-time.Hour)), ptrTime(now.Add(-2 * time.Hour)), ptrTime(now.Add(-time.Hour))},
		// Broadcast 25h ago, but reconciliation retries refreshed updated_at
		// into the window: must NOT count, the window keys on the intent.
		{"stale-refreshed", "failed", "400", ptrTime(now.Add(-25 * time.Hour)), ptrTime(now.Add(-26 * time.Hour)), ptrTime(now.Add(-time.Hour))},
		// Broadcast recently but the intent stamp is missing: the immutable
		// creation time keeps it counted instead of silently dropping it.
		{"no-stamp", "submitted", "30", nil, ptrTime(now.Add(-2 * time.Hour)), ptrTime(now.Add(-2 * time.Hour))},
	}
	for _, row := range rows {
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
			(operation_id,route_key,cycle,engine_version,idempotency_key,action,status,expected_effects,broadcast_intent_at,created_at,updated_at)
			VALUES($1,$2,1,'linus_v1',$3,'VOLTR_ALLOCATE_TO_SQUADS',$4,
			 jsonb_build_object('decision',jsonb_build_object('amountRaw',$5::text)),$6,$7,$8)`,
			route+"-"+row.name, route, route+"-"+row.name, row.status, row.amount, row.intent, row.created, row.update); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AcquireRouteLease(ctx, route, "window-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	sent, err := db.AllocationSentRawTrailingWindow(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	// The refreshed-but-stale row is excluded; intent-less rows still count.
	if sent != 130 {
		t.Fatalf("trailing window summed %d raw, want 130 (parked 100 + intent-less 30, stale-refreshed 400 aged out)", sent)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
