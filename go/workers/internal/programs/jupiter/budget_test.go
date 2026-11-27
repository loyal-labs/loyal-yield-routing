package jupiter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func budgetPools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("JUPITER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local jupiter_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "jupiter_test") {
		t.Fatal("requires a disposable local jupiter_test database")
	}
	config.MaxConns = 2
	ctx := t.Context()
	first, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if _, err := first.Exec(ctx, `CREATE SCHEMA loyal_yield`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := first.Exec(ctx, `DROP SCHEMA loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	})
	if _, err := NewBudget(ctx, first, engine.NewFacts(prometheus.NewRegistry())); err == nil {
		t.Fatal("started without the coordination migration")
	}
	sql, err := os.ReadFile("../../../../../migrations/yield/0145_jupiter_request_budget.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func newTestBudget(t *testing.T, pool *pgxpool.Pool, registry *prometheus.Registry) *Budget {
	t.Helper()
	b, err := NewBudget(t.Context(), pool, engine.NewFacts(registry))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBudgetSharedPacingAndProtectedCapacity(t *testing.T) {
	first, second := budgetPools(t)
	budgets := []*Budget{newTestBudget(t, first, prometheus.NewRegistry()), newTestBudget(t, second, prometheus.NewRegistry())}
	type arrival struct {
		class string
		at    time.Time
	}
	var mu sync.Mutex
	var arrivals []arrival
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, arrival{r.Header.Get("X-Test-Class"), time.Now()})
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	clients := []*http.Client{{Transport: budgets[0].Transport(nil)}, {Transport: budgets[1].Transport(nil)}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 28 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			class, ctx := "background", Background(t.Context())
			if i == 0 {
				class, ctx = "execution", t.Context()
			}
			if i == 1 {
				class, ctx = "valuation", Valuation(t.Context())
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/swap/v1/quote", nil)
			req.Header.Set("X-Test-Class", class)
			response, err := clients[i%2].Do(req)
			if err != nil {
				var deferred *DeferredError
				// Protected work must pass the limiter's admission deadline; total
				// HTTP latency also includes goroutine scheduling and response IO.
				if class != "background" || !errors.As(err, &deferred) || deferred.Reason != "budget" {
					t.Error(err)
				}
				return
			}
			response.Body.Close()
		}()
	}
	close(start)
	wg.Wait()
	sort.Slice(arrivals, func(i, j int) bool { return arrivals[i].at.Before(arrivals[j].at) })
	counts := map[string]int{}
	for i, a := range arrivals {
		counts[a.class]++
		// A saturated scan cannot borrow either protected class's allowance.
		inWindow := 0
		for _, b := range arrivals[i:] {
			if b.at.Sub(a.at) < time.Second {
				inWindow++
			}
		}
		if inWindow > 10 {
			t.Errorf("aggregate one-second burst = %d", inWindow)
		}
	}
	if counts["execution"] != 1 || counts["valuation"] != 1 || counts["background"] < 4 || counts["background"] > 7 {
		t.Fatalf("starvation or unbounded scan: %v", counts)
	}
	// A new process/pool must observe the last process's pacing, not start full.
	_, err := first.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET execution_at=clock_timestamp()+interval '1 second'`)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newTestBudget(t, second, prometheus.NewRegistry())
	wait, cooldown, err := restarted.take(t.Context(), "execution")
	if err != nil || cooldown || wait < 500*time.Millisecond {
		t.Fatalf("restart lost admission state: %v, %v, %v", wait, cooldown, err)
	}
	t.Logf("28 simultaneous requests across independent pools: upstream=%v; protected requests admitted; remaining scans deferred", counts)
}

