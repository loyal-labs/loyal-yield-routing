package backyard

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

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
