package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const (
	earnMaxDefaultRPC  = "https://api.mainnet-beta.solana.com"
	earnMaxSquadsVault = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	earnMaxRouteKey    = "rwa-multiply:" + earnMaxSquadsVault
	earnMaxVoltrVault  = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"
	earnMaxLPMint      = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"
	earnMaxIdleATA     = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"
	earnMaxCustodyDebt = "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn"
	earnMaxCustodyColl = "9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G"

	earnMaxHTTPTimeout  = 10 * time.Second
	earnMaxRPCTimeout   = 15 * time.Second
	earnMaxEarnedBudget = 90 * time.Second
	earnMaxFetchers     = 8
	earnMaxMaxBody      = 16 << 20
)

var earnMaxDebtReserves = []struct{ lane, market, reserve string }{
	{"AUTO", "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk", "6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM"},
	{"OnRe", "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"},
	{"Maple", "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"},
}

var earnMaxLaneOrder = []string{"AUTO", "OnRe", "Prime", "Maple"}

type EarnMaxPart[T any] struct {
	Ok    bool    `json:"ok"`
	Value *T      `json:"value"`
	Error *string `json:"error"`
}

type EarnMaxPage struct {
	Apy       EarnMaxPart[EarnMaxApy]          `json:"apy"`
	Backyard  BackyardPage                     `json:"backyard"`
	Balances  EarnMaxPart[EarnMaxBalances]     `json:"balances"`
	Borrowing EarnMaxPart[[]EarnMaxBorrowRow]  `json:"borrowing"`
	Health    EarnMaxPart[EarnMaxHealth]       `json:"health"`
	History   EarnMaxPart[[]EarnMaxHistoryRow] `json:"history"`
	Latches   EarnMaxPart[EarnMaxLatches]      `json:"latches"`
	LoadedAt  string                           `json:"loadedAt"`
	Moves     EarnMaxPart[[]EarnMaxMove]       `json:"moves"`
	NavSeries EarnMaxPart[[]EarnMaxNavPoint]   `json:"navSeries"`
	Route     EarnMaxPart[EarnMaxRoute]        `json:"route"`
	Vault     EarnMaxPart[EarnMaxVault]        `json:"vault"`
}

type EarnMaxRoute struct {
	CurrentApy         *EarnMaxCurrentApy    `json:"currentApy"`
	CurrentOperationID *string               `json:"currentOperationId"`
	DebtUsd            *float64              `json:"debtUsd"`
	Goal               *string               `json:"goal"`
	LastHoldReason     *string               `json:"lastHoldReason"`
	LeaseOwner         *string               `json:"leaseOwner"`
	LeverageTarget     *float64              `json:"leverageTarget"`
	LeverageTargetAt   *string               `json:"leverageTargetAt"`
	LeverageWatch      *EarnMaxLeverageWatch `json:"leverageWatch"`
	NavFresh           *bool                 `json:"navFresh"`
	NavUsd             *float64              `json:"navUsd"`
	ReportAgeS         *int32                `json:"reportAgeS"`
	ReportObservedAt   *string               `json:"reportObservedAt"`
	ReportSlot         *float64              `json:"reportSlot"`
	RouteStatus        *string               `json:"routeStatus"`
	SelectorAction     *string               `json:"selectorAction"`
	SelectorReason     *string               `json:"selectorReason"`
	UpdatedAgeS        int32                 `json:"updatedAgeS"`
	Version            *string               `json:"version"`
	WithdrawalStatus   *string               `json:"withdrawalStatus"`
}

type EarnMaxCurrentApy struct {
	ApyPct     float64  `json:"apyPct"`
	Flat       bool     `json:"flat"`
	Lane       *string  `json:"lane"`
	Level      *float64 `json:"level"`
	ObservedAt *string  `json:"observedAt"`
}

type EarnMaxLeverageWatch struct {
	Lanes      []EarnMaxLeverageLane `json:"lanes" nullable:"false"`
	ObservedAt string                `json:"observedAt"`
}

type EarnMaxLeverageLane struct {
	ApyPct    map[string]float64 `json:"apyPct"`
	Enterable bool               `json:"enterable"`
	Lane      string             `json:"lane"`
	Levels    []float64          `json:"levels" nullable:"false"`
	Older     bool               `json:"older"`
	SpreadPct float64            `json:"spreadPct"`
}

type EarnMaxNavPoint struct {
	At     string  `json:"at"`
	NavUsd float64 `json:"navUsd"`
}

type EarnMaxHealth struct {
	FailedSteps          []EarnMaxFailedStep    `json:"failedSteps" nullable:"false"`
	NavAll               int64                  `json:"navAll"`
	NavFailed            int64                  `json:"navFailed"`
	NavFailureReasons    []EarnMaxFailureReason `json:"navFailureReasons" nullable:"false"`
	NavReconciled        int64                  `json:"navReconciled"`
	RefusedBeforeSending int64                  `json:"refusedBeforeSending"`
}

