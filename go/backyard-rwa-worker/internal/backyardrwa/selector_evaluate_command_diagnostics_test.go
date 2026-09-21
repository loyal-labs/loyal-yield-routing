package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestSelectorEvaluateFailureCodesAllowlisted proves the diagnostic echoes a
// hold reason only when it is one of the locked admission's own codes, and
// that every allowlisted code echoes itself verbatim.
func TestSelectorEvaluateFailureCodesAllowlisted(t *testing.T) {
	for reason := range selectorEvaluateAdmissionHoldCodes {
		if got := sanitizedSelectorEvaluateFailure(&BudgetHold{Reason: reason}); got != reason {
			t.Fatalf("allowlisted hold %q did not echo itself: %q", reason, got)
		}
	}
	// The stale-planning guard stays visible instead of folding into the
	// generic code — the exact shape root could not see at 05:27:44Z.
	if got := sanitizedSelectorEvaluateFailure(budgetHold("selector_state_changed_during_quote")); got != "selector_state_changed_during_quote" {
		t.Fatalf("stale guard not visible: %q", got)
	}
	wrapped := fmt.Errorf("locked persistence: %w", budgetHold("selector_entry_requires_reconciled_idle"))
	if got := sanitizedSelectorEvaluateFailure(wrapped); got != "selector_entry_requires_reconciled_idle" {
		t.Fatalf("wrapped hold not visible: %q", got)
	}
}

// TestSelectorEvaluateFailureDatabaseClasses proves each known database error
// class maps to its one fixed code, including when wrapped.
func TestSelectorEvaluateFailureDatabaseClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"attempt_deadline", context.DeadlineExceeded, "selector_evaluate_timeout"},
		{"wrapped_deadline", fmt.Errorf("evaluate: %w", context.DeadlineExceeded), "selector_evaluate_timeout"},
		{"route_fence", pgx.ErrNoRows, "selector_evaluate_route_fence_unavailable"},
		{"wrapped_route_fence", fmt.Errorf("version: %w", pgx.ErrNoRows), "selector_evaluate_route_fence_unavailable"},
		{"record_lock_timeout", &pgconn.PgError{Code: "55P03", Message: "lock_not_available"}, "selector_evaluate_lock_timeout"},
		{"query_cancelled", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, "selector_evaluate_query_cancelled"},
	}
	for _, tc := range cases {
		if got := sanitizedSelectorEvaluateFailure(tc.err); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestSelectorEvaluateFailureMasksSecrets proves unknown hold reasons, raw
// errors and unlisted SQL classes collapse to the generic code without ever
// carrying their text — DSNs, hosts and passwords stay masked.
func TestSelectorEvaluateFailureMasksSecrets(t *testing.T) {
	secrets := []string{"hunter2", "postgresql://", "10.0.0.7"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unknown_hold", &BudgetHold{Reason: "operator note postgresql://backyard:hunter2@10.0.0.7/neon"}, "selector_evaluate_unavailable"},
		{"generic_dsn", errors.New("pq: failed connect to postgresql://backyard:hunter2@10.0.0.7/neon"), "selector_evaluate_unavailable"},
		{"unknown_sql_class", &pgconn.PgError{Code: "XX000", Message: "crash", Detail: "host=10.0.0.7 password=hunter2"}, "selector_evaluate_unavailable"},
		{"wrapped_unknown_sql", fmt.Errorf("record: %w", &pgconn.PgError{Code: "23505", Detail: "dsn postgresql://backyard:hunter2@10.0.0.7/neon"}), "selector_evaluate_unavailable"},
	}
	for _, tc := range cases {
		got := sanitizedSelectorEvaluateFailure(tc.err)
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
		for _, secret := range secrets {
			if strings.Contains(got, secret) {
				t.Fatalf("%s: leaked %q in %q", tc.name, secret, got)
			}
		}
	}
}

// TestSelectorEvaluateExecuteEvaluatorFailureDiagnostic drives the execute
// path with a failing evaluator over the disposable database: the failure
// report carries the sanitized code, the returned hold and lease release stay
// exactly as before, and nothing is persisted — no entry, no receipt, no
// version change, no operation row.
func TestSelectorEvaluateExecuteEvaluatorFailureDiagnostic(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	manifest := autoInitializerFixtureManifest(t)
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", fmt.Sprintf(`{"id":%q,"lane":%q,"equityRaw":250000,"expiresAt":%q}`,
		sha256Bytes([]byte("selector-evaluate-failure")), testAutoLane, time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339Nano)))
	key := productionRouteKey
	resetManualRecoveryProductionRoute(t, ctx, db, "selector-evaluate-cmd-failure")
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, versionBefore := canaryRouteStateAssertions(t, ctx, db, key)

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"stale_guard", budgetHold("selector_state_changed_during_quote"), "selector_state_changed_during_quote"},
		{"secret_sql", fmt.Errorf("scan: duplicate key dsn=postgresql://backyard:hunter2@10.0.0.7/neon"), "selector_evaluate_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			deps := selectorEvaluateDeps{
				manifest: manifest, databaseURL: url,
				newFeed: func(context.Context, RouteManifest) (selectorEvaluateFeed, error) {
					return &stubSelectorEvaluateFeed{}, nil
				},
				evaluate: func(context.Context, *Database, []LaneEconomics) (SelectorResult, error) {
					return SelectorResult{}, tc.err
				},
			}
			err := runSelectorEvaluate(ctx, &out, true, deps)
			var hold *BudgetHold
			if !errors.As(err, &hold) || hold.Reason != "selector_evaluate_unavailable" {
				t.Fatalf("evaluator failure did not return the unchanged generic hold: %v", err)
			}
			var parsed struct {
				Mode             string  `json:"mode"`
				AttemptSeconds   float64 `json:"attemptSeconds"`
				FeedFailure      string  `json:"feedFailure,omitempty"`
				EvaluatorFailure string  `json:"evaluatorFailure"`
			}
			if err = json.Unmarshal(canaryJSON(t, &out), &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed.Mode != "locked_execute_failure" || parsed.EvaluatorFailure != tc.want || parsed.AttemptSeconds < 0 {
				t.Fatalf("failure report wrong: %+v", parsed)
			}
			if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "postgresql://") {
				t.Fatalf("raw evaluator error leaked: %q", out.String())
			}
			if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); version != versionBefore || len(receipts) != 0 {
				t.Fatalf("failure persisted state: version %d (before %d) receipts %d", version, versionBefore, len(receipts))
			}
			assertNoCanaryOperations(t, ctx, db, key)
			assertCanaryLeaseFree(t, ctx, db, key)
		})
	}
}
