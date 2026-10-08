package db

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RequireTables checks a family's actual prerequisites without applying migrations.
func RequireTables(ctx context.Context, pool *pgxpool.Pool, tables ...string) error {
	if pool == nil {
		return fmt.Errorf("missing database pool")
	}
	for _, table := range tables {
		var present bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&present); err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("required schema object unavailable: %s", table)
		}
	}
	return nil
}