type EarnMaxFailedStep struct {
	Action string `json:"action"`
	Count  int32  `json:"count"`
}

type EarnMaxFailureReason struct {
	Count  int32  `json:"count"`
	Reason string `json:"reason"`
}

type EarnMaxMove struct {
	Action      string  `json:"action"`
	AmountRaw   *string `json:"amountRaw"`
	At          string  `json:"at"`
	Reason      *string `json:"reason"`
	Signature   *string `json:"signature"`
	Status      string  `json:"status"`
	StrategyKey *string `json:"strategyKey"`
}

type EarnMaxLatches struct {
	Cleared []EarnMaxClearedLatch `json:"cleared" nullable:"false"`
	Open    []EarnMaxOpenLatch    `json:"open" nullable:"false"`
}

type EarnMaxClearedLatch struct {
	ClearedAt     string  `json:"clearedAt"`
	ClearedReason *string `json:"clearedReason"`
	LatchedAt     *string `json:"latchedAt"`
	Reason        *string `json:"reason"`
}

type EarnMaxOpenLatch struct {
	LatchedAt string `json:"latchedAt"`
	Reason    string `json:"reason"`
}

type EarnMaxHistoryRow struct {
	At        string   `json:"at"`
	EquityUsd *float64 `json:"equityUsd"`
	LtvPct    *float64 `json:"ltvPct"`
}

type EarnMaxBalances struct {
	Collateral float64 `json:"collateral"`
	Debt       float64 `json:"debt"`
	Idle       float64 `json:"idle"`
}

type EarnMaxApy struct {
	Daily      []EarnMaxDailyPrice `json:"daily" nullable:"false"`
	SharePrice float64             `json:"sharePrice"`
}

type EarnMaxDailyPrice struct {
	Day   string  `json:"day"`
	Price float64 `json:"price"`
}

type EarnMaxVault struct {
	ComputedAt string          `json:"computedAt"`
	Error      *string         `json:"error"`
	Flows      []EarnMaxLpFlow `json:"flows" nullable:"false"`
	ValueUsd   float64         `json:"valueUsd"`
}

type EarnMaxLpFlow struct {
	BlockTime int64   `json:"blockTime"`
	IdleDelta float64 `json:"idleDelta"`
	LpNet     float64 `json:"lpNet"`
}

type EarnMaxBorrowRow struct {
	BorrowApyPct float64 `json:"borrowApyPct"`
	BorrowUsd    float64 `json:"borrowUsd"`
	Lane         string  `json:"lane"`
	RoomTo90Usd  float64 `json:"roomTo90Usd"`
	SupplyUsd    float64 `json:"supplyUsd"`
	Token        string  `json:"token"`
	Utilization  float64 `json:"utilization"`
}

