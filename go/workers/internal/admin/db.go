package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type Deps struct {
	App       *pgxpool.Pool
	Yield     *pgxpool.Pool
	Timescale *pgxpool.Pool

	R2                *R2Config
	ClickStackReadKey string
}

const readTimeout = 30 * time.Second

var SessionTimeoutOptions = fmt.Sprintf("-c statement_timeout=%d", readTimeout.Milliseconds())

func CheckSessionTimeout(ctx context.Context, pool *pgxpool.Pool) error {
	var milliseconds int64
	if err := pool.QueryRow(ctx, `SELECT setting::bigint FROM pg_settings WHERE name = 'statement_timeout'`).Scan(&milliseconds); err != nil {
		return err
	}
	if milliseconds != readTimeout.Milliseconds() {
		return fmt.Errorf("statement_timeout is %d ms, want %d ms", milliseconds, readTimeout.Milliseconds())
	}
	return nil
}

func Disabled(what string) error {
	return huma.Error503ServiceUnavailable(fmt.Sprintf("%s is not configured on this host", what))
}

func readOnly(ctx context.Context, pool *pgxpool.Pool, what string, sessionTimeout bool, fn func(pgx.Tx) error) error {
	if pool == nil {
		return Disabled(what)
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return db.WithTx(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if !sessionTimeout {
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", readTimeout.Milliseconds())); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

func (d *Deps) YieldRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.Yield, "the yield database", true, fn)
}

func (d *Deps) TimescaleRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.Timescale, "the Timescale database", false, fn)
}

func (d *Deps) AppRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.App, "the app database", false, fn)
}

func (d *Deps) AppWrite(ctx context.Context, fn func(pgx.Tx) error) error {
	if d.App == nil {
		return Disabled("the app database")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return db.WithTx(ctx, d.App, pgx.TxOptions{}, fn)
}

func (d *Deps) Require(what, value string) error {
	if value == "" {
		return Disabled(what)
	}
	return nil
}
