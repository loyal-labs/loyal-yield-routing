package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

var earnOverviewFamily = earnFamily{
	name:     "smart-account overview",
	ready:    func(h earnRoutes) bool { return h.auth != nil && h.Chain != nil },
	register: earnRoutes.registerOverview,
}

const (
	overviewCooldown    = 15 * time.Second
	overviewResultTTL   = 2 * time.Second
	overviewLoadTimeout = 30 * time.Second
)

var overviewMissingSettingsDelays = [...]time.Duration{250 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond, 2500 * time.Millisecond}

var (
	overviewUnauthenticated = []byte(`{"error":{"code":"unauthenticated","message":"No active auth session."}}`)
	overviewRateLimitedBody = []byte(`{"error":{"code":"rpc_rate_limited","message":"Smart-account data is temporarily rate limited. Please wait a moment and try again."}}`)
)

type overviewRoutes struct {
	earnRoutes
	cache  *overviewCache
	assets *overviewAssets
}

type overviewInput struct {
	SessionInput
}

type overviewPoliciesInput struct {
	SessionInput
	ForceRefresh string `query:"forceRefresh"`
}

type overviewReservesInput struct {
	SessionInput
	RiskProfile string `query:"riskProfile"`
}

type overviewOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	ContentType  string `header:"Content-Type"`
	RetryAfter   string `header:"Retry-After"`
	ServerTiming string `header:"Server-Timing"`
	Body         []byte
}

type overviewTimedLoad struct {
	Data any          `json:"data"`
	Meta overviewMeta `json:"meta"`
}

type overviewMeta struct {
	FetchedAt int64              `json:"fetchedAt"`
	TimingsMs map[string]float64 `json:"timingsMs"`
}

func (h earnRoutes) registerOverview(api huma.API) {
	o := &overviewRoutes{earnRoutes: h, cache: newOverviewCache(), assets: newOverviewAssets(h)}
	o.register(api)
}

func (o *overviewRoutes) register(api huma.API) {
	registerWebReads(api, "smart-account-overview-base", "/api/smart-accounts/overview/base", func(ctx context.Context, in *overviewInput) (*overviewOutput, error) {
		return o.serve(ctx, in.SessionInput, "smart-account-overview-base", func(ctx context.Context, settings solana.PublicKey) (any, error) {
			return overviewLoad(ctx, o.cache, settings.String()+":base", false, overviewResultTTL, func(ctx context.Context) (overviewBase, error) { return o.base(ctx, settings) })
		}), nil
	})
	registerWebReads(api, "smart-account-overview-policies", "/api/smart-accounts/overview/policies", func(ctx context.Context, in *overviewPoliciesInput) (*overviewOutput, error) {
		return o.serve(ctx, in.SessionInput, "smart-account-overview-policies", func(ctx context.Context, settings solana.PublicKey) (any, error) {
			return overviewLoad(ctx, o.cache, settings.String()+":policies", in.ForceRefresh == "1", overviewResultTTL, func(ctx context.Context) (overviewPolicies, error) { return o.policies(ctx, settings) })
		}), nil
	})
	registerWebReads(api, "smart-account-overview-best-apy-reserves", "/api/smart-accounts/overview/best-apy-reserves", func(ctx context.Context, in *overviewReservesInput) (*overviewOutput, error) {
		markets, ok := overviewRiskBasketMarkets(in.RiskProfile)
		if !ok {
			slog.Warn("[smart-account-overview] unsupported risk profile", "riskProfile", in.RiskProfile)
			return overviewFailed(), nil
		}
		return o.serve(ctx, in.SessionInput, "smart-account-overview-best-apy-reserves", func(ctx context.Context, _ solana.PublicKey) (any, error) {
			return o.bestApyReserves(ctx, markets)
		}), nil
	})
	registerWebReads(api, "smart-account-overview-vaults", "/api/smart-accounts/overview/vaults", func(ctx context.Context, in *overviewVaultsInput) (*overviewOutput, error) {
		return o.serve(ctx, in.SessionInput, "smart-account-overview-vaults", func(ctx context.Context, settings solana.PublicKey) (any, error) {
			bypass, err := overviewInvalidates(in.Invalidate)
			if err != nil {
				return nil, err
			}
			highest, given := overviewParseInt(in.AccountUtilization)
			return overviewLoad(ctx, o.cache, settings.String()+":vaults", bypass, overviewResultTTL, func(ctx context.Context) ([]overviewVault, error) { return o.vaults(ctx, settings, highest, given) })
		}), nil
	})
	registerWebReads(api, "smart-account-vault-activity", "/api/smart-accounts/vault-activity", func(ctx context.Context, in *overviewActivityInput) (*overviewOutput, error) {
		return o.serveActivity(ctx, in), nil
	})
}

func (o *overviewRoutes) principal(ctx context.Context, in SessionInput, timing string) (solana.PublicKey, *overviewOutput) {
	settings, err := o.sessionSettings(ctx, in)
	var refused *AuthError
	if errors.As(err, &refused) && refused.Failure == Unauthenticated {
		return solana.PublicKey{}, overviewJSON(http.StatusUnauthorized, overviewUnauthenticated)
	}
	if err != nil {
		slog.Warn("[smart-account-overview] session principal refused", "timing", timing, "error", err)
		return solana.PublicKey{}, overviewFailed()
	}
	return settings, nil
}

