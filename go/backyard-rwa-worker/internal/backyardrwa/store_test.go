package backyardrwa

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEveryDecisionHasDurableInitialStatus(t *testing.T) {
	held, reason := initialDecisionStatus(Decision{Action: Hold, Reason: "no_action"})
	if held != Held || reason != nil || IsNonterminal(held) {
		t.Fatalf("HOLD was not terminal and durable: status=%s reason=%v", held, reason)
	}
	manual, reason := initialDecisionStatus(Decision{Action: HoldManualRecovery, Reason: "identity"})
	if manual != ManualRecovery || reason != "identity" || IsNonterminal(manual) {
		t.Fatalf("manual hold was not terminal and durable: status=%s reason=%v", manual, reason)
	}
	transactional, _ := initialDecisionStatus(Decision{Action: VoltrAllocateToSquads})
	if transactional != Decided || !IsNonterminal(transactional) {
		t.Fatalf("transactional decision did not enter decided: %s", transactional)
	}
}

func TestMigrationPreservesOneNonterminalAndTerminalHolds(t *testing.T) {
	migrationsRoot := filepath.Join("..", "..", "..", "..", "crates", "loyal-yield-store", "migrations")
	legacyPath := filepath.Join(migrationsRoot, "0055_backyard_rwa_worker.sql")
	if _, err := os.Stat(legacyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("colliding Backyard migration 0055 must not exist: %v", err)
	}

	path := filepath.Join(migrationsRoot, "0070_backyard_rwa_worker.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := normalizeSQL(string(data))
	for _, required := range []string{
		"multiply_route_states_schema_v8_v9_or_backyard_v1",
		"(state ->> 'schemaVersion')::INTEGER = 8",
		"(state ->> 'schemaVersion')::INTEGER = 9",
		"(state ->> 'schemaVersion')::INTEGER = 10",
		"state ->> 'engineVersion' = 'earn_max_v1'",
		"state ->> 'engineVersion' = 'earn_max_v2'",
		"state ->> 'engineVersion' = 'backyard_rwa_v1'",
		"'request_withdrawal'",
		"'cancel_withdrawal'",
		"source_instruction_index IS NOT NULL",
		"source_instruction_index IS NULL",
		"multiply_operations_one_nonterminal_per_route",
		"status = 'held'",
		"action = 'HOLD'",
		"status = 'manual_recovery' AND recovery_reason IS NOT NULL",
		"simulation_result JSONB",
		"reconciled_effects JSONB",
		"submitted_at IS NOT NULL",
		"confirmed_slot IS NOT NULL AND confirmation_status IN ('confirmed', 'finalized')",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration contract missing %q", required)
		}
	}

	earnMaxData, err := os.ReadFile(filepath.Join(migrationsRoot, "0068_earn_max_account_cash_flows.sql"))
	if err != nil {
		t.Fatal(err)
	}
	earnMaxLifecycle := constraintCheckBody(t, string(earnMaxData), "multiply_operations_check")
	if !strings.Contains(sql, earnMaxLifecycle) {
		t.Fatal("migration does not preserve the complete 0068 Earn Max lifecycle check")
	}

	indexStart := strings.Index(sql, "CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route")
	if indexStart < 0 {
		t.Fatal("one-nonterminal index is missing")
	}
	indexEnd := strings.Index(sql[indexStart:], ";")
	if indexEnd < 0 || strings.Contains(sql[indexStart:indexStart+indexEnd], "'held'") {
		t.Fatal("terminal HOLD entered the one-nonterminal index")
	}
}

