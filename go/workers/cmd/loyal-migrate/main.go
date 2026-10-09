// Command loyal-migrate applies the repository's SQL migrations to the Yield
// (Neon) and Timescale databases. It keeps the ledger the retired Rust runners
// wrote: version, name and the lowercase hex SHA-256 of the file bytes, in
// loyal_yield.schema_migrations and loyal.timescale_schema_migrations.
//
//	loyal-migrate -db yield status   # read-only: applied, pending, checksum drift
//	loyal-migrate -db yield up       # apply pending migrations in version order
//
// The URL comes from NEON_DATABASE_URL (yield) or TIMESCALEDB_URL (timescale).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type database struct{ env, ledger string }

var databases = map[string]database{
	"yield":     {env: "NEON_DATABASE_URL", ledger: "loyal_yield.schema_migrations"},
	"timescale": {env: "TIMESCALEDB_URL", ledger: "loyal.timescale_schema_migrations"},
}

type migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

var fileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

func load(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int64]string{}
	for _, entry := range entries {
		match := fileName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("unexpected file %s in %s", entry.Name(), dir)
		}
		version, _ := strconv.ParseInt(match[1], 10, 64)
		if prior, ok := seen[version]; ok {
			return nil, fmt.Errorf("version %d used by %s and %s", version, prior, entry.Name())
		}
		seen[version] = entry.Name()
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{Version: version, Name: match[2], SQL: string(raw), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

type plan struct {
	Applied, Pending []migration
	Drift            []string
}

func compare(files []migration, ledger map[int64]string) plan {
	var p plan
	known := map[int64]bool{}
	for _, m := range files {
		known[m.Version] = true
		switch sum, ok := ledger[m.Version]; {
		case !ok:
			p.Pending = append(p.Pending, m)
		case sum != m.Checksum:
			p.Drift = append(p.Drift, fmt.Sprintf("%04d_%s applied with checksum %s, file is %s", m.Version, m.Name, sum, m.Checksum))
		default:
			p.Applied = append(p.Applied, m)
		}
	}
	var unknown []int64
	for version := range ledger {
		if !known[version] {
			unknown = append(unknown, version)
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i] < unknown[j] })
	for _, version := range unknown {
		p.Drift = append(p.Drift, fmt.Sprintf("ledger version %d has no migration file", version))
	}
	return p
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readLedger(ctx context.Context, q querier, table string) (map[int64]string, error) {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return nil, err
	}
	ledger := map[int64]string{}
	if !exists {
		return ledger, nil
	}
	rows, err := q.Query(ctx, "SELECT version, checksum FROM "+table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var sum string
		if err := rows.Scan(&version, &sum); err != nil {
			return nil, err
		}
		ledger[version] = sum
	}
	return ledger, rows.Err()
}

func status(ctx context.Context, conn *pgx.Conn, db database, files []migration, out io.Writer) (plan, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return plan{}, err
	}
	defer tx.Rollback(ctx)
	ledger, err := readLedger(ctx, tx, db.ledger)
	if err != nil {
		return plan{}, err
	}
	p := compare(files, ledger)
	for _, m := range p.Pending {
		fmt.Fprintf(out, "pending %04d_%s %s\n", m.Version, m.Name, m.Checksum)
	}
	for _, d := range p.Drift {
		fmt.Fprintln(out, "drift", d)
	}
	fmt.Fprintf(out, "%s: %d files, %d applied with matching checksum, %d pending, %d drift\n", db.ledger, len(files), len(p.Applied), len(p.Pending), len(p.Drift))
	if len(p.Drift) > 0 {
		return p, errors.New("migration ledger drifted from migration files")
	}
	return p, nil
}

// statements splits a CREATE INDEX CONCURRENTLY migration: PostgreSQL rejects
// it inside the implicit transaction of a multi-statement query.
func statements(sql string) []string {
	var out []string
	for _, statement := range strings.Split(sql, ";") {
		if body := strings.TrimSpace(statement); body != "" && !onlyComments(body) {
			out = append(out, body)
		}
	}
	return out
}

func onlyComments(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

var concurrently = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)

func apply(ctx context.Context, conn *pgx.Conn, db database, m migration) error {
	record := "INSERT INTO " + db.ledger + " (version, name, checksum) VALUES ($1, $2, $3)"
	if concurrently.MatchString(m.SQL) {
		for _, statement := range statements(m.SQL) {
			if _, err := conn.Exec(ctx, statement); err != nil {
				return err
			}
		}
		_, err := conn.Exec(ctx, record, m.Version, m.Name, m.Checksum)
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, record, m.Version, m.Name, m.Checksum)
		return err
	})
}

func up(ctx context.Context, conn *pgx.Conn, db database, files []migration, out io.Writer) error {
	schema, _, _ := strings.Cut(db.ledger, ".")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema+"; CREATE TABLE IF NOT EXISTS "+db.ledger+
		" (version BIGINT PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	p, err := status(ctx, conn, db, files, out)
	if err != nil {
		return err
	}
	for _, m := range p.Pending {
		fmt.Fprintf(out, "applying %04d_%s\n", m.Version, m.Name)
		if err := apply(ctx, conn, db, m); err != nil {
			return fmt.Errorf("%04d_%s: %w", m.Version, m.Name, err)
		}
	}
	fmt.Fprintf(out, "%s up to date\n", db.ledger)
	return nil
}

// defaultDir finds migrations/<db> in the working directory or an ancestor,
// so the command runs from the repository root or from go/workers.
func defaultDir(name string) string {
	dir, err := os.Getwd()
	if err != nil {
		return filepath.Join("migrations", name)
	}
	for {
		candidate := filepath.Join(dir, "migrations", name)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join("migrations", name)
		}
		dir = parent
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("loyal-migrate", flag.ContinueOnError)
	name := flags.String("db", "yield", "yield or timescale")
	dir := flags.String("dir", "", "migration directory (default: nearest migrations/<db>)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	db, ok := databases[*name]
	if !ok || flags.NArg() != 1 || (flags.Arg(0) != "status" && flags.Arg(0) != "up") {
		return errors.New("usage: loyal-migrate [-db yield|timescale] [-dir path] status|up")
	}
	if *dir == "" {
		*dir = defaultDir(*name)
	}
	files, err := load(*dir)
	if err != nil {
		return err
	}
	url := os.Getenv(db.env)
	if url == "" {
		return fmt.Errorf("%s is not set", db.env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if flags.Arg(0) == "status" {
		_, err = status(ctx, conn, db, files, out)
		return err
	}
	return up(ctx, conn, db, files, out)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "loyal-migrate:", err)
		os.Exit(1)
	}
}
