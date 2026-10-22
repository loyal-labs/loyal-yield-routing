package stats

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const (
	vaultsFresh   = 5 * time.Minute
	vaultsStale   = time.Minute
	vaultsTimeout = 10 * time.Second
	treasuryVault = "F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e"
)

type vaultsBody struct {
	Vaults    []string `json:"vaults"`
	Count     int      `json:"count"`
	UpdatedAt string   `json:"updatedAt"`
}

type vaultsPage struct {
	body    []byte
	fetched time.Time
}

type vaultList struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	last vaultsPage
}

func newVaultList(pool *pgxpool.Pool) *vaultList {
	return &vaultList{pool: pool}
}

func (v *vaultList) page(ctx context.Context, now time.Time) (vaultsPage, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := now.Sub(v.last.fetched)
	if v.last.body != nil && age < vaultsFresh {
		return v.last, true
	}
	body, err := v.read(context.WithoutCancel(ctx), now)
	if err == nil {
		v.last = vaultsPage{body: body, fetched: now}
		return v.last, true
	}
	slog.Warn("vault list unavailable", "error", err)
	if v.last.body != nil && age < vaultsFresh+vaultsStale {
		return v.last, true
	}
	return vaultsPage{}, false
}

func (v *vaultList) read(ctx context.Context, now time.Time) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, vaultsTimeout)
	defer cancel()
	vaults := []string{}
	err := db.WithTx(ctx, v.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(vaultsTimeout.Milliseconds(), 10)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT vault_pubkey
FROM loyal_yield.user_yield_positions
WHERE status = 'active'
  AND vault_pubkey IS NOT NULL
  AND length(vault_pubkey) BETWEEN 32 AND 44
ORDER BY vault_pubkey
LIMIT 50000`)
		if err != nil {
			return err
		}
		vaults, err = pgx.AppendRows(vaults, rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	if !slices.Contains(vaults, treasuryVault) {
		vaults = append(vaults, treasuryVault)
	}
	return json.Marshal(vaultsBody{
		Vaults:    vaults,
		Count:     len(vaults),
		UpdatedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

func (v *vaultList) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	page, ok := v.page(r.Context(), time.Now())
	if !ok {
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(`{"error":"vault list unavailable"}`))
		}
		return
	}
	h.Set("Cache-Control", "public, s-maxage=300, stale-while-revalidate=60, max-age=60")
	h.Set("CDN-Cache-Control", "public, s-maxage=300")
	h.Set("Content-Length", strconv.Itoa(len(page.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page.body)
	}
}
