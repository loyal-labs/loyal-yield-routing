package earnmax

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Reader struct {
	chain *chain.Client
	yield Querier
	now   func() time.Time

	historyMu sync.Mutex
	history   map[solana.PublicKey]map[solana.Signature][]Entry
	apy       StaleWhileRevalidate[APY]
}

func NewReader(client *chain.Client, yield Querier) *Reader {
	return &Reader{
		chain:   client,
		yield:   yield,
		now:     time.Now,
		history: map[solana.PublicKey]map[solana.Signature][]Entry{},
		apy:     StaleWhileRevalidate[APY]{Fresh: apyCacheFor, Stale: apyCacheFor, Wait: apyTimeout, Budget: apyTimeout},
	}
}

type Authority struct{ key solana.PublicKey }

func AuthorityOf(settings solana.PublicKey) (Authority, error) {
	key, _, err := squads.SmartAccountAddress(settings, AccountIndex)
	return Authority{key}, err
}

func (a Authority) Key() solana.PublicKey { return a.key }

var errNoYield = errors.New("the yield database is not configured")

func (r *Reader) routeState(ctx context.Context, field string) ([]byte, error) {
	if r.yield == nil {
		return nil, errNoYield
	}
	var raw []byte
	err := r.yield.QueryRow(ctx, `SELECT state->`+field+` FROM loyal_yield.multiply_route_states WHERE route_key = $1`, RouteKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

func retryRateLimited[T any](ctx context.Context, call func() (T, error)) (T, error) {
	value, err := call()
	if !errors.Is(err, chain.ErrRateLimited) {
		return value, err
	}
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return value, ctx.Err()
	}
	return call()
}

func signed[T ~int64](raw uint64) (T, error) {
	if raw > math.MaxInt64 {
		return 0, fmt.Errorf("amount %d overflows int64", raw)
	}
	return T(raw), nil
}

func iso(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func roundHalfUp(x float64) int64 {
	return int64(math.Floor(x + 0.5))
}
