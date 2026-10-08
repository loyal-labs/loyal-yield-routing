package fleet

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The holding guards listed 'needs_reconcile', a lookup-table state that
// signed_route_submissions never holds, and missed reconciliation_pending,
// expiry_check_pending and effect_ambiguous. A route still in flight could be
// leased or continued again. The guard must exclude exactly the states the
// schema's one-open-submission index treats as terminal.
func TestSubmissionGuardMatchesTheSchemaOpenSubmissionIndex(t *testing.T) {
	raw := os.Getenv("FLEET_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var def string
	if err := store.pool.QueryRow(ctx, `SELECT pg_get_expr(i.indpred, i.indrelid) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='loyal_yield' AND c.relname='signed_route_submissions_one_nonterminal_opportunity_idx'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	for _, state := range strings.Split(strings.Trim(submissionTerminalStates, "()"), ",") {
		if !strings.Contains(def, state) {
			t.Fatalf("schema open-submission index %q does not treat %s as terminal", def, state)
		}
	}
	var allowed string
	if err := store.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='signed_route_submissions_state_check'`).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"reconciliation_pending", "expiry_check_pending", "effect_ambiguous"} {
		if !strings.Contains(allowed, "'"+state+"'") || strings.Contains(submissionTerminalStates, "'"+state+"'") {
			t.Fatalf("in-flight state %s must be allowed by the schema and held by the guard", state)
		}
	}
}
