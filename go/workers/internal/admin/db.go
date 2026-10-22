package admin

import (
	"context"
	"fmt"
	"net/url"
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

	LibraryUploadToken string
	AppAPIBaseURL      *url.URL
	ClickStackReadKey  string
	ExpoAccessToken    string
}

const readTimeout = 30 * time.Second

func Disabled(what string) error {
	return huma.Error503ServiceUnavailable(fmt.Sprintf("%s is not configured on this host", what))
}

func readOnly(ctx context.Context, pool *pgxpool.Pool, what string, fn func(pgx.Tx) error) error {
	if pool == nil {
		return Disabled(what)
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return db.WithTx(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", readTimeout.Milliseconds())); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (d *Deps) YieldRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.Yield, "the yield database", fn)
}

func (d *Deps) TimescaleRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.Timescale, "the Timescale database", fn)
}

func (d *Deps) AppRead(ctx context.Context, fn func(pgx.Tx) error) error {
	return readOnly(ctx, d.App, "the app database", fn)
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