func earnMaxRec(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func earnMaxNum(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case string:
		if n == "" {
			return nil
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return nil
		}
		f = parsed
	case bool:
		if n {
			f = 1
		}
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func earnMaxStr(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func earnMaxOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}

func earnMaxDiv(v *float64, by float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v / by
	return &out
}

func earnMaxRPCURL() string {
	if value := os.Getenv("LOYAL_ADMIN_SOLANA_RPC_URL"); value != "" {
		return value
	}
	return earnMaxDefaultRPC
}

func earnMaxMessage(err error) string {
	var status huma.StatusError
	if errors.As(err, &status) {
		return status.Error()
	}
	return chain.Scrub(err.Error())
}

func earnMaxPart[T any](load func() (T, error)) EarnMaxPart[T] {
	value, err := load()
	if err != nil {
		message := earnMaxMessage(err)
		return EarnMaxPart[T]{Error: &message}
	}
	return EarnMaxPart[T]{Ok: true, Value: &value}
}

func earnMaxRPCError(method string, err error) error {
	var rpcErr *jsonrpc.RPCError
	var httpErr *jsonrpc.HTTPError
	switch {
	case errors.As(err, &rpcErr) && (rpcErr.Code == 429 || rpcErr.Code == -32429),
		errors.As(err, &httpErr) && httpErr.Code == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", method, chain.ErrRateLimited)
	case errors.As(err, &rpcErr):
		return fmt.Errorf("%s: rpc error %d: %s", method, rpcErr.Code, chain.Scrub(rpcErr.Message))
	case errors.As(err, &httpErr):
		return fmt.Errorf("%s: %w: rpc status %d", method, chain.ErrUnavailable, httpErr.Code)
	}
	return fmt.Errorf("%s: %w", method, chain.ErrUnavailable)
}

func earnMaxSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func earnMaxRetry[T any](ctx context.Context, call func() (T, error)) (T, error) {
	value, err := call()
	if !errors.Is(err, chain.ErrRateLimited) {
		return value, err
	}
	if err := earnMaxSleep(ctx, time.Second); err != nil {
		return value, err
	}
	return call()
}

func earnMaxChain() (*chain.Client, error) {
	return chain.New(earnMaxRPCURL(), earnMaxRPCTimeout)
}

func earnMaxGet(ctx context.Context, target string, out any) error {
	return readsFetchJSON(ctx, earnMaxHTTPTimeout, earnMaxMaxBody, http.MethodGet, target, nil, nil, out)
}

func earnMaxQuery[T any](ctx context.Context, d *Deps, text string, scan func(pgx.Rows) (T, error), args ...any) ([]T, error) {
	var out []T
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, text, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			row, err := scan(rows)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

func earnMaxLaneRank(lane string) int {
	head, _, _ := strings.Cut(lane, "/")
	for i, name := range earnMaxLaneOrder {
		if name == head {
			return i
		}
	}
	return len(earnMaxLaneOrder)
}

func earnMaxParseTime(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	return t, err == nil
}

func earnMaxLeverageLanes(watch map[string]any, watchAt *string) []EarnMaxLeverageLane {
	lanes := []EarnMaxLeverageLane{}
	raws, _ := watch["lanes"].([]any)
	for _, raw := range raws {
		lane := earnMaxRec(raw)
		apyPct := map[string]float64{}
		for level, bps := range earnMaxRec(lane["apyBps"]) {
			key := level
			if f, err := strconv.ParseFloat(level, 64); err == nil {
				key = strconv.FormatFloat(f, 'f', -1, 64)
			}
			apyPct[key] = earnMaxOr(earnMaxNum(bps), 0) / 100
		}
		levels := []float64{}
		if list, ok := lane["levels"].([]any); ok {
			for _, item := range list {
				if level := earnMaxNum(item); level != nil {
					levels = append(levels, *level)
				}
			}
		}
		name := "unknown"
		if s, ok := lane["lane"].(string); ok {
			name = s
		}
		older := false
		if at, ok := earnMaxParseTime(earnMaxStr(lane["observedAt"])); ok {
			if watched, ok := earnMaxParseTime(watchAt); ok {
				older = at.Before(watched)
			}
		}
		enterable, _ := lane["enterable"].(bool)
		lanes = append(lanes, EarnMaxLeverageLane{
			ApyPct: apyPct, Enterable: enterable, Lane: name, Levels: levels, Older: older,
			SpreadPct: earnMaxOr(earnMaxNum(lane["spreadBps"]), 0) / 100,
		})
	}
	sort.SliceStable(lanes, func(i, j int) bool { return earnMaxLaneRank(lanes[i].Lane) < earnMaxLaneRank(lanes[j].Lane) })
	return lanes
}

func earnMaxLoadRoute(ctx context.Context, d *Deps) (EarnMaxRoute, error) {
	var route EarnMaxRoute
	var found bool
	var raw []byte
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT state, lease_owner,
       extract(epoch FROM now() - updated_at)::int AS updated_age_s,
       extract(epoch FROM now() - (state->'observation'->>'reportObservedAt')::timestamptz)::int AS report_age_s
     FROM loyal_yield.multiply_route_states WHERE route_key = $1`, earnMaxRouteKey).
			Scan(&raw, &route.LeaseOwner, &route.UpdatedAgeS, &route.ReportAgeS)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		if err != nil {
			return err
		}
		var reason *string
		err = tx.QueryRow(ctx, `SELECT expected_effects #>> '{decision,reason}' AS reason
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND action = 'HOLD' AND created_at > now() - interval '1 hour'
     ORDER BY created_at DESC LIMIT 1`, earnMaxRouteKey).Scan(&reason)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		route.LastHoldReason = reason
		return nil
	})
	if err != nil {
		return route, err
	}
	if !found {
		return route, errors.New("The Earn MAX route has no state row yet.")
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		return route, err
	}
	observation := earnMaxRec(state["observation"])
	target := earnMaxRec(state["leverageTarget"])
	apy := earnMaxRec(state["currentApy"])
	selector := earnMaxRec(state["selector"])["result"]
	if selector == nil {
		selector = state["selectorResult"]
	}
	selector = earnMaxRec(selector)
	watch := earnMaxRec(state["leverageWatch"])
	watchAt := earnMaxStr(watch["observedAt"])

	if _, ok := apy["apyBps"]; ok {
		flat, _ := apy["flat"].(bool)
		route.CurrentApy = &EarnMaxCurrentApy{
			ApyPct: earnMaxOr(earnMaxNum(apy["apyBps"]), 0) / 100, Flat: flat, Lane: earnMaxStr(apy["lane"]),
			Level: earnMaxNum(apy["level"]), ObservedAt: earnMaxStr(apy["observedAt"]),
		}
	}
	route.CurrentOperationID = earnMaxStr(state["currentOperationId"])
	route.DebtUsd = earnMaxDiv(earnMaxNum(observation["payoffDebtRaw"]), 1e6)
	route.Goal = earnMaxStr(state["goal"])
	route.LeverageTarget = earnMaxNum(target["level"])
	route.LeverageTargetAt = earnMaxStr(target["decidedAt"])
	if watchAt != nil {
		route.LeverageWatch = &EarnMaxLeverageWatch{Lanes: earnMaxLeverageLanes(watch, watchAt), ObservedAt: *watchAt}
	}
	if fresh, ok := observation["navFresh"].(bool); ok {
		route.NavFresh = &fresh
	}
	route.NavUsd = earnMaxDiv(earnMaxNum(observation["navRaw"]), 1e6)
	route.ReportObservedAt = earnMaxStr(observation["reportObservedAt"])
	route.ReportSlot = earnMaxNum(observation["reportSlot"])
	route.RouteStatus = earnMaxStr(observation["routeStatus"])
	route.SelectorAction = earnMaxStr(earnMaxRec(selector)["action"])
	route.SelectorReason = earnMaxStr(earnMaxRec(selector)["reason"])
	if route.LeaseOwner != nil {
		owner := *route.LeaseOwner
		if len(owner) > 40 {
			owner = owner[len(owner)-40:]
		}
		version := owner[:min(7, len(owner))]
		route.Version = &version
	}
	route.WithdrawalStatus = earnMaxStr(earnMaxRec(state["withdrawal"])["status"])
	return route, nil
}

func earnMaxDecodeReport(request *string) (float64, bool) {
	if request == nil {
		return 0, false
	}
	var decoded []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err = enc.DecodeString(strings.TrimSpace(*request)); err == nil {
			break
		}
	}
	if err != nil {
		return 0, false
	}
	var body map[string]any
	if json.Unmarshal(decoded, &body) != nil {
		return 0, false
	}
	nav := earnMaxNum(earnMaxRec(body["Report"])["NAVAfterRaw"])
	if nav == nil {
		return 0, false
	}
	return *nav, true
}

func earnMaxLoadNavSeries(ctx context.Context, d *Deps) ([]EarnMaxNavPoint, error) {
	type report struct {
		at      time.Time
		request *string
	}
	reports, err := earnMaxQuery(ctx, d, `SELECT DISTINCT ON (date_bin('30 minutes', created_at, 'epoch'::timestamptz))
       created_at AS at, expected_effects->'phase3'->'buildInput'->>'request' AS request
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND action = 'REPORT_NAV' AND status = 'reconciled'
       AND created_at > now() - interval '24 hours'
     ORDER BY date_bin('30 minutes', created_at, 'epoch'::timestamptz), created_at DESC`,
		func(rows pgx.Rows) (report, error) {
			var r report
			return r, rows.Scan(&r.at, &r.request)
		}, earnMaxRouteKey)
	series := []EarnMaxNavPoint{}
	for _, r := range reports {
		if nav, ok := earnMaxDecodeReport(r.request); ok {
			series = append(series, EarnMaxNavPoint{At: readsISO(r.at), NavUsd: nav / 1e6})
		}
	}
	return series, err
}

func earnMaxLoadHealth(ctx context.Context, d *Deps) (EarnMaxHealth, error) {
	health := EarnMaxHealth{FailedSteps: []EarnMaxFailedStep{}, NavFailureReasons: []EarnMaxFailureReason{}}
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		nav, err := tx.Query(ctx, `SELECT status, recovery_reason AS reason, count(*)::int AS count
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND action = 'REPORT_NAV' AND created_at > now() - interval '24 hours'
       GROUP BY 1, 2 ORDER BY 3 DESC`, earnMaxRouteKey)
		if err != nil {
			return err
		}
		for nav.Next() {
			var status string
			var reason *string
			var count int32
			if err := nav.Scan(&status, &reason, &count); err != nil {
				return err
			}
			health.NavAll += int64(count)
			switch status {
			case "failed":
				health.NavFailed += int64(count)
				if len(health.NavFailureReasons) < 5 {
					text := "no reason"
					if reason != nil {
						text = *reason
					}
					health.NavFailureReasons = append(health.NavFailureReasons, EarnMaxFailureReason{Count: count, Reason: text})
				}
			case "reconciled":
				health.NavReconciled += int64(count)
			}
		}
		if err := nav.Err(); err != nil {
			return err
		}
		failed, err := tx.Query(ctx, `SELECT action, count(*)::int AS count
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND status = 'failed' AND created_at > now() - interval '24 hours'
         AND action NOT IN ('HOLD', 'REPORT_NAV')
       GROUP BY 1 ORDER BY 2 DESC`, earnMaxRouteKey)
		if err != nil {
			return err
		}
		for failed.Next() {
			var step EarnMaxFailedStep
			if err := failed.Scan(&step.Action, &step.Count); err != nil {
				return err
			}
			health.FailedSteps = append(health.FailedSteps, step)
		}
		if err := failed.Err(); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT count(*)::int AS count
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND status = 'failed' AND transaction_signature IS NULL
         AND created_at > now() - interval '24 hours'
         AND action NOT IN ('HOLD', 'REPORT_NAV')`, earnMaxRouteKey).Scan(&health.RefusedBeforeSending)
		return err
	})
	return health, err
}