func TestMigration0070IsRegisteredAndSnapshotConflictMatches0067(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..", "..", "..")
	for _, path := range []string{
		filepath.Join(repositoryRoot, "crates", "loyal-yield-store", "src", "store.rs"),
		filepath.Join(repositoryRoot, "crates", "loyal-yield-orchestrator", "src", "bin", "yield-migrations.rs"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source := normalizeSQL(string(data))
		if !strings.Contains(source, "version: 70") || !strings.Contains(source, "0070_backyard_rwa_worker.sql") {
			t.Fatalf("migration 0070 is not registered in %s", path)
		}
	}
	if !strings.Contains(PositionSnapshotInsert, "ON CONFLICT (route_key, observed_slot) DO NOTHING") {
		t.Fatal("position snapshot insert does not target the unique key installed by migration 0067")
	}
	if strings.Contains(PositionSnapshotInsert, "ON CONFLICT (route_key, generation)") {
		t.Fatal("position snapshot insert targets the unique key removed by migration 0067")
	}
}

func TestMigration0071UpgradesApplied0070ForPhaseOne(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..", "..", "..")
	migrationsRoot := filepath.Join(repositoryRoot, "crates", "loyal-yield-store", "migrations")
	data, err := os.ReadFile(filepath.Join(migrationsRoot, "0071_backyard_rwa_phase1_activation.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := normalizeSQL(string(data))
	for _, required := range []string{
		"DROP CONSTRAINT IF EXISTS multiply_operations_action_check",
		"DROP CONSTRAINT IF EXISTS multiply_operations_backyard_action_scope",
		"'SWAP_USDC_TO_PRIME_STEP'",
		"'SWAP_PRIME_TO_USDC_STEP'",
		"DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v8_v9_or_backyard_v1",
		"DROP CONSTRAINT IF EXISTS multiply_route_states_backyard_kind",
		"UPDATE loyal_yield.multiply_route_states",
		"state_version = state_version + 1",
		"'schemaVersion', 10",
		"'engineVersion', 'backyard_rwa_v1'",
		"'routeKind', 'backyard_rwa_v1'",
		"'generation', state_version + 1",
		"route_row.state_version = 817",
		"route_row.state_version = 818",
		"route_row.state ->> 'goal' IS DISTINCT FROM 'claimed'",
		"jsonb_typeof(route_row.state -> 'currentOperationId') IS DISTINCT FROM 'null'",
		"jsonb_typeof(route_row.state -> 'manualRecoveryReason') IS DISTINCT FROM 'null'",
		"route_row.lease_owner IS NOT NULL",
		"route_row.lease_expires_at IS NOT NULL",
		"route_row.fencing_token IS DISTINCT FROM 14480",
		"nonterminal_count <> 0",
		"route_key_count <> 1 OR settings_vault_count <> 1 OR vault_count <> 1",
		"6e6d0e852bec3b64d92b7a33a8cdd96ecb6270e400b3c0713535cb389599102e",
		"f8f33dae4b171fe1eedd3038f2bf2dc440a0aa044e6cbb7f9aac4933ee107ff8",
		"RAISE EXCEPTION 'Backyard Phase 1 canonical route is neither the approved prestate nor poststate'",
		"RAISE EXCEPTION 'Backyard Phase 1 canonical route poststate readback failed'",
		"'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'",
		"'5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6'",
		"'ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration 0071 contract missing %q", required)
		}
	}
	if strings.Count(sql, "'SWAP_USDC_TO_PRIME_STEP'") != 3 || strings.Count(sql, "'SWAP_PRIME_TO_USDC_STEP'") != 3 {
		t.Fatal("both swap actions must appear in the global and both Backyard scope branches")
	}
	for _, forbidden := range []string{
		"INSERT INTO loyal_yield.multiply_route_states",
		"DELETE FROM loyal_yield.multiply_route_states",
		"UPDATE loyal_yield.multiply_operations SET",
		"DELETE FROM loyal_yield.multiply_operations",
		"UPDATE loyal_yield.multiply_position_snapshots",
		"DELETE FROM loyal_yield.multiply_position_snapshots",
		"loyal-voltr-rwa-multiply-usdc-v1",
	} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("migration 0071 must preserve the canonical row and history; found %q", forbidden)
		}
	}
	for _, path := range []string{
		filepath.Join(repositoryRoot, "crates", "loyal-yield-store", "src", "store.rs"),
		filepath.Join(repositoryRoot, "crates", "loyal-yield-orchestrator", "src", "bin", "yield-migrations.rs"),
	} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), "version: 71") || !strings.Contains(string(source), "0071_backyard_rwa_phase1_activation.sql") {
			t.Fatalf("migration 0071 is not registered after 0070 in %s", path)
		}
	}
}

