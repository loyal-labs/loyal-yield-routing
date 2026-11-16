package jupiter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type requestClassKey struct{}

// Background marks speculative work; it cannot spend execution or valuation capacity.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestClassKey{}, "background")
}

// Valuation protects the quotes needed to keep NAV and withdrawal evidence fresh.
func Valuation(ctx context.Context) context.Context {
	if requestClass(ctx) == "background" {
		return ctx
	}
	return context.WithValue(ctx, requestClassKey{}, "valuation")
}

func requestClass(ctx context.Context) string {
	if class, _ := ctx.Value(requestClassKey{}).(string); class == "background" || class == "valuation" {
		return class
	}
	return "execution"
}

// DeferredError means no HTTP request was sent. It must never masquerade as an upstream 429.
type DeferredError struct{ Reason string }

func (e *DeferredError) Error() string { return "Jupiter request deferred: " + e.Reason }

type RequestMetrics interface {
	JupiterRequest(class, endpoint, outcome string, seconds float64)
	JupiterDeferred(class, reason string)
	JupiterQuota(current, remaining, reset float64)
	JupiterCoalesced()
}

// Budget uses a separate, small pool: Backyard's single writer connection may
// already be in a transaction when a quote is requested.
type Budget struct {
	pool    *pgxpool.Pool
	metrics RequestMetrics
}

func NewBudget(ctx context.Context, pool *pgxpool.Pool, metrics RequestMetrics) (*Budget, error) {
	if pool == nil || metrics == nil {
		return nil, errors.New("Jupiter budget requires a pool and metrics")
	}
	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM loyal_yield.jupiter_request_budget WHERE scope='loyal'`).Scan(&scope); err != nil {
		return nil, errors.New("Jupiter budget unavailable: apply Yield migration 0145 before activation")
	}
	return &Budget{pool: pool, metrics: metrics}, nil
}

type budgetTransport struct {
	budget *Budget
	base   http.RoundTripper
}

func (b *Budget) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &budgetTransport{budget: b, base: base}
}

// Each class is paced independently across processes: 2 execution + 2 valuation
// + 4 background requests/second. No accumulated burst; 20% of the paid 10 RPS
// allowance remains unused for clock/network variation and other consumers.
func classInterval(class string) time.Duration {
	if class == "background" {
		return 250 * time.Millisecond
	}
	return 500 * time.Millisecond
}

func (b *Budget) take(ctx context.Context, class string) (time.Duration, bool, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, false, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var cooldown, next, now time.Time
	err = tx.QueryRow(ctx, `SELECT cooldown_until,
GREATEST(request_at, CASE $1 WHEN 'background' THEN background_at WHEN 'valuation' THEN valuation_at ELSE execution_at END),
clock_timestamp() FROM loyal_yield.jupiter_request_budget WHERE scope='loyal' FOR UPDATE`, class).Scan(&cooldown, &next, &now)
	if err != nil {
		return 0, false, err
	}
	// No HTTP call or sleep holds this row lock. Canceled waiters reserve nothing.
	if cooldown.After(now) || next.After(now) {
		wait := max(cooldown.Sub(now), next.Sub(now))
		return wait, cooldown.After(now), tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.jupiter_request_budget SET
request_at=$3,
execution_at=CASE WHEN $1='execution' THEN $2 ELSE execution_at END,
valuation_at=CASE WHEN $1='valuation' THEN $2 ELSE valuation_at END,
background_at=CASE WHEN $1='background' THEN $2 ELSE background_at END WHERE scope='loyal'`, class, now.Add(classInterval(class)), now.Add(125*time.Millisecond))
	if err != nil {
		return 0, false, err
	}
	return 0, false, tx.Commit(ctx)
}

func (b *Budget) admit(ctx context.Context, class string) error {
	// A quote must remain useful to its caller; don't queue it behind seconds of scans.
	bounded, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	for {
		if err := bounded.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &DeferredError{Reason: "budget"}
		}
		wait, cooldown, err := b.take(bounded, class)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if bounded.Err() != nil {
				return &DeferredError{Reason: "budget"}
			}
			return &DeferredError{Reason: "coordinator_unavailable"}
		}
		if cooldown {
			return &DeferredError{Reason: "cooldown"}
		}
		if wait <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-bounded.Done():
			timer.Stop()
		}
	}
}

func endpoint(path string) string {
	switch path {
	case "/swap/v1/quote":
		return "quote"
	case "/swap/v1/swap-instructions":
		return "instructions"
	case "/swap/v2/build":
		return "build"
	default:
		return "other"
	}
}

func (t *budgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	class := requestClass(request.Context())
	started := time.Now()
	if err := t.budget.admit(request.Context(), class); err != nil {
		reason := "canceled"
		var deferred *DeferredError
		if errors.As(err, &deferred) {
			reason = deferred.Reason
		}
		t.budget.metrics.JupiterDeferred(class, reason)
		return nil, err
	}
	response, err := t.base.RoundTrip(request)
	outcome := "transport_error"
	if response != nil {
		outcome = strconv.Itoa(response.StatusCode)
	}
	t.budget.metrics.JupiterRequest(class, endpoint(request.URL.Path), outcome, time.Since(started).Seconds())
	if err != nil {
		return response, err
	}
	current, currentErr := strconv.ParseUint(response.Header.Get("X-Ratelimit-Current"), 10, 64)
	remaining, remainingErr := strconv.ParseInt(response.Header.Get("X-Ratelimit-Remaining"), 10, 64)
	reset, resetErr := strconv.ParseInt(response.Header.Get("X-Ratelimit-Reset"), 10, 64)
	if currentErr == nil && remainingErr == nil && resetErr == nil {
		t.budget.metrics.JupiterQuota(float64(current), float64(remaining), float64(reset))
	}
	if response.StatusCode == http.StatusTooManyRequests || (remainingErr == nil && remaining <= 0 && resetErr == nil) {
		until := retryTime(response.Header, time.Now())
		// Persist even if this caller has just been canceled, so peers see the cooldown.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), time.Second)
		_, saveErr := t.budget.pool.Exec(ctx, `UPDATE loyal_yield.jupiter_request_budget
SET cooldown_until=GREATEST(cooldown_until,$1) WHERE scope='loyal'`, until)
		cancel()
		requestID := response.Header.Get("X-Api-Gateway-Request-Id")
		if len(requestID) > 128 {
			requestID = ""
		}
		slog.Warn("jupiter_quota_exhausted", "class", class, "endpoint", endpoint(request.URL.Path), "status", response.StatusCode,
			"request_id", requestID, "retry_at", until, "cooldown_saved", saveErr == nil)
	}
	return response, nil
}