func earnMaxLoadMoves(ctx context.Context, d *Deps) ([]EarnMaxMove, error) {
	moves, err := earnMaxQuery(ctx, d, `SELECT action, status, strategy_key, transaction_signature, created_at,
       -- A borrow's decision amount is 0 (its size comes from the leverage
       -- target); the amount is what landed in the vault's own accounts.
       CASE WHEN expected_effects #>> '{expectedEffects,kind}' = 'kamino-borrow'
         THEN (SELECT sum((a->>'afterRaw')::numeric - (a->>'beforeRaw')::numeric)::text
               FROM jsonb_array_elements(expected_effects #> '{expectedEffects,accounts}') a
               WHERE a->>'authority' = $2
                 AND (a->>'afterRaw')::numeric > (a->>'beforeRaw')::numeric)
         ELSE expected_effects #>> '{decision,amountRaw}'
       END AS amount_raw,
       COALESCE(recovery_reason, expected_effects #>> '{decision,reason}') AS reason
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND transaction_signature IS NOT NULL
       AND action NOT IN ('HOLD', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED')
     ORDER BY created_at DESC LIMIT 30`,
		func(rows pgx.Rows) (EarnMaxMove, error) {
			var m EarnMaxMove
			var created time.Time
			err := rows.Scan(&m.Action, &m.Status, &m.StrategyKey, &m.Signature, &created, &m.AmountRaw, &m.Reason)
			m.At = readsISO(created)
			return m, err
		}, earnMaxRouteKey, earnMaxSquadsVault)
	if moves == nil {
		moves = []EarnMaxMove{}
	}
	return moves, err
}

