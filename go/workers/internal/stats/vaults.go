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
	vaultsBackoff = 5 * time.Second
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
	pool     *pgxpool.Pool
	mu       sync.Mutex
	last     vaultsPage
	inflight chan struct{}
	failed   time.Time
}

func newVaultList(pool *pgxpool.Pool) *vaultList {
	return &vaultList{pool: pool}
}

// page never holds mu across the read. A usable page is returned at once while
// the single refresh runs detached; without one the caller waits for that
// refresh or its own ctx, and a failed refresh is not retried for vaultsBackoff.
func (v *vaultList) page(ctx context.Context) (vaultsPage, bool) {
	v.mu.Lock()
	now := time.Now()
	age := now.Sub(v.last.fetched)
	if v.last.body != nil && age < vaultsFresh {
		last := v.last
		v.mu.Unlock()
		return last, true
	}
	if v.inflight == nil && now.Sub(v.failed) >= vaultsBackoff {
		v.inflight = make(chan struct{})
		go v.refresh(v.inflight)
	}
	last, inflight := v.last, v.inflight
	v.mu.Unlock()
	if last.body != nil && age < vaultsFresh+vaultsStale {
		return last, true
	}
	if inflight == nil {
		return vaultsPage{}, false
	}
	select {
	case <-inflight:
		return v.page(ctx)
	case <-ctx.Done():
		return vaultsPage{}, false
	}
}

func (v *vaultList) refresh(done chan struct{}) {
	now := time.Now()
	body, err := v.read(context.Background(), now)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		slog.Warn("vault list unavailable", "error", err)
		v.failed = time.Now()
	} else {
		v.last = vaultsPage{body: body, fetched: now}
	}
	v.inflight = nil
	close(done)
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
	page, ok := v.page(r.Context())
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