func (o *overviewRoutes) serve(ctx context.Context, in SessionInput, timing string, load func(context.Context, solana.PublicKey) (any, error)) *overviewOutput {
	settings, refused := o.principal(ctx, in, timing)
	if refused != nil {
		return refused
	}
	started := time.Now()
	data, err := load(ctx, settings)
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	var limited *overviewRateLimited
	if errors.As(err, &limited) {
		out := overviewJSON(http.StatusTooManyRequests, overviewRateLimitedBody)
		out.RetryAfter = strconv.Itoa(limited.retryAfterSeconds)
		return out
	}
	if err != nil {
		slog.Warn("[smart-account-overview] "+timing+".failed", "settingsPda", settings.String(), "durationMs", elapsed, "error", chain.Scrub(err.Error()))
		return overviewFailed()
	}
	body, err := json.Marshal(overviewTimedLoad{Data: data, Meta: overviewMeta{FetchedAt: time.Now().UnixMilli(), TimingsMs: map[string]float64{"total": math.Round(elapsed*100) / 100}}})
	if err != nil {
		slog.Error("[smart-account-overview] encode response", "timing", timing, "error", err)
		return overviewFailed()
	}
	out := overviewJSON(http.StatusOK, body)
	out.ServerTiming = fmt.Sprintf("%s;dur=%.2f", timing, elapsed)
	return out
}

func (o *overviewRoutes) sessionSettings(ctx context.Context, in SessionInput) (solana.PublicKey, error) {
	session, err := o.auth.cookieSession(in)
	if err != nil {
		return solana.PublicKey{}, err
	}
	settings, _, err := session.account(ctx)
	return settings, err
}

func overviewJSON(status int, body []byte) *overviewOutput {
	return &overviewOutput{Status: status, CacheControl: perWalletCacheControl(), ContentType: "application/json", Body: body}
}

func overviewFailed() *overviewOutput {
	return &overviewOutput{Status: http.StatusInternalServerError, CacheControl: perWalletCacheControl(), Body: []byte{}}
}

var errOverviewSettingsMissing = errors.New("smart account settings account is not on chain yet")

type overviewRateLimited struct{ retryAfterSeconds int }

func (e *overviewRateLimited) Error() string {
	return "smart-account overview is temporarily rate limited by the RPC provider"
}

type overviewFlight struct {
	done  chan struct{}
	value any
	err   error
}

type overviewCache struct {
	mu       sync.Mutex
	order    uint64
	latest   map[string]uint64
	flights  map[string]*overviewFlight
	cooldown map[string]time.Time
	done     map[string]ttlItem[any]
}

func newOverviewCache() *overviewCache {
	return &overviewCache{latest: map[string]uint64{}, flights: map[string]*overviewFlight{}, cooldown: map[string]time.Time{}, done: map[string]ttlItem[any]{}}
}

func overviewLoad[T any](ctx context.Context, c *overviewCache, key string, bypass bool, ttl time.Duration, load func(context.Context) (T, error)) (T, error) {
	var zero T
	now := time.Now()
	c.mu.Lock()
	if until, ok := c.cooldown[key]; ok && until.After(now) {
		c.mu.Unlock()
		return zero, overviewRetryAfter(until, now)
	}
	if !bypass {
		if flight := c.flights[key]; flight != nil {
			c.mu.Unlock()
			return overviewWait[T](ctx, flight)
		}
		if item, ok := c.done[key]; ok && item.expires.After(now) {
			c.mu.Unlock()
			return item.value.(T), nil
		}
	}
	c.prune(now)
	c.order++
	token := c.order
	c.latest[key] = token
	flight := &overviewFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()

	go func() {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), overviewLoadTimeout)
		defer cancel()
		flight.value, flight.err = c.run(loadCtx, key, token, ttl, func(ctx context.Context) (any, error) { return load(ctx) })
		c.mu.Lock()
		if c.flights[key] == flight {
			delete(c.flights, key)
		}
		if c.latest[key] == token {
			delete(c.latest, key)
		}
		c.mu.Unlock()
		close(flight.done)
	}()
	return overviewWait[T](ctx, flight)
}

func (c *overviewCache) run(ctx context.Context, key string, token uint64, ttl time.Duration, load func(context.Context) (any, error)) (any, error) {
	for attempt := 0; ; attempt++ {
		value, err := load(ctx)
		now := time.Now()
		switch {
		case err == nil:
			c.mu.Lock()
			if c.latest[key] == token {
				delete(c.cooldown, key)
				c.done[key] = ttlItem[any]{expires: now.Add(ttl), value: value}
			}
			c.mu.Unlock()
			return value, nil
		case errors.Is(err, chain.ErrRateLimited):
			c.mu.Lock()
			if c.latest[key] == token {
				c.cooldown[key] = now.Add(overviewCooldown)
			}
			until, ok := c.cooldown[key]
			c.mu.Unlock()
			if !ok {
				until = now.Add(overviewCooldown)
			}
			return nil, overviewRetryAfter(until, now)
		case !errors.Is(err, errOverviewSettingsMissing) || attempt == len(overviewMissingSettingsDelays):
			return nil, err
		}
		select {
		case <-time.After(overviewMissingSettingsDelays[attempt]):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *overviewCache) prune(now time.Time) {
	for key, item := range c.done {
		if !item.expires.After(now) {
			delete(c.done, key)
		}
	}
	for key, until := range c.cooldown {
		if !until.After(now) {
			delete(c.cooldown, key)
		}
	}
}

func overviewWait[T any](ctx context.Context, flight *overviewFlight) (T, error) {
	var zero T
	select {
	case <-flight.done:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	if flight.err != nil {
		return zero, flight.err
	}
	return flight.value.(T), nil
}

func overviewRetryAfter(until, now time.Time) *overviewRateLimited {
	return &overviewRateLimited{retryAfterSeconds: max(1, int(math.Ceil(until.Sub(now).Seconds())))}
}