func TestRouteLeaseDatabaseContractIsNonReentrantAndFenced(t *testing.T) {
	acquire := normalizeSQL(AcquireRouteLeaseSQL)
	for _, required := range []string{
		"lease_owner = $2",
		"lease_expires_at = clock_timestamp() + ($3 * interval '1 millisecond')",
		"fencing_token = fencing_token + 1",
		"lease_owner IS NULL OR lease_expires_at <= clock_timestamp()",
		"RETURNING fencing_token, lease_expires_at",
	} {
		if !strings.Contains(acquire, required) {
			t.Fatalf("lease acquisition is missing %q", required)
		}
	}
	if strings.Contains(acquire, "lease_owner = $2)") {
		t.Fatal("lease acquisition is re-entrant for two overlapping instances with the same deployment owner")
	}

	refresh := normalizeSQL(RefreshRouteLeaseSQL)
	for _, required := range []string{
		"lease_owner = $2",
		"fencing_token = $3",
		"lease_expires_at > clock_timestamp()",
	} {
		if !strings.Contains(refresh, required) {
			t.Fatalf("lease refresh is missing fence %q", required)
		}
	}
	release := normalizeSQL(ReleaseRouteLeaseSQL)
	if !strings.Contains(release, "lease_owner = $2 AND fencing_token = $3") ||
		!strings.Contains(release, "lease_owner = NULL, lease_expires_at = NULL") {
		t.Fatal("lease release is not compare-and-clear on the exact fence")
	}
	for name, query := range map[string]string{
		"route decision":     RouteStateForUpdate,
		"operation mutation": OperationRouteForLeaseSQL,
	} {
		normalized := normalizeSQL(query)
		if !strings.Contains(normalized, "lease_owner = $2") ||
			!strings.Contains(normalized, "fencing_token = $3") ||
			!strings.Contains(normalized, "lease_expires_at > clock_timestamp()") ||
			!strings.Contains(normalized, "FOR UPDATE") {
			t.Fatalf("%s does not lock and verify the live fence: %s", name, normalized)
		}
	}
}

func TestPostMutationNAVCadenceUsesLatestReconciledMoneyMutation(t *testing.T) {
	query := normalizeSQL(PostMutationNAVRequiredSQL)
	for _, required := range []string{
		"status = 'reconciled'",
		"'SWAP_USDC_TO_PRIME_STEP'",
		"'SWAP_PRIME_TO_USDC_STEP'",
		"'OPEN_PRIME_USDC_STEP'",
		"'DELEVER_PRIME_USDC_STEP'",
		"'VOLTR_ALLOCATE_TO_SQUADS'",
		"'STAGE_SQUADS_TO_VOLTR'",
		"'VOLTR_RESTORE_IDLE'",
		"'REPORT_NAV'",
		"ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id DESC LIMIT 1",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("post-mutation NAV query is missing %q", required)
		}
	}
}

func TestActionableIdempotencyUsesDurableTerminalEpoch(t *testing.T) {
	query := normalizeSQL(LatestDecisionEpochSQL)
	for _, required := range []string{
		"operation_id",
		"route_key = $1",
		"status IN ('reconciled','failed')",
		"status = 'manual_recovery' AND action = 'REPORT_NAV'",
		"ORDER BY updated_at DESC, operation_id DESC LIMIT 1",
		"'genesis'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("durable operation epoch query is missing %q", required)
		}
	}
	if strings.Contains(query, "status = 'manual_recovery')") {
		t.Fatal("capital-moving manual recovery was made retryable")
	}
	actionable := Decision{Action: VoltrAllocateToSquads, IdempotencyKey: "economic-state"}
	first, err := durableDecisionIdempotencyKey("route", "reconciled-a", actionable)
	if err != nil {
		t.Fatal(err)
	}
	later, err := durableDecisionIdempotencyKey("route", "reconciled-b", actionable)
	if err != nil {
		t.Fatal(err)
	}
	if first == later {
		t.Fatal("a genuinely later reconciled lifecycle reused an actionable operation identity")
	}
	retry, err := durableDecisionIdempotencyKey("route", "failed-prebroadcast", actionable)
	if err != nil || retry == first {
		t.Fatal("a terminal pre-broadcast failure did not advance the durable retry identity")
	}
	if _, err := durableDecisionIdempotencyKey("route", "", actionable); err == nil {
		t.Fatal("actionable decision accepted an absent durable epoch")
	}
	hold := Decision{Action: Hold, IdempotencyKey: "unchanged-hold"}
	holdA, err := durableDecisionIdempotencyKey("route", "reconciled-a", hold)
	if err != nil {
		t.Fatal(err)
	}
	holdB, err := durableDecisionIdempotencyKey("route", "reconciled-b", hold)
	if err != nil {
		t.Fatal(err)
	}
	if holdA != holdB {
		t.Fatal("unchanged HOLD stopped deduping across lifecycle epochs")
	}
}