func TestBudgetValuationWaitsWithinCallerDeadline(t *testing.T) {
	pool, _ := budgetPools(t)
	b := newTestBudget(t, pool, prometheus.NewRegistry())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := &http.Client{Transport: b.Transport(nil)}
	for _, class := range []string{"background", "valuation", "cancelled valuation"} {
		if _, err := pool.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET request_at=clock_timestamp()+interval '2 seconds'`); err != nil {
			t.Fatal(err)
		}
		deadline := 3 * time.Second
		if class == "cancelled valuation" {
			deadline = 25 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		if class == "background" {
			ctx = Background(ctx)
		} else {
			ctx = Valuation(ctx)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/swap/v1/quote", nil)
		started := time.Now()
		response, err := client.Do(req)
		cancel()
		if response != nil {
			response.Body.Close()
		}
		switch class {
		case "background":
			var deferred *DeferredError
			if !errors.As(err, &deferred) || deferred.Reason != "budget" || calls.Load() != 0 {
				t.Fatalf("discovery exceeded its admission window: %v, upstream=%d", err, calls.Load())
			}
		case "valuation":
			if err != nil || calls.Load() != 1 || time.Since(started) < 1800*time.Millisecond {
				t.Fatalf("valuation failed before caller deadline or bypassed pacing: %v, upstream=%d", err, calls.Load())
			}
		case "cancelled valuation":
			if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
				t.Fatalf("valuation ignored caller deadline: %v, upstream=%d", err, calls.Load())
			}
		}
	}
}

func TestBudgetCooldownCancellationAndFailure(t *testing.T) {
	first, second := budgetPools(t)
	registry := prometheus.NewRegistry()
	b := newTestBudget(t, first, registry)
	peer := newTestBudget(t, second, prometheus.NewRegistry())
	var calls atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "3")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(6*time.Second).Unix(), 10))
			w.Header().Set("X-Ratelimit-Current", "100")
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.WriteHeader(429)
		}
		_, _ = w.Write([]byte(`{"outAmount":"42"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL+"/swap/v1", "vault-key", server.Client())
	if err := client.SetBudget(b); err != nil {
		t.Fatal(err)
	}
	_, err := client.Quote(t.Context(), QuoteRequest{Amount: 1})
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 429 {
		t.Fatalf("upstream throttle = %v", err)
	}
	var waitUntil time.Time
	if err := first.QueryRow(t.Context(), `SELECT cooldown_until FROM loyal_yield.jupiter_request_budget`).Scan(&waitUntil); err != nil {
		t.Fatal(err)
	}
	if time.Until(waitUntil) < 4*time.Second {
		t.Fatal("reset header was not honored")
	}
	peerClient, _ := NewClient(server.URL+"/swap/v1", "retail-key", server.Client())
	if err := peerClient.SetBudget(peer); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{client, peerClient} {
		_, err := c.Quote(t.Context(), QuoteRequest{Amount: 1})
		var deferred *DeferredError
		if !errors.As(err, &deferred) || deferred.Reason != "cooldown" {
			t.Fatalf("shared cooldown = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("peer ignored shared cooldown")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			counts[family.GetName()] += metric.GetCounter().GetValue()
		}
	}
	if counts["loyal_jupiter_requests_total"] != 1 || counts["loyal_jupiter_deferred_total"] != 1 {
		t.Fatal("local cooldown counted as upstream traffic", counts)
	}
	// Expire the persisted cooldown, as the database clock would. No cached quote survives it.
	limited.Store(false)
	if _, err := first.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET cooldown_until='epoch', execution_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	q, err := peerClient.Quote(t.Context(), QuoteRequest{Amount: 2})
	if err != nil || q.OutAmount != "42" || calls.Load() != 2 {
		t.Fatalf("recovery did not fetch fresh evidence: %v, %v", q, err)
	}
	if _, err := first.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET execution_at=clock_timestamp()+interval '2 seconds'`); err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	_ = first.QueryRow(t.Context(), `SELECT execution_at FROM loyal_yield.jupiter_request_budget`).Scan(&before)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = peerClient.Quote(ctx, QuoteRequest{Amount: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation = %v", err)
	}
	_ = first.QueryRow(t.Context(), `SELECT execution_at FROM loyal_yield.jupiter_request_budget`).Scan(&after)
	if !before.Equal(after) || calls.Load() != 2 {
		t.Fatal("canceled caller consumed admission or sent HTTP")
	}
	// Simulate unavailable coordination; even critical requests must not bypass it.
	second.Close()
	_, err = peerClient.Quote(t.Context(), QuoteRequest{Amount: 4})
	var deferred *DeferredError
	if !errors.As(err, &deferred) || deferred.Reason != "coordinator_unavailable" || calls.Load() != 2 {
		t.Fatalf("coordinator failure did not fail closed: %v", err)
	}
}

func TestBudgetSuccessfulExhaustionAndRedirects(t *testing.T) {
	first, second := budgetPools(t)
	b := newTestBudget(t, first, prometheus.NewRegistry())
	peer := newTestBudget(t, second, prometheus.NewRegistry())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/swap/v2/build" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("X-Ratelimit-Remaining", "-1")
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client := &http.Client{Transport: b.Transport(nil)}
	started := time.Now()
	res, err := client.Get(server.URL + "/swap/v2/build")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if calls.Load() != 2 || time.Since(started) < 450*time.Millisecond {
		t.Fatal("redirect bypassed admission")
	}
	if err := peer.admit(t.Context(), "valuation"); err == nil {
		t.Fatal("successful exhaustion did not stop peer")
	}
}

func TestBudgetReserveAllowsSimultaneousPairAndKeepsGlobalRateCap(t *testing.T) {
	first, second := budgetPools(t)
	b1 := newTestBudget(t, first, prometheus.NewRegistry())
	b2 := newTestBudget(t, second, prometheus.NewRegistry())

	var mu sync.Mutex
	var arrivals []time.Time
	pairArrived := make(chan struct{}, 2)
	pairRelease := make(chan struct{})
	var pairInFlight atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		if r.URL.Query().Get("pair") == "1" {
			pairArrived <- struct{}{}
			if pairInFlight.Add(1) == 2 {
				close(pairRelease)
			}
			select {
			case <-pairRelease:
			case <-time.After(time.Second):
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
		}
		if r.URL.Path == "/swap/v1/redirect" {
			http.Redirect(w, r, "/swap/v1/final", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"outAmount":"1"}`))
	}))
	defer server.Close()

	c1, err := NewClient(server.URL+"/swap/v1", "key-1", server.Client())
	if err != nil || c1.SetBudget(b1) != nil {
		t.Fatal(err)
	}
	c2, err := NewClient(server.URL+"/swap/v1", "key-2", server.Client())
	if err != nil || c2.SetBudget(b2) != nil {
		t.Fatal(err)
	}

	// Out-of-range reservations fail closed.
	if _, err := c1.Reserve(t.Context(), maxBudgetBatch+1); err == nil {
		t.Fatal("oversized reservation was accepted")
	}

	// Reserve(2) lets an execution pair run simultaneously on the same slot,
	// while any extra call beyond the reservation (e.g. a redirect) still waits.
	reservedCtx, err := c1.Reserve(t.Context(), 2)
	if err != nil {
		t.Fatalf("reserve pair failed: %v", err)
	}
	started := time.Now()
	var pairWG sync.WaitGroup
	pairErrs := make([]error, 2)
	for i := range 2 {
		pairWG.Add(1)
		go func() {
			defer pairWG.Done()
			req, _ := http.NewRequestWithContext(reservedCtx, http.MethodGet, server.URL+"/swap/v1/quote?pair=1", nil)
			res, err := c1.http.Do(req)
			if err != nil {
				pairErrs[i] = err
				return
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				pairErrs[i] = fmt.Errorf("status %d", res.StatusCode)
			}
		}()
	}
	pairWG.Wait()
	if pairErrs[0] != nil || pairErrs[1] != nil || time.Since(started) > 350*time.Millisecond {
		t.Fatalf("reserved pair did not execute simultaneously: errs=%v elapsed=%v", pairErrs, time.Since(started))
	}
	// A 3rd request on the exhausted allowance must wait for execution pacing (~1s).
	extraStart := time.Now()
	req, _ := http.NewRequestWithContext(reservedCtx, http.MethodGet, server.URL+"/swap/v1/quote", nil)
	res, err := c1.http.Do(req)
	if err != nil {
		t.Fatalf("post-allowance request failed: %v", err)
	}
	res.Body.Close()
	if time.Since(extraStart) < 700*time.Millisecond {
		t.Fatalf("exhausted reservation bypassed pacing: %v", time.Since(extraStart))
	}

	// Two consecutive execution Reserve(4) batches (representing a 1x slot retry)
	// must both succeed within the execution admission deadline.
	if _, err := first.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET request_at='epoch', execution_at='epoch', valuation_at='epoch', background_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	arrivals = nil
	mu.Unlock()
	for attempt := range 2 {
		batchCtx, err := c1.Reserve(t.Context(), 4)
		if err != nil {
			t.Fatalf("execution Reserve(4) attempt %d failed: %v", attempt+1, err)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequestWithContext(batchCtx, http.MethodGet, server.URL+"/swap/v1/quote", nil)
				r, err := c1.http.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body.Close()
			}()
		}
		wg.Wait()
	}

	// Mixed concurrent Reserve(4), Reserve(2), and single requests across both pools
	// and all classes must never exceed 10 requests in any 1-second window.
	if _, err := first.Exec(t.Context(), `UPDATE loyal_yield.jupiter_request_budget SET request_at='epoch', execution_at='epoch', valuation_at='epoch', background_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	arrivals = nil
	mu.Unlock()
	start := make(chan struct{})
	var mixWG sync.WaitGroup
	jobs := []struct {
		client *Client
		ctx    context.Context
		count  int
	}{
		{c1, Background(t.Context()), 4},
		{c2, Background(t.Context()), 2},
		{c1, Background(t.Context()), 2},
		{c2, t.Context(), 2},
		{c1, Valuation(t.Context()), 1},
		{c2, Valuation(t.Context()), 1},
		{c1, Background(t.Context()), 1},
		{c2, Background(t.Context()), 1},
	}
	for _, job := range jobs {
		mixWG.Add(1)
		go func() {
			defer mixWG.Done()
			<-start
			useCtx := job.ctx
			if job.count > 1 {
				var err error
				useCtx, err = job.client.Reserve(job.ctx, job.count)
				if err != nil {
					var deferred *DeferredError
					if requestClass(job.ctx) != "background" || !errors.As(err, &deferred) || deferred.Reason != "budget" {
						t.Errorf("unexpected reserve error for %s/%d: %v", requestClass(job.ctx), job.count, err)
					}
					return
				}
			}
			var inner sync.WaitGroup
			for range job.count {
				inner.Add(1)
				go func() {
					defer inner.Done()
					req, _ := http.NewRequestWithContext(useCtx, http.MethodGet, server.URL+"/swap/v1/quote", nil)
					r, err := job.client.http.Do(req)
					if err != nil {
						var deferred *DeferredError
						if requestClass(job.ctx) != "background" || !errors.As(err, &deferred) || deferred.Reason != "budget" {
							t.Errorf("unexpected request error for %s/%d: %v", requestClass(job.ctx), job.count, err)
						}
						return
					}
					r.Body.Close()
				}()
			}
			inner.Wait()
		}()
	}
	close(start)
	mixWG.Wait()

	mu.Lock()
	snapshot := append([]time.Time(nil), arrivals...)
	mu.Unlock()
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Before(snapshot[j]) })
	if len(snapshot) < 8 {
		t.Fatalf("too few mixed requests admitted: %d", len(snapshot))
	}
	for i, a := range snapshot {
		inWindow := 0
		for _, b := range snapshot[i:] {
			if b.Sub(a) < time.Second {
				inWindow++
			}
		}
		if inWindow > 10 {
			t.Fatalf("mixed Reserve burst exceeded 10 RPS: %d in 1s window", inWindow)
		}
	}
}
