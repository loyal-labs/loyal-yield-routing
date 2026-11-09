package webapp

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

func TestExportWindowStartClampsToTrackLimit(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ watermark, want time.Time }{
		{time.Date(2026, 11, 9, 11, 30, 0, 0, time.UTC), time.Date(2026, 11, 9, 10, 30, 0, 0, time.UTC)},
		{time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
	} {
		if got := exportWindowStart(c.watermark, now); !got.Equal(c.want) {
			t.Fatalf("exportWindowStart(%v) = %v, want %v", c.watermark, got, c.want)
		}
	}
}

func earnEventsTestCrons(t *testing.T, tracker *MixpanelTracker) (Crons, *pgxpool.Pool) {
	t.Helper()
	pool := joyTestPool(t)
	_, err := pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS loyal_yield CASCADE;
DROP TABLE IF EXISTS analytics_export_watermarks;
CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.user_yield_position_deposits (
	id bigint PRIMARY KEY, wallet_address text NOT NULL, vault_index smallint NOT NULL,
	principal_amount_raw bigint NOT NULL, confirmed_at timestamptz NOT NULL, balance_sweep_execution_id bigint);
CREATE TABLE loyal_yield.user_yield_position_withdrawals (
	id bigint PRIMARY KEY, wallet_address text NOT NULL, vault_index smallint NOT NULL,
	withdrawn_amount_raw bigint NOT NULL, source_type text, confirmed_at timestamptz NOT NULL);
CREATE TABLE analytics_export_watermarks (
	stream text PRIMARY KEY, confirmed_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		t.Fatal(err)
	}
	crons := NewCrons(pool, Earn{YieldDB: pool})
	crons.Mixpanel = tracker
	return crons, pool
}

func watermarkCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM analytics_export_watermarks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEarnEventsExportSeedsThenExports(t *testing.T) {
	ctx := context.Background()
	tracker, requests := mixpanelStub(t)
	crons, pool := earnEventsTestCrons(t, tracker)

	// A dry run before the seed writes nothing.
	summary, err := crons.EarnEventsMixpanel(ctx, true)
	if err != nil || !summary.DryRun || !summary.Seeded || watermarkCount(t, pool) != 0 {
		t.Fatalf("dry run seeded: %+v %v", summary, err)
	}
	summary, err = crons.EarnEventsMixpanel(ctx, false)
	if err != nil || !summary.Seeded || summary.Deposits != 0 || summary.Withdrawals != 0 || len(requests()) != 0 || watermarkCount(t, pool) != 2 {
		t.Fatalf("first run: %+v %v", summary, err)
	}

	// Rows confirmed after the seed: one sweep deposit, one other-vault deposit, one withdrawal.
	vault := int16(earnprep.EarnVaultIndex)
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.user_yield_position_deposits VALUES
	(7, 'W1', $1, 25000000, now(), 99), (8, 'W2', $1 + 1, 5000000, now(), NULL)`, vault); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.user_yield_position_withdrawals VALUES (3, 'W1', $1, 10500000, 'user', now())`, vault); err != nil {
		t.Fatal(err)
	}
	summary, err = crons.EarnEventsMixpanel(ctx, true)
	if err != nil || summary.Deposits != 1 || summary.Withdrawals != 1 || len(requests()) != 0 {
		t.Fatalf("dry run with rows: %+v %v", summary, err)
	}
	summary, err = crons.EarnEventsMixpanel(ctx, false)
	if err != nil || summary.Seeded || summary.Deposits != 1 || summary.Withdrawals != 1 {
		t.Fatalf("export: %+v %v", summary, err)
	}
	got := requests()
	if len(got) != 2 {
		t.Fatalf("requests = %v", got)
	}
	deposit, withdrawal := got[0][0], got[1][0]
	props, _ := deposit["properties"].(map[string]any)
	if deposit["event"] != "Earn Deposit Confirmed" || props["amount_usd"] != float64(25) || props["source"] != "autodeposit" ||
		props["$insert_id"] != "earn-deposit:7" || props["distinct_id"] != "mob:W1" || props["deposit_id"] != float64(7) {
		t.Fatalf("deposit = %v", deposit)
	}
	props, _ = withdrawal["properties"].(map[string]any)
	if withdrawal["event"] != "Earn Withdrawal Confirmed" || props["amount_usd"] != 10.5 || props["source"] != "manual" ||
		props["source_type"] != "user" || props["$insert_id"] != "earn-withdrawal:3" || props["withdrawal_id"] != float64(3) {
		t.Fatalf("withdrawal = %v", withdrawal)
	}
}

func TestEarnEventsExportKeepsWatermarkWhenTrackFails(t *testing.T) {
	ctx := context.Background()
	tracker, requests := mixpanelStub(t, 400)
	crons, pool := earnEventsTestCrons(t, tracker)
	if _, err := crons.EarnEventsMixpanel(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.user_yield_position_deposits VALUES (1, 'W1', $1, 1000000, now(), NULL)`, int16(earnprep.EarnVaultIndex)); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT confirmed_at FROM analytics_export_watermarks WHERE stream = 'earn-deposits'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := crons.EarnEventsMixpanel(ctx, false); err == nil {
		t.Fatal("a rejected batch must fail the run")
	}
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT confirmed_at FROM analytics_export_watermarks WHERE stream = 'earn-deposits'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) || len(requests()) != 1 {
		t.Fatalf("watermark moved %v -> %v, requests %d", before, after, len(requests()))
	}
}
