package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The production ledgers were written by the Rust runners. These rows are
// copied from them; a checksum change here would make every deploy refuse.
func TestChecksumsMatchProductionLedgerRows(t *testing.T) {
	for dir, want := range map[string]map[int64]string{
		"../../../../migrations/yield": {
			8:  "d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34",
			85: "7cec74a15563b3a9406c5f6b5f42139a3dc7e834ecd480002d1edf18086b76d3",
			86: "07d46b137dafb60d5dce35f32d16a74dcab9fb2ec9c4b1730a9c815f1f8a9c14",
		},
		"../../../../migrations/timescale": {
			8: "52c59d576890c355a2737655a4a461c5768e173816724545d388faca0b8b20ad",
		},
	} {
		files, err := load(dir)
		if err != nil {
			t.Fatal(err)
		}
		byVersion := map[int64]migration{}
		for _, m := range files {
			byVersion[m.Version] = m
		}
		for version, sum := range want {
			m, ok := byVersion[version]
			if !ok {
				t.Fatalf("%s: version %d missing", dir, version)
			}
			if m.Checksum != sum {
				t.Fatalf("%s: version %d checksum %s, ledger has %s", dir, version, m.Checksum, sum)
			}
		}
	}
}

func TestCompareSeparatesPendingFromDrift(t *testing.T) {
	files := []migration{{Version: 1, Checksum: "a"}, {Version: 2, Checksum: "b"}, {Version: 3, Checksum: "c"}}
	p := compare(files, map[int64]string{1: "a", 2: "x", 9: "z"})
	if len(p.Applied) != 1 || len(p.Pending) != 1 || p.Pending[0].Version != 3 || len(p.Drift) != 2 {
		t.Fatalf("unexpected plan %+v", p)
	}
}

// MIGRATE_TEST_DATABASE_URL must name an empty disposable database.
func TestUpAndStatusAgainstDisposableDatabase(t *testing.T) {
	url := os.Getenv("MIGRATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MIGRATE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	reset := func() {
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS loyal_yield CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	defer reset()

	dir := t.TempDir()
	write := func(name, sql string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0001_table.sql", "CREATE SCHEMA IF NOT EXISTS loyal_yield;\nCREATE TABLE loyal_yield.t (id BIGINT);\n")
	write("0002_index.sql", "-- autocommit\nCREATE INDEX CONCURRENTLY t_a ON loyal_yield.t (id);\nCREATE INDEX CONCURRENTLY t_b ON loyal_yield.t (id DESC);\n")
	db := databases["yield"]
	files, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	p, err := status(ctx, conn, db, files, &out)
	if err != nil || len(p.Pending) != 2 {
		t.Fatalf("status before up: %v %+v", err, p)
	}
	if err := up(ctx, conn, db, files, &out); err != nil {
		t.Fatal(err, out.String())
	}
	var indexes int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'loyal_yield' AND tablename = 't'").Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("indexes %d %v", indexes, err)
	}
	var rows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM loyal_yield.schema_migrations WHERE (version, name, checksum) IN (($1, 'table', $2), ($3, 'index', $4))",
		files[0].Version, files[0].Checksum, files[1].Version, files[1].Checksum).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("ledger rows %d %v", rows, err)
	}
	if p, err := status(ctx, conn, db, files, &out); err != nil || len(p.Pending) != 0 || len(p.Applied) != 2 {
		t.Fatalf("status after up: %v %+v", err, p)
	}

	// A failing migration leaves neither schema nor ledger row behind.
	write("0003_broken.sql", "CREATE TABLE loyal_yield.u (id BIGINT);\nSELECT 1/0;\n")
	files, _ = load(dir)
	if err := up(ctx, conn, db, files, &out); err == nil {
		t.Fatal("broken migration applied")
	}
	var leaked bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('loyal_yield.u') IS NOT NULL OR EXISTS (SELECT 1 FROM loyal_yield.schema_migrations WHERE version = 3)").Scan(&leaked); err != nil || leaked {
		t.Fatalf("failed migration leaked state: %v", err)
	}

	// An edited applied file is drift: status and up both refuse.
	write("0003_broken.sql", "SELECT 1;\n")
	write("0001_table.sql", "CREATE SCHEMA IF NOT EXISTS loyal_yield;\nCREATE TABLE loyal_yield.t (id BIGINT, x INT);\n")
	files, _ = load(dir)
	if _, err := status(ctx, conn, db, files, &out); err == nil {
		t.Fatal("status accepted an edited applied migration")
	}
	if err := up(ctx, conn, db, files, &out); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("up accepted drift: %v", err)
	}
}
