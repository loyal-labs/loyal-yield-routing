package backyardrwa

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The harness supplies only a fresh private-socket DB. No runtime, journal,
// signing, recovery or accounting path is invoked by this lease contract.
func TestRouteLeaseIsolatedContentionAndStaleFencing(t *testing.T) {
	if os.Getenv("BACKYARD_ROUTE_LEASE_ISOLATED_FIXTURE") != "1" {
		t.Skip("run scripts/hetzner-backyard-lease-fixture/run.py")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbURL := os.Getenv("BACKYARD_RWA_TEST_DATABASE_URL")
	first, err := OpenDatabase(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	var name string
	if err := first.pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil || name != "backyard_route_lease_fixture" {
		t.Fatalf("requires named disposable fixture database: %v", err)
	}
	second, err := OpenDatabase(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	legacy := "render:srv-leasefixture:sha-" + strings.Repeat("e", 40)
	neutral := "deployment:backyard:production:fixture-host-release:sha-" + strings.Repeat("f", 40)
	if !validLeaseOwner(legacy) || !validLeaseOwner(neutral) {
		t.Fatal("fixture owners must pass patched runtime identity validation")
	}
	assertRow := func(t *testing.T, want RouteLease) {
		t.Helper()
		var owner string
		var token int64
		if err := first.pool.QueryRow(ctx, `SELECT lease_owner,fencing_token FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&owner, &token); err != nil || owner != want.Owner || token != want.FencingToken {
			t.Fatalf("successor row changed: owner=%s token=%d err=%v", owner, token, err)
		}
	}
	for _, pair := range [][2]string{{legacy, neutral}, {neutral, legacy}, {legacy, legacy}, {neutral, neutral}} {
		t.Run(pair[0][:strings.IndexByte(pair[0], ':')]+"-to-"+pair[1][:strings.IndexByte(pair[1], ':')], func(t *testing.T) {
			lease, err := first.AcquireRouteLease(ctx, productionRouteKey, pair[0], time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.AcquireRouteLease(ctx, productionRouteKey, pair[0], time.Minute); !errors.Is(err, ErrRouteLeaseUnavailable) {
				t.Fatalf("same-owner re-entry permitted: %v", err)
			}
			if _, err := second.AcquireRouteLease(ctx, productionRouteKey, pair[1], time.Minute); !errors.Is(err, ErrRouteLeaseUnavailable) {
				t.Fatalf("unexpired contender permitted: %v", err)
			}
			refreshed, err := first.RefreshRouteLease(ctx, time.Minute)
			if err != nil || !refreshed.ExpiresAt.After(lease.ExpiresAt) || refreshed.FencingToken != lease.FencingToken {
				t.Fatalf("active refresh failed or changed token: %v", err)
			}
			// Force expiry in fixture DB rather than sleeping or changing clocks.
			if _, err := first.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, productionRouteKey); err != nil {
				t.Fatal(err)
			}
			if _, err := first.RefreshRouteLease(ctx, time.Minute); !errors.Is(err, ErrRouteLeaseLost) {
				t.Fatalf("expired lease refreshed: %v", err)
			}
			successor, err := second.AcquireRouteLease(ctx, productionRouteKey, pair[1], time.Minute)
			if err != nil || successor.FencingToken != lease.FencingToken+1 {
				t.Fatalf("takeover did not increment fence: %+v %v", successor, err)
			}
			// Simulate a predecessor returning with its exact old cached token.
			first.setLease(&lease)
			if released, err := first.ReleaseRouteLease(ctx); err != nil || released {
				t.Fatalf("stale release cleared successor: %v %v", released, err)
			}
			assertRow(t, successor)
			first.setLease(&lease)
			if _, err := first.RefreshRouteLease(ctx, time.Minute); !errors.Is(err, ErrRouteLeaseLost) {
				t.Fatalf("stale refresh extended successor: %v", err)
			}
			assertRow(t, successor)
			first.setLease(&lease)
			if err := first.AssertRouteLease(ctx, productionRouteKey); !errors.Is(err, ErrRouteLeaseLost) {
				t.Fatalf("stale authority assertion passed: %v", err)
			}
			if err := second.AssertRouteLease(ctx, productionRouteKey); err != nil {
				t.Fatal(err)
			}
			assertRow(t, successor)
			if released, err := second.ReleaseRouteLease(ctx); err != nil || !released {
				t.Fatalf("current owner exact release failed: %v %v", released, err)
			}
		})
	}
	t.Run("simultaneous-distinct-contenders", func(t *testing.T) {
		type result struct {
			db    *Database
			lease RouteLease
			err   error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for i, db := range []*Database{first, second} {
			owner := []string{legacy, neutral}[i]
			go func(db *Database, owner string) {
				<-start
				lease, err := db.AcquireRouteLease(ctx, productionRouteKey, owner, time.Minute)
				results <- result{db, lease, err}
			}(db, owner)
		}
		close(start)
		var winner *Database
		wins, losses := 0, 0
		for i := 0; i < 2; i++ {
			r := <-results
			if r.err == nil {
				wins++
				winner = r.db
				assertRow(t, r.lease)
			} else if errors.Is(r.err, ErrRouteLeaseUnavailable) {
				losses++
			} else {
				t.Fatal(r.err)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("expected one winner/one excluded contender: %d/%d", wins, losses)
		}
		if released, err := winner.ReleaseRouteLease(ctx); err != nil || !released {
			t.Fatalf("winner release failed: %v %v", released, err)
		}
	})
	t.Run("database-enforced-lease-constraints", func(t *testing.T) {
		for _, sql := range []string{
			`UPDATE loyal_yield.multiply_route_states SET lease_owner='fixture',lease_expires_at=NULL`,
			`UPDATE loyal_yield.multiply_route_states SET lease_owner=NULL,lease_expires_at=now()`,
			`UPDATE loyal_yield.multiply_route_states SET fencing_token=-1`,
			`UPDATE loyal_yield.multiply_route_states SET state_version=0`,
		} {
			_, err := first.pool.Exec(ctx, sql)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "23514" {
				t.Fatalf("expected PostgreSQL check violation, got %v", err)
			}
		}
	})
}