func earnMaxLoadLatches(ctx context.Context, d *Deps) (EarnMaxLatches, error) {
	latches := EarnMaxLatches{Cleared: []EarnMaxClearedLatch{}, Open: []EarnMaxOpenLatch{}}
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		open, err := tx.Query(ctx, `SELECT reason, latched_at FROM loyal_yield.backyard_manual_recovery_latches
       WHERE route_key = $1 AND cleared_at IS NULL`, earnMaxRouteKey)
		if err != nil {
			return err
		}
		for open.Next() {
			var latch EarnMaxOpenLatch
			var at time.Time
			if err := open.Scan(&latch.Reason, &at); err != nil {
				return err
			}
			latch.LatchedAt = readsISO(at)
			latches.Open = append(latches.Open, latch)
		}
		if err := open.Err(); err != nil {
			return err
		}
		cleared, err := tx.Query(ctx, `SELECT expected_effects AS effects, created_at
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND action = 'HOLD_CLEARED'
       ORDER BY created_at DESC LIMIT 10`, earnMaxRouteKey)
		if err != nil {
			return err
		}
		for cleared.Next() {
			var raw []byte
			var created time.Time
			if err := cleared.Scan(&raw, &created); err != nil {
				return err
			}
			var effects map[string]any
			_ = json.Unmarshal(raw, &effects)
			latch := EarnMaxClearedLatch{
				ClearedAt:     readsISO(created),
				ClearedReason: earnMaxStr(effects["clearedReason"]),
				LatchedAt:     earnMaxStr(effects["latchedAt"]),
				Reason:        earnMaxStr(effects["latchedReason"]),
			}
			if at := earnMaxStr(effects["clearedAt"]); at != nil {
				latch.ClearedAt = *at
			}
			latches.Cleared = append(latches.Cleared, latch)
		}
		return cleared.Err()
	})
	return latches, err
}

func earnMaxLoadHistory(ctx context.Context, d *Deps) ([]EarnMaxHistoryRow, error) {
	points, err := earnMaxQuery(ctx, d, `SELECT date_trunc('hour', observed_at) AS hour,
       (avg(equity_usd_micros) / 1e6)::float8 AS equity,
       (avg(ltv_bps) / 100)::float8 AS ltv
     FROM loyal_yield.multiply_position_snapshots
     WHERE route_key = $1 AND observed_at > now() - interval '7 days'
     GROUP BY 1 ORDER BY 1`,
		func(rows pgx.Rows) (EarnMaxHistoryRow, error) {
			var p EarnMaxHistoryRow
			var hour time.Time
			err := rows.Scan(&hour, &p.EquityUsd, &p.LtvPct)
			p.At = readsISO(hour)
			return p, err
		}, earnMaxRouteKey)
	if points == nil {
		points = []EarnMaxHistoryRow{}
	}
	return points, err
}

