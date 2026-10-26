package backyard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestSelectorEvaluateFailureCodesAllowlisted proves the diagnostic echoes a
// hold reason only when it is one of the locked admission's own codes, and
// that every allowlisted code echoes itself verbatim.
func TestSelectorEvaluateFailureCodesAllowlisted(t *testing.T) {
	t.Parallel()
	for reason := range selectorEvaluateAdmissionHoldCodes {
		if got := sanitizedSelectorEvaluateFailure(staleHold(reason)); got != reason {
			t.Fatalf("allowlisted hold %q did not echo itself: %q", reason, got)
		}
	}
	wrapped := fmt.Errorf("locked persistence: %w", staleHold("selector_evaluation_not_current"))
	if got := sanitizedSelectorEvaluateFailure(wrapped); got != "selector_evaluation_not_current" {
		t.Fatalf("wrapped hold not visible: %q", got)
	}
}

// TestSelectorEvaluateFailureDatabaseClasses proves each known database error
// class maps to its one fixed code, including when wrapped.
func TestSelectorEvaluateFailureDatabaseClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"attempt_deadline", context.DeadlineExceeded, "selector_evaluate_timeout"},
		{"wrapped_deadline", fmt.Errorf("evaluate: %w", context.DeadlineExceeded), "selector_evaluate_timeout"},
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
	t.Parallel()
	secrets := []string{"hunter2", "postgresql://", "10.0.0.7"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unknown_hold", staleHold("operator note postgresql://backyard:hunter2@10.0.0.7/neon"), "selector_evaluate_unavailable"},
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
