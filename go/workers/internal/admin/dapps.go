package admin

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const dappsModule = "dapps/actions"

type DappsPage struct {
	Dapps []DappRow `json:"dapps" nullable:"false"`
}

type DappRow struct {
	ID           string    `json:"id"`
	Origin       string    `json:"origin"`
	Name         string    `json:"name"`
	StartURL     string    `json:"startUrl"`
	Category     *string   `json:"category"`
	DisplayOrder int       `json:"displayOrder"`
	IsActive     bool      `json:"isActive"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type DappActionResult struct {
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success,omitempty"`
}

type DappSeedResult struct {
	Inserted   int    `json:"inserted"`
	Backfilled int    `json:"backfilled"`
	Skipped    int    `json:"skipped"`
	Error      string `json:"error,omitempty"`
}

type dappsFields struct {
	origin, name, startURL string
	category               *string
	displayOrder           int32
	isActive               bool
	problem                string
}

var dappsLeadingInt = regexp.MustCompile(`^\s*[+-]?\d+`)

func dappsHTTPURL(raw string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil
	}
	u.Host = strings.ToLower(u.Host)
	if port := u.Port(); (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		u.Host = strings.ToLower(u.Hostname())
	}
	return u
}

func dappsReadForm(call Call) dappsFields {
	rawOrigin := call.FormValue("origin")
	rawStart := call.FormValue("startUrl")
	if rawStart == "" {
		rawStart = rawOrigin
	}
	f := dappsFields{
		name:     strings.TrimSpace(call.FormValue("name")),
		isActive: call.FormValue("isActive") == "on",
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(dappsLeadingInt.FindString(call.FormValue("displayOrder"))), 10, 32); err == nil {
		f.displayOrder = int32(n)
	}
	if category := strings.TrimSpace(call.FormValue("category")); category != "" {
		f.category = &category
	}
	origin := dappsHTTPURL(rawOrigin)
	start := dappsHTTPURL(rawStart)
	switch {
	case origin == nil:
		f.problem = "Origin must be a valid http(s) URL"
	case start == nil:
		f.problem = "Start URL must be a valid http(s) URL"
	case f.name == "":
		f.problem = "Name is required"
	default:
		f.origin = origin.Scheme + "://" + origin.Host
		if start.Path == "" {
			start.Path = "/"
		}
		f.startURL = start.String()
	}
	return f
}

func appCrudIsUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func dappsWriteResult(err error, failure string) (DappActionResult, error) {
	switch {
	case err == nil:
		return DappActionResult{Success: true}, nil
	case appCrudIsUnique(err):
		return DappActionResult{Error: "A dApp with this origin already exists"}, nil
	default:
		return DappActionResult{Error: failure}, nil
	}
}

func loadDapps(ctx context.Context, d *Deps, _ PageRequest) (DappsPage, error) {
	page := DappsPage{Dapps: []DappRow{}}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, origin, name, start_url, category, display_order, is_active,
created_at AT TIME ZONE 'UTC', updated_at AT TIME ZONE 'UTC'
FROM trusted_dapps ORDER BY display_order, name`)
		if err != nil {
			return err
		}
		page.Dapps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[DappRow])
		return err
	})
	return page, err
}

func registerDapps(m *Mux) {
	Page(m, "/dapps", loadDapps)

	Action(m, dappsModule, "addTrustedDapp", func(ctx context.Context, d *Deps, call Call) (DappActionResult, error) {
		f := dappsReadForm(call)
		if f.problem != "" {
			return DappActionResult{Error: f.problem}, nil
		}
		err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO trusted_dapps (origin, name, start_url, category, display_order, is_active)
VALUES ($1, $2, $3, $4, $5, $6)`, f.origin, f.name, f.startURL, f.category, f.displayOrder, f.isActive)
			return err
		})
		return dappsWriteResult(err, "Failed to add dApp")
	})

	Action(m, dappsModule, "updateTrustedDapp", func(ctx context.Context, d *Deps, call Call) (DappActionResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return DappActionResult{}, err
		}
		f := dappsReadForm(call)
		if f.problem != "" {
			return DappActionResult{Error: f.problem}, nil
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE trusted_dapps
SET origin = $2, name = $3, start_url = $4, category = $5, display_order = $6, is_active = $7, updated_at = now()
WHERE id = $1`, id, f.origin, f.name, f.startURL, f.category, f.displayOrder, f.isActive)
			return err
		})
		return dappsWriteResult(err, "Failed to update dApp")
	})

	Action(m, dappsModule, "deleteTrustedDapp", func(ctx context.Context, d *Deps, call Call) (DappActionResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return DappActionResult{}, err
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM trusted_dapps WHERE id = $1`, id)
			return err
		})
		return DappActionResult{Success: true}, err
	})

	Action(m, dappsModule, "toggleTrustedDappActive", func(ctx context.Context, d *Deps, call Call) (DappActionResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return DappActionResult{}, err
		}
		var isActive bool
		if err := call.Arg(1, &isActive); err != nil {
			return DappActionResult{}, err
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE trusted_dapps SET is_active = $2, updated_at = now() WHERE id = $1`, id, isActive)
			return err
		})
		return DappActionResult{Success: true}, err
	})

	Action(m, dappsModule, "seedAllowlist", func(ctx context.Context, d *Deps, _ Call) (DappSeedResult, error) {
		var result DappSeedResult
		err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			result = DappSeedResult{}
			rows, err := tx.Query(ctx, `SELECT origin, category FROM trusted_dapps`)
			if err != nil {
				return err
			}
			categories := map[string]*string{}
			for rows.Next() {
				var origin string
				var category *string
				if err := rows.Scan(&origin, &category); err != nil {
					return err
				}
				categories[origin] = category
			}
			if err := rows.Err(); err != nil {
				return err
			}
			for _, entry := range dappsAllowlistSeed {
				origin := "https://" + entry.host
				category, found := categories[origin]
				switch {
				case !found:
					if _, err := tx.Exec(ctx, `INSERT INTO trusted_dapps (origin, name, start_url, category, display_order, is_active)
VALUES ($1, $2, $1, $3, $4, true)`, origin, entry.name, entry.category, entry.displayOrder); err != nil {
						return err
					}
					result.Inserted++
				case category == nil:
					if _, err := tx.Exec(ctx, `UPDATE trusted_dapps SET category = $2, updated_at = now() WHERE origin = $1`, origin, entry.category); err != nil {
						return err
					}
					result.Backfilled++
				default:
					result.Skipped++
				}
			}
			return nil
		})
		return result, err
	})
}