func earnMaxLoadBalances(ctx context.Context) (EarnMaxBalances, error) {
	client, err := earnMaxChain()
	if err != nil {
		return EarnMaxBalances{}, err
	}
	addresses := []string{earnMaxCustodyDebt, earnMaxCustodyColl, earnMaxIdleATA}
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		keys[i] = solana.MustPublicKeyFromBase58(address)
	}
	accounts, err := earnMaxRetry(ctx, func() ([]*chain.Account, error) {
		_, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
		return accounts, err
	})
	if err != nil {
		return EarnMaxBalances{}, err
	}
	var amounts [3]float64
	for i, account := range accounts {
		if account == nil {
			return EarnMaxBalances{}, fmt.Errorf("Token account %s is unreadable.", addresses[i])
		}
		token, err := spl.DecodeTokenAccount(account)
		if err != nil {
			return EarnMaxBalances{}, fmt.Errorf("Token account %s is unreadable.", addresses[i])
		}
		amounts[i] = float64(token.Amount) / 1e6
	}
	return EarnMaxBalances{Debt: amounts[0], Collateral: amounts[1], Idle: amounts[2]}, nil
}

func earnMaxSharePrice(ctx context.Context) (float64, error) {
	var body map[string]any
	if err := earnMaxGet(ctx, "https://api.voltr.xyz/vault/"+earnMaxVoltrVault+"/share-price", &body); err != nil {
		return 0, err
	}
	price := earnMaxNum(earnMaxRec(body["data"])["sharePrice"])
	if price == nil {
		return 0, errors.New("Voltr returned no share price.")
	}
	return *price, nil
}

func earnMaxLoadApy(ctx context.Context, sharePrice func() (float64, error)) (EarnMaxApy, error) {
	var price float64
	var priceErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		price, priceErr = sharePrice()
	}()
	var vault map[string]any
	vaultErr := earnMaxGet(ctx, "https://api.voltr.xyz/vault/"+earnMaxVoltrVault, &vault)
	<-done
	if vaultErr != nil {
		return EarnMaxApy{}, vaultErr
	}
	if priceErr != nil {
		return EarnMaxApy{}, priceErr
	}
	stats := earnMaxRec(earnMaxRec(vault["vault"])["dailyStats"])
	days, _ := stats["dateLabels"].([]any)
	tvl, _ := stats["tvlData"].([]any)
	lp, _ := stats["lpData"].([]any)
	apy := EarnMaxApy{Daily: []EarnMaxDailyPrice{}, SharePrice: price}
	at := func(list []any, i int) *float64 {
		if i < len(list) {
			return earnMaxNum(list[i])
		}
		return nil
	}
	for i, raw := range days {
		day, ok := raw.(string)
		t, l := at(tvl, i), at(lp, i)
		if ok && t != nil && *t != 0 && l != nil && *l != 0 {
			apy.Daily = append(apy.Daily, EarnMaxDailyPrice{Day: day, Price: *t / *l})
		}
	}
	return apy, nil
}

type earnMaxScan struct {
	computedAt string
	flows      []EarnMaxLpFlow
	valueUsd   float64
}

var (
	earnMaxFlowMu    sync.Mutex
	earnMaxFlowCache = map[string]EarnMaxLpFlow{}
	earnMaxScans     = &staleWhileRevalidate[earnMaxScan]{fresh: time.Minute, stale: 10 * time.Minute, wait: 3 * time.Second, budget: earnMaxEarnedBudget}
)

func earnMaxLoadVault() (EarnMaxVault, error) {
	scan, have, scanErr := earnMaxScans.get(earnMaxScanVault)
	if !have {
		return EarnMaxVault{}, scanErr
	}
	value := EarnMaxVault{
		ComputedAt: scan.computedAt,
		Flows:      scan.flows,
		ValueUsd:   scan.valueUsd,
	}
	if scanErr != nil {
		message := earnMaxMessage(scanErr)
		value.Error = &message
	}
	return value, nil
}

func earnMaxFetchFlow(ctx context.Context, client *chain.Client, signature solana.Signature) (EarnMaxLpFlow, error) {
	execution, err := earnMaxRetry(ctx, func() (chain.Execution, error) {
		return client.Execution(ctx, signature, rpc.CommitmentConfirmed)
	})
	if errors.Is(err, chain.ErrNotFound) {
		return EarnMaxLpFlow{}, fmt.Errorf("Transaction %s is unavailable.", signature)
	}
	if err != nil {
		return EarnMaxLpFlow{}, err
	}
	lpMint := solana.MustPublicKeyFromBase58(earnMaxLPMint)
	idle := solana.MustPublicKeyFromBase58(earnMaxIdleATA)
	var lpNet, idleDelta int64
	for _, side := range []struct {
		balances map[solana.PublicKey]chain.TokenBalance
		sign     int64
	}{{execution.Post, 1}, {execution.Pre, -1}} {
		for key, balance := range side.balances {
			if balance.Mint == lpMint {
				lpNet += side.sign * int64(balance.Amount)
			}
			if key == idle {
				idleDelta += side.sign * int64(balance.Amount)
			}
		}
	}
	flow := EarnMaxLpFlow{LpNet: float64(lpNet), IdleDelta: float64(idleDelta)}
	if lpNet != 0 {
		blockTime, err := earnMaxRetry(ctx, func() (time.Time, error) { return client.BlockTime(ctx, execution.Slot) })
		if err != nil {
			return EarnMaxLpFlow{}, fmt.Errorf("Transaction %s is unavailable.", signature)
		}
		flow.BlockTime = blockTime.Unix()
	}
	return flow, nil
}

