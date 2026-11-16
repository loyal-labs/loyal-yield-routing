package backyard

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

func TestSelectorDatabaseWriterWaitKeepsStatementBudget(t *testing.T) {
	control := withdrawalCostTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := control.pool.Exec(ctx, `CREATE TABLE loyal_yield.multiply_route_states (
		route_key text PRIMARY KEY, state jsonb NOT NULL DEFAULT '{}', updated_at timestamptz);
		INSERT INTO loyal_yield.multiply_route_states(route_key) VALUES ('selector-wait-test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := control.pool.Exec(context.Background(), `DROP TABLE loyal_yield.multiply_route_states`); err != nil {
			t.Error(err)
		}
	})
	db, err := OpenDatabase(ctx, engine.DirectDSN(os.Getenv("BACKYARD_WITHDRAWAL_TEST_DSN")), rwa.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	input := func() SelectorInput {
		in := selectorFixture()
		advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
		in.Snapshot.MonitorsArmed = true
		in.Markets, in.Quotes = nil, nil
		return in
	}
	for _, stale := range []bool{false, true} {
		name := "writer_wait"
		if stale {
			name = "stale_after_wait"
		}
		t.Run(name, func(t *testing.T) {
			conn, err := db.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			in := input()
			if stale {
				in.Now = in.Now.Add(-4 * time.Second)
			}
			done := make(chan error, 1)
			go func() {
				_, err := db.RecordSelectorEvaluation(ctx, "selector-wait-test", in, in.Snapshot.Slot)
				done <- err
			}()
			time.Sleep(1200 * time.Millisecond)
			conn.Release()
			err = <-done
			if stale {
				assertPreSendHold(t, err, selectorEvaluationNotCurrent)
			} else if err != nil {
				t.Fatalf("waiting for the writer consumed the statement budget: %v", err)
			}
		})
	}
	t.Run("caller_deadline_bounds_wait", func(t *testing.T) {
		conn, err := db.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		waiting, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		defer cancel()
		in := input()
		_, err = db.RecordSelectorEvaluation(waiting, "selector-wait-test", in, in.Snapshot.Slot)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("writer wait ignored caller cancellation: %v", err)
		}
	})
	t.Run("statement_still_bounded", func(t *testing.T) {
		tx, err := control.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key='selector-wait-test' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		in, started := input(), time.Now()
		_, err = db.RecordSelectorEvaluation(ctx, "selector-wait-test", in, in.Snapshot.Slot)
		if sanitizedSelectorEvaluateFailure(err) != "selector_evaluate_query_cancelled" || time.Since(started) > 3*time.Second {
			t.Fatalf("statement did not retain its one-second bound: %v (%s)", err, time.Since(started))
		}
		if err := db.pool.Ping(ctx); err != nil {
			t.Fatalf("statement cancellation lost the writer session: %v", err)
		}
	})
}
