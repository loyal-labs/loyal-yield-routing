package backyard

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestForeignInflowStoreCommitsEvidenceAndCursorTogether(t *testing.T) {
	dsn := os.Getenv("BACKYARD_FOREIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local backyard_foreign_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "backyard_foreign_test") {
		t.Fatal("requires a disposable local backyard_foreign_test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA loyal_yield`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	migration, err := os.ReadFile("../../../../migrations/yield/0125_backyard_foreign_inflows.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	db := &Database{pool: pool}
	address := rwa.idleATA
	anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
	seed := foreignEvidence{After: anchor}
	if err := db.saveForeignEvidence(ctx, rwa.RouteKey, address, seed); err != nil {
		t.Fatal(err)
	}
	x := foreignFixture(address, 17, 1)
	e, err := advanceForeignCheckpoint(rwa, address, x.Transaction.Signatures[0].String(), anchor, x)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.saveForeignEvidence(ctx, rwa.RouteKey, address, e); err != nil {
			t.Fatal(err)
		}
	}
	got, exists, err := db.foreignCheckpoint(ctx, rwa.RouteKey, address)
	if err != nil || !exists || got != e.After {
		t.Fatalf("checkpoint: %+v %v", got, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.backyard_foreign_inflows`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("replay duplicated evidence: %d %v", count, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE loyal_yield.backyard_foreign_inflows ADD CONSTRAINT fail_evidence CHECK (signature <> 'reject')`); err != nil {
		t.Fatal(err)
	}
	rejected := foreignEvidence{Before: got, After: got}
	rejected.After.Signature = "reject"
	rejected.After.Slot++
	rejected.After.ForeignRaw++
	if err := db.saveForeignEvidence(ctx, rwa.RouteKey, address, rejected); err == nil {
		t.Fatal("expected evidence insert to fail")
	}
	after, _, err := db.foreignCheckpoint(ctx, rwa.RouteKey, address)
	if err != nil || after != got {
		t.Fatalf("failed evidence advanced checkpoint: %+v %v", after, err)
	}
	rejected.Before = anchor
	rejected.After.Signature = "stale"
	if err := db.saveForeignEvidence(ctx, rwa.RouteKey, address, rejected); err == nil {
		t.Fatal("stale cursor overwrote journal")
	}
}