func earnMaxScanVault(ctx context.Context) (earnMaxScan, error) {
	client, err := earnMaxChain()
	if err != nil {
		return earnMaxScan{}, err
	}
	mintKey := solana.MustPublicKeyFromBase58(earnMaxLPMint)
	accounts, err := earnMaxRetry(ctx, func() ([]*chain.Account, error) {
		_, accounts, err := client.Accounts(ctx, []solana.PublicKey{mintKey}, rpc.CommitmentConfirmed, 0)
		return accounts, err
	})
	if err != nil {
		return earnMaxScan{}, err
	}
	if len(accounts) != 1 || accounts[0] == nil || len(accounts[0].Data) < 82 {
		return earnMaxScan{}, errors.New("The LP mint is unreadable.")
	}
	supply := uint64(0)
	for i := 7; i >= 0; i-- {
		supply = supply<<8 | uint64(accounts[0].Data[36+i])
	}

	largest, err := earnMaxRetry(ctx, func() (*rpc.GetTokenLargestAccountsResult, error) {
		out, err := rpc.New(earnMaxRPCURL()).GetTokenLargestAccounts(ctx, mintKey, rpc.CommitmentConfirmed)
		if err != nil {
			return nil, earnMaxRPCError("getTokenLargestAccounts", err)
		}
		return out, nil
	})
	if err != nil {
		return earnMaxScan{}, err
	}
	holders := largest.Value
	var scanned uint64
	for _, holder := range holders {
		amount, err := strconv.ParseUint(holder.Amount, 10, 64)
		if err != nil {
			return earnMaxScan{}, errors.New("holder_scan_incomplete")
		}
		scanned += amount
	}
	if scanned != supply || len(holders) >= 20 {
		return earnMaxScan{}, errors.New("holder_scan_incomplete")
	}
	signatures := map[solana.Signature]bool{}
	for _, holder := range holders {
		found, err := earnMaxRetry(ctx, func() ([]chain.Signed, error) {
			return client.History(ctx, holder.Address, 1000, solana.Signature{}, rpc.CommitmentConfirmed, 0)
		})
		if err != nil {
			return earnMaxScan{}, err
		}
		for _, s := range found {
			if !s.Failed {
				signatures[s.Signature] = true
			}
		}
	}
	missing := []solana.Signature{}
	earnMaxFlowMu.Lock()
	for signature := range signatures {
		if _, ok := earnMaxFlowCache[signature.String()]; !ok {
			missing = append(missing, signature)
		}
	}
	earnMaxFlowMu.Unlock()

	var (
		wg       sync.WaitGroup
		failMu   sync.Mutex
		firstErr error
		slots    = make(chan struct{}, earnMaxFetchers)
	)
	fetchCtx, stop := context.WithCancel(ctx)
	defer stop()
	for _, signature := range missing {
		slots <- struct{}{}
		if fetchCtx.Err() != nil {
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			flow, err := earnMaxFetchFlow(fetchCtx, client, signature)
			if err != nil {
				failMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				failMu.Unlock()
				stop()
				return
			}
			earnMaxFlowMu.Lock()
			earnMaxFlowCache[signature.String()] = flow
			earnMaxFlowMu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return earnMaxScan{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return earnMaxScan{}, err
	}

	sharePrice, err := earnMaxSharePrice(ctx)
	if err != nil {
		return earnMaxScan{}, err
	}
	scan := earnMaxScan{computedAt: readsISO(time.Now()), flows: []EarnMaxLpFlow{}, valueUsd: float64(supply) * sharePrice / 1000 / 1e6}
	earnMaxFlowMu.Lock()
	kept := make(map[string]EarnMaxLpFlow, len(signatures))
	for signature := range signatures {
		flow := earnMaxFlowCache[signature.String()]
		kept[signature.String()] = flow
		if flow.LpNet != 0 {
			scan.flows = append(scan.flows, flow)
		}
	}
	earnMaxFlowCache = kept
	earnMaxFlowMu.Unlock()
	return scan, nil
}

func earnMaxLoadBorrowing(ctx context.Context) ([]EarnMaxBorrowRow, error) {
	rows := make([]EarnMaxBorrowRow, len(earnMaxDebtReserves))
	errs := make([]error, len(earnMaxDebtReserves))
	var wg sync.WaitGroup
	for i, target := range earnMaxDebtReserves {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var reserves []map[string]any
			endpoint := fmt.Sprintf("https://api.kamino.finance/kamino-market/%s/reserves/metrics?env=mainnet-beta", url.PathEscape(target.market))
			if err := earnMaxGet(ctx, endpoint, &reserves); err != nil {
				errs[i] = err
				return
			}
			var row map[string]any
			for _, item := range reserves {
				if item["reserve"] == target.reserve {
					row = item
					break
				}
			}
			supply, borrow := earnMaxNum(row["totalSupplyUsd"]), earnMaxNum(row["totalBorrowUsd"])
			if supply == nil || borrow == nil || *supply <= 0 {
				errs[i] = fmt.Errorf("Kamino has no metrics for the %s debt reserve.", target.lane)
				return
			}
			token := "?"
			if s := earnMaxStr(row["liquidityToken"]); s != nil {
				token = *s
			}
			rows[i] = EarnMaxBorrowRow{
				BorrowApyPct: earnMaxOr(earnMaxNum(row["borrowApy"]), 0) * 100, BorrowUsd: *borrow, Lane: target.lane,
				RoomTo90Usd: math.Max(0.9**supply-*borrow, 0), SupplyUsd: *supply, Token: token, Utilization: *borrow / *supply,
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func loadEarnMax(ctx context.Context, d *Deps, _ PageRequest) (EarnMaxPage, error) {
	page := EarnMaxPage{LoadedAt: readsISO(time.Now())}
	var backyardErr error
	sharePrice := sync.OnceValues(func() (float64, error) { return earnMaxSharePrice(ctx) })
	var wg sync.WaitGroup
	run := func(load func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			load()
		}()
	}
	run(func() { page.Backyard, backyardErr = loadBackyard(ctx, d) })
	run(func() { page.Route = earnMaxPart(func() (EarnMaxRoute, error) { return earnMaxLoadRoute(ctx, d) }) })
	run(func() {
		page.NavSeries = earnMaxPart(func() ([]EarnMaxNavPoint, error) { return earnMaxLoadNavSeries(ctx, d) })
	})
	run(func() { page.Health = earnMaxPart(func() (EarnMaxHealth, error) { return earnMaxLoadHealth(ctx, d) }) })
	run(func() { page.Moves = earnMaxPart(func() ([]EarnMaxMove, error) { return earnMaxLoadMoves(ctx, d) }) })
	run(func() {
		page.Latches = earnMaxPart(func() (EarnMaxLatches, error) { return earnMaxLoadLatches(ctx, d) })
	})
	run(func() {
		page.History = earnMaxPart(func() ([]EarnMaxHistoryRow, error) { return earnMaxLoadHistory(ctx, d) })
	})
	run(func() {
		page.Balances = earnMaxPart(func() (EarnMaxBalances, error) { return earnMaxLoadBalances(ctx) })
	})
	run(func() { page.Vault = earnMaxPart(func() (EarnMaxVault, error) { return earnMaxLoadVault() }) })
	run(func() { page.Apy = earnMaxPart(func() (EarnMaxApy, error) { return earnMaxLoadApy(ctx, sharePrice) }) })
	run(func() {
		page.Borrowing = earnMaxPart(func() ([]EarnMaxBorrowRow, error) { return earnMaxLoadBorrowing(ctx) })
	})
	wg.Wait()
	return page, backyardErr
}

func registerEarnMax(m *Mux) {
	Page(m, "/vault-integrations/earn-max", loadEarnMax)
}

var errStillRefreshing = errors.New("Still refreshing; try again shortly.")

type staleWhileRevalidate[T any] struct {
	fresh, stale, wait, budget time.Duration

	mu      sync.Mutex
	value   *T
	at      time.Time
	err     error
	running chan struct{}
}

func (c *staleWhileRevalidate[T]) get(load func(context.Context) (T, error)) (T, bool, error) {
	c.mu.Lock()
	if c.running == nil && (c.value == nil || time.Since(c.at) >= c.fresh) {
		c.running = make(chan struct{})
		go c.refresh(load, c.running)
	}
	running, current, wait := c.running, c.value == nil || time.Since(c.at) >= c.stale, c.wait
	if c.value == nil {
		wait = c.budget
	}
	c.mu.Unlock()
	if current && running != nil {
		select {
		case <-running:
		case <-time.After(wait):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	if err == nil && (c.value == nil || time.Since(c.at) >= c.stale) {
		err = errStillRefreshing
	}
	if c.value == nil {
		var zero T
		return zero, false, err
	}
	return *c.value, true, err
}

func (c *staleWhileRevalidate[T]) refresh(load func(context.Context) (T, error), done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), c.budget)
	defer cancel()
	value, err := load(ctx)
	c.mu.Lock()
	if err == nil {
		c.value, c.at = &value, time.Now()
	}
	c.err = err
	c.running = nil
	c.mu.Unlock()
	close(done)
}