func TestCapitalManualRecoveryBlocksEveryNewExecutableDecision(t *testing.T) {
	query := normalizeSQL(UnresolvedCapitalRecoverySQL)
	for _, required := range []string{
		"status = 'manual_recovery'",
		"'VOLTR_ALLOCATE_TO_SQUADS'",
		"'STAGE_SQUADS_TO_VOLTR'",
		"'VOLTR_RESTORE_IDLE'",
		"'SWAP_USDC_TO_PRIME_STEP'",
		"'SWAP_PRIME_TO_USDC_STEP'",
		"'OPEN_PRIME_USDC_STEP'",
		"'DELEVER_PRIME_USDC_STEP'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("manual-recovery fence is missing %q", required)
		}
	}
	if strings.Contains(query, "'REPORT_NAV'") {
		t.Fatal("report-only manual recovery was made a capital execution blocker")
	}
}

func TestExistingRouteStateMigrationProvidesLeaseFence(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "crates", "loyal-yield-store", "migrations", "0051_multiply_route_state.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := normalizeSQL(string(data))
	for _, required := range []string{
		"lease_owner TEXT",
		"lease_expires_at TIMESTAMPTZ",
		"fencing_token BIGINT NOT NULL DEFAULT 0",
		"(lease_owner IS NULL) = (lease_expires_at IS NULL)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("existing route table lacks lease contract %q", required)
		}
	}
}

// This integration test is intentionally opt-in: CI and local unit runs do not
// need credentials, while an activation environment can prove acquisition,
// non-reentrancy, refresh, and exact-token release against the real schema.
func TestRouteLeaseAgainstDatabase(t *testing.T) {
	databaseURL := os.Getenv("BACKYARD_RWA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BACKYARD_RWA_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner := "render:srv-integrationtest:sha-" + strings.Repeat("e", 40)
	lease, err := database.AcquireRouteLease(ctx, productionRouteKey, owner, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_, _ = database.ReleaseRouteLease(context.Background())
		}
	}()
	if lease.Owner != owner || lease.FencingToken <= 0 {
		t.Fatalf("invalid acquired lease: %+v", lease)
	}
	second, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.AcquireRouteLease(ctx, productionRouteKey, owner, 30*time.Second); !errors.Is(err, ErrRouteLeaseUnavailable) {
		t.Fatalf("unexpired same-owner lease was re-entered: %v", err)
	}
	refreshed, err := database.RefreshRouteLease(ctx, 30*time.Second)
	if err != nil || !refreshed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("lease refresh failed: lease=%+v err=%v", refreshed, err)
	}
	released, err = database.ReleaseRouteLease(ctx)
	if err != nil || !released {
		t.Fatalf("exact lease release failed: released=%v err=%v", released, err)
	}
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

func constraintCheckBody(t *testing.T, sql string, constraint string) string {
	t.Helper()
	normalized := normalizeSQL(sql)
	prefix := "ADD CONSTRAINT " + constraint + " CHECK ("
	start := strings.Index(normalized, prefix)
	if start < 0 {
		t.Fatalf("constraint %s not found", constraint)
	}
	start += len(prefix)
	end := strings.Index(normalized[start:], ") NOT VALID;")
	if end < 0 {
		t.Fatalf("constraint %s has no NOT VALID terminator", constraint)
	}
	return strings.TrimSpace(normalized[start : start+end])
}
