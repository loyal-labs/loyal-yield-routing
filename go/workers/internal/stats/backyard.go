package stats

// The public Backyard dashboard (backyard.askloyal.com): a read-only view of the
// custodial Loyal Looping vault that Backyard integrates. The queries mirror
// the admin Earn MAX page (internal/admin/earn_max.go) bound to the Looping
// route instead of the Earn MAX one.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

// The custodial Looping vault, from the worker's own registry.
var (
	backyardLooping = func() backyard.VaultDescription {
		vault, err := backyard.DescribeVault("looping")
		if err != nil {
			panic(err)
		}
		return vault
	}()
	backyardRouteKey   = backyardLooping.RouteKey
	backyardSquads     = backyardLooping.Accounts.Manager.String()
	backyardVoltrVault = backyardLooping.Accounts.Vault
	backyardNAVAdaptor = backyardLooping.Accounts.AdaptorProgram.String()
)

const (
	backyardDefault = "OnRe/ONyc/USDC"

	backyardQueryTimeout = 10 * time.Second
	backyardHTTPTimeout  = 10 * time.Second
	backyardMaxBody      = 16 << 20
)

type backyardLane struct{ market, collateral, debt string }

// Kamino reserves per lane, from go/workers/internal/backyard/route_catalog.go.
var backyardLanes = map[string]backyardLane{
	"AUTO/AUTO/PYUSD":      {"Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk", "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX", "6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM"},
	"AUTO/AUTO/wYLDS":      {"Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk", "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX", "34GJjXf7jGTdNL8Yw1HtqWSVvfX6JTsft3UsuFFUL7VU"},
	"Maple/syrupUSDC/USDC": {"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"},
	"OnRe/ONyc/USDC":       {"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"},
	"Prime/PRIME/USDC":     {"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"},
}

// Debt reserves shown in "Borrowing room".
var backyardBorrowLanes = []string{"AUTO/AUTO/PYUSD", "OnRe/ONyc/USDC", "Maple/syrupUSDC/USDC"}

var backyardLaneOrder = []string{"AUTO", "OnRe", "Prime", "Maple"}

// backyardPart is one independently loaded source. The page is public, so a
// failure carries no error text; the reason goes to the log.
type backyardPart[T any] struct {
	Ok    bool `json:"ok"`
	Value *T   `json:"value"`
}

func backyardLoad[T any](name string, load func() (T, error)) backyardPart[T] {
	value, err := load()
	if err != nil {
		slog.Warn("backyard dashboard source unavailable", "source", name, "error", err)
		return backyardPart[T]{}
	}
	return backyardPart[T]{Ok: true, Value: &value}
}

type backyardPage struct {
	Borrowing backyardPart[[]backyardBorrowRow] `json:"borrowing"`
	Breakdown backyardPart[backyardBreakdown]   `json:"breakdown"`
	Health    backyardPart[backyardHealth]      `json:"health"`
	History   backyardPart[[]backyardHistory]   `json:"history"`
	LoadedAt  string                            `json:"loadedAt"`
	MoveDays  backyardPart[[]string]            `json:"moveDays"`
	Moves     backyardPart[[]backyardMove]      `json:"moves"`
	NavSeries backyardPart[[]backyardNavPoint]  `json:"navSeries"`
	Route     backyardPart[backyardRoute]       `json:"route"`
	Voltr     backyardPart[backyardVoltr]       `json:"voltr"`
}

type backyardCurrentApy struct {
	ApyPct     float64  `json:"apyPct"`
	Flat       bool     `json:"flat"`
	Lane       *string  `json:"lane"`
	Level      *float64 `json:"level"`
	ObservedAt *string  `json:"observedAt"`
}

type backyardLeverageLane struct {
	ApyPct    map[string]float64 `json:"apyPct"`
	Enterable bool               `json:"enterable"`
	Lane      string             `json:"lane"`
	Levels    []float64          `json:"levels"`
	Older     bool               `json:"older"`
	SpreadPct float64            `json:"spreadPct"`
}

type backyardLeverageWatch struct {
	Lanes      []backyardLeverageLane `json:"lanes"`
	ObservedAt string                 `json:"observedAt"`
}

type backyardRoute struct {
	CurrentApy         *backyardCurrentApy    `json:"currentApy"`
	CurrentOperationID *string                `json:"currentOperationId"`
	AumUsd             *float64               `json:"aumUsd"`
	CollateralUsd      *float64               `json:"collateralUsd"`
	CustodyUsd         *float64               `json:"custodyUsd"`
	DebtUsd            *float64               `json:"debtUsd"`
	LastHoldReason     *string                `json:"lastHoldReason"`
	LeverageTarget     *float64               `json:"leverageTarget"`
	LtvRatio           *float64               `json:"ltvRatio"`
	UpApproved         *bool                  `json:"upApproved"`
	LeverageWatch      *backyardLeverageWatch `json:"leverageWatch"`
	NavFresh           *bool                  `json:"navFresh"`
	NavUsd             *float64               `json:"navUsd"`
	ReportAgeS         *int32                 `json:"reportAgeS"`
	ReportObservedAt   *string                `json:"reportObservedAt"`
	RouteStatus        *string                `json:"routeStatus"`
	SelectorAction     *string                `json:"selectorAction"`
	SelectorReason     *string                `json:"selectorReason"`
	UpdatedAgeS        int32                  `json:"updatedAgeS"`
	VoltrIdleUsd       *float64               `json:"voltrIdleUsd"`
}

type backyardNavPoint struct {
	At     string  `json:"at"`
	NavUsd float64 `json:"navUsd"`
}

type backyardHealth struct {
	NavAll               int64    `json:"navAll"`
	NavReconciled        int64    `json:"navReconciled"`
	OpenStops            []string `json:"openStops"`
	RefusedBeforeSending int32    `json:"refusedBeforeSending"`
}

type backyardMove struct {
	Action      string  `json:"action"`
	AmountRaw   *string `json:"amountRaw"`
	At          string  `json:"at"`
	Reason      *string `json:"reason"`
	Signature   *string `json:"signature"`
	Status      string  `json:"status"`
	StrategyKey *string `json:"strategyKey"`
}

type backyardHistory struct {
	At        string   `json:"at"`
	EquityUsd *float64 `json:"equityUsd"`
	LtvPct    *float64 `json:"ltvPct"`
}

type backyardDailyPrice struct {
	Day   string  `json:"day"`
	Price float64 `json:"price"`
}

type backyardTvlPoint struct {
	At    string  `json:"at"`
	Value float64 `json:"value"`
}

type backyardVoltr struct {
	Daily         []backyardDailyPrice `json:"daily"`
	SharePrice    float64              `json:"sharePrice"`
	TotalValueUsd *float64             `json:"totalValueUsd"`
	Tvl           []backyardTvlPoint   `json:"tvl"`
}

type backyardBorrowRow struct {
	BorrowApyPct float64 `json:"borrowApyPct"`
	Lane         string  `json:"lane"`
	RoomTo90Usd  float64 `json:"roomTo90Usd"`
	SupplyUsd    float64 `json:"supplyUsd"`
	Token        string  `json:"token"`
	Utilization  float64 `json:"utilization"`
}

type backyardBreakdownPoint struct {
	At        string  `json:"at"`
	BorrowPct float64 `json:"borrowPct"`
	SupplyPct float64 `json:"supplyPct"`
}

type backyardBreakdown struct {
	Lane   string                   `json:"lane"`
	Points []backyardBreakdownPoint `json:"points"`
}

func backyardRec(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func backyardNum(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if n == "" || err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func backyardStr(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func backyardUsd(v any) *float64 {
	n := backyardNum(v)
	if n == nil {
		return nil
	}
	usd := *n / 1e6
	return &usd
}

func backyardISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type backyardSource struct{ pool *pgxpool.Pool }

func (s backyardSource) read(ctx context.Context, fn func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, backyardQueryTimeout)
	defer cancel()
	return db.WithTx(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(backyardQueryTimeout.Milliseconds(), 10)); err != nil {
			return err
		}
		return fn(tx)
	})
}

func backyardQuery[T any](ctx context.Context, s backyardSource, text string, scan func(pgx.Rows) (T, error), args ...any) ([]T, error) {
	out := []T{}
	err := s.read(ctx, func(tx pgx.Tx) error {
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

func backyardLaneRank(lane string) int {
	head, _, _ := strings.Cut(lane, "/")
	for i, name := range backyardLaneOrder {
		if name == head {
			return i
		}
	}
	return len(backyardLaneOrder)
}

func backyardLeverageLanes(watch map[string]any, watchAt time.Time) []backyardLeverageLane {
	lanes := []backyardLeverageLane{}
	raws, _ := watch["lanes"].([]any)
	for _, raw := range raws {
		lane := backyardRec(raw)
		apyPct := map[string]float64{}
		for level, bps := range backyardRec(lane["apyBps"]) {
			if f, err := strconv.ParseFloat(level, 64); err == nil {
				level = strconv.FormatFloat(f, 'f', -1, 64)
			}
			if n := backyardNum(bps); n != nil {
				apyPct[level] = *n / 100
			}
		}
		levels := []float64{}
		list, _ := lane["levels"].([]any)
		for _, item := range list {
			if level := backyardNum(item); level != nil {
				levels = append(levels, *level)
			}
		}
		name := "unknown"
		if s := backyardStr(lane["lane"]); s != nil {
			name = *s
		}
		older := false
		if at := backyardStr(lane["observedAt"]); at != nil {
			if t, err := time.Parse(time.RFC3339Nano, *at); err == nil {
				older = t.Before(watchAt)
			}
		}
		enterable, _ := lane["enterable"].(bool)
		spread := 0.0
		if n := backyardNum(lane["spreadBps"]); n != nil {
			spread = *n / 100
		}
		lanes = append(lanes, backyardLeverageLane{ApyPct: apyPct, Enterable: enterable, Lane: name, Levels: levels, Older: older, SpreadPct: spread})
	}
	sort.SliceStable(lanes, func(i, j int) bool { return backyardLaneRank(lanes[i].Lane) < backyardLaneRank(lanes[j].Lane) })
	return lanes
}

func (s backyardSource) route(ctx context.Context) (backyardRoute, error) {
	var route backyardRoute
	var raw []byte
	err := s.read(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT s.state,
       extract(epoch FROM now() - s.updated_at)::int AS updated_age_s,
       extract(epoch FROM now() - (s.state->'observation'->>'reportObservedAt')::timestamptz)::int AS report_age_s,
       levels.level, levels.up_approved
     FROM loyal_yield.multiply_route_states s
     LEFT JOIN loyal_yield.backyard_lane_levels levels
       ON levels.route_key = s.route_key AND levels.lane = s.state #>> '{currentApy,lane}'
     WHERE s.route_key = $1`, backyardRouteKey).
			Scan(&raw, &route.UpdatedAgeS, &route.ReportAgeS, &route.LeverageTarget, &route.UpApproved); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT expected_effects #>> '{decision,reason}'
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND action = 'HOLD' AND created_at > now() - interval '1 hour'
     ORDER BY created_at DESC LIMIT 1`, backyardRouteKey).Scan(&route.LastHoldReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return route, err
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		return route, err
	}
	observation := backyardRec(state["observation"])
	apy := backyardRec(state["currentApy"])
	selector := state["selectorResult"]
	if result, ok := backyardRec(state["selector"])["result"]; ok {
		selector = result
	}
	watch := backyardRec(state["leverageWatch"])

	if _, ok := apy["apyBps"]; ok {
		flat, _ := apy["flat"].(bool)
		pct := 0.0
		if n := backyardNum(apy["apyBps"]); n != nil {
			pct = *n / 100
		}
		route.CurrentApy = &backyardCurrentApy{ApyPct: pct, Flat: flat, Lane: backyardStr(apy["lane"]), Level: backyardNum(apy["level"]), ObservedAt: backyardStr(apy["observedAt"])}
	}
	route.CurrentOperationID = backyardStr(state["currentOperationId"])
	route.DebtUsd = backyardUsd(observation["positionDebtValueRaw"])
	route.CollateralUsd = backyardUsd(observation["positionCollateralValueRaw"])
	if ltv := backyardNum(observation["ltvBps"]); ltv != nil {
		ratio := *ltv / 1e4
		route.LtvRatio = &ratio
	}
	route.AumUsd = backyardUsd(observation["aumUsdMicros"])
	if at := backyardStr(watch["observedAt"]); at != nil {
		if t, err := time.Parse(time.RFC3339Nano, *at); err == nil {
			route.LeverageWatch = &backyardLeverageWatch{Lanes: backyardLeverageLanes(watch, t), ObservedAt: *at}
		}
	}
	if fresh, ok := observation["navFresh"].(bool); ok {
		route.NavFresh = &fresh
	}
	route.NavUsd = backyardUsd(observation["computedStrategyNavRaw"])
	if observation["navKnown"] == false {
		route.NavUsd = nil
		route.AumUsd = nil
		fresh := false
		route.NavFresh = &fresh
	}
	route.CustodyUsd = backyardUsd(observation["squadsIdleRaw"])
	route.ReportObservedAt = backyardStr(observation["reportObservedAt"])
	route.RouteStatus = backyardStr(observation["routeStatus"])
	route.SelectorAction = backyardStr(backyardRec(selector)["action"])
	route.SelectorReason = backyardStr(backyardRec(selector)["reason"])
	route.VoltrIdleUsd = backyardUsd(observation["voltrIdleRaw"])
	return route, nil
}

// backyardReportNAV reads a landed NAV report: the adaptor's verified return
// bytes when present, otherwise the built request (as the admin page does).
func backyardReportNAV(program, returnData, request *string) (float64, bool) {
	if returnData != nil && program != nil && *program == backyardNAVAdaptor {
		decoded, err := base64.StdEncoding.DecodeString(*returnData)
		if err == nil && len(decoded) == 8 {
			return float64(binary.LittleEndian.Uint64(decoded)), true
		}
	}
	if request == nil {
		return 0, false
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(strings.TrimSpace(*request))
		if err != nil {
			continue
		}
		var body map[string]any
		if json.Unmarshal(decoded, &body) != nil {
			return 0, false
		}
		if nav := backyardNum(backyardRec(body["Report"])["NAVAfterRaw"]); nav != nil {
			return *nav, true
		}
		return 0, false
	}
	return 0, false
}

func (s backyardSource) navSeries(ctx context.Context) ([]backyardNavPoint, error) {
	type report struct {
		at                           time.Time
		program, returnData, request *string
	}
	reports, err := backyardQuery(ctx, s, `SELECT DISTINCT ON (date_bin('30 minutes', created_at, 'epoch'::timestamptz))
       created_at,
       expected_effects #>> '{expectedEffects,returnData,programId}',
       expected_effects #>> '{expectedEffects,returnData,dataBase64}',
       expected_effects #>> '{phase3,buildInput,request}'
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND action = 'REPORT_NAV' AND status = 'reconciled'
       AND created_at > now() - interval '24 hours'
     ORDER BY date_bin('30 minutes', created_at, 'epoch'::timestamptz), created_at DESC`,
		func(rows pgx.Rows) (report, error) {
			var r report
			err := rows.Scan(&r.at, &r.program, &r.returnData, &r.request)
			return r, err
		}, backyardRouteKey)
	series := []backyardNavPoint{}
	for _, r := range reports {
		if nav, ok := backyardReportNAV(r.program, r.returnData, r.request); ok {
			series = append(series, backyardNavPoint{At: backyardISO(r.at), NavUsd: nav / 1e6})
		}
	}
	return series, err
}

func (s backyardSource) health(ctx context.Context) (backyardHealth, error) {
	health := backyardHealth{OpenStops: []string{}}
	err := s.read(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT status, count(*)::int
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND action = 'REPORT_NAV' AND created_at > now() - interval '24 hours'
       GROUP BY 1`, backyardRouteKey)
		if err != nil {
			return err
		}
		for rows.Next() {
			var status string
			var count int32
			if err := rows.Scan(&status, &count); err != nil {
				rows.Close()
				return err
			}
			health.NavAll += int64(count)
			if status == "reconciled" {
				health.NavReconciled += int64(count)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Refused by the worker's own pre-send checks: never reached the chain.
		if err := tx.QueryRow(ctx, `SELECT count(*)::int
       FROM loyal_yield.multiply_operations
       WHERE route_key = $1 AND status = 'failed' AND transaction_signature IS NULL
         AND created_at > now() - interval '24 hours'
         AND action NOT IN ('HOLD', 'REPORT_NAV')`, backyardRouteKey).Scan(&health.RefusedBeforeSending); err != nil {
			return err
		}
		stops, err := tx.Query(ctx, `SELECT reason FROM loyal_yield.backyard_manual_recovery_latches
       WHERE route_key = $1 AND cleared_at IS NULL`, backyardRouteKey)
		if err != nil {
			return err
		}
		health.OpenStops, err = pgx.AppendRows(health.OpenStops, stops, pgx.RowTo[string])
		return err
	})
	return health, err
}

const backyardMoneyActions = `action NOT IN ('HOLD', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED')`

func (s backyardSource) moves(ctx context.Context) ([]backyardMove, error) {
	return backyardQuery(ctx, s, `SELECT action, status, strategy_key, transaction_signature, created_at,
       -- A borrow's decision amount is 0 (its size comes from the leverage
       -- target); the amount is what landed in the vault's own accounts.
       CASE WHEN expected_effects #>> '{expectedEffects,kind}' = 'kamino-borrow'
         THEN (SELECT sum((a->>'afterRaw')::numeric - (a->>'beforeRaw')::numeric)::text
               FROM jsonb_array_elements(expected_effects #> '{expectedEffects,accounts}') a
               WHERE a->>'authority' = $2
                 AND (a->>'afterRaw')::numeric > (a->>'beforeRaw')::numeric)
         ELSE expected_effects #>> '{decision,amountRaw}'
       END,
       COALESCE(recovery_reason, expected_effects #>> '{decision,reason}')
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND transaction_signature IS NOT NULL AND `+backyardMoneyActions+`
     ORDER BY created_at DESC LIMIT 30`,
		func(rows pgx.Rows) (backyardMove, error) {
			var m backyardMove
			var created time.Time
			err := rows.Scan(&m.Action, &m.Status, &m.StrategyKey, &m.Signature, &created, &m.AmountRaw, &m.Reason)
			m.At = backyardISO(created)
			return m, err
		}, backyardRouteKey, backyardSquads)
}

// moveDays are the UTC days the worker changed the position on chain, for the
// chart's markers.
func (s backyardSource) moveDays(ctx context.Context) ([]string, error) {
	return backyardQuery(ctx, s, `SELECT DISTINCT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')
     FROM loyal_yield.multiply_operations
     WHERE route_key = $1 AND transaction_signature IS NOT NULL AND status = 'reconciled'
       AND `+backyardMoneyActions+` AND created_at > now() - interval '366 days'
     ORDER BY 1`, func(rows pgx.Rows) (string, error) {
		var day string
		err := rows.Scan(&day)
		return day, err
	}, backyardRouteKey)
}

func (s backyardSource) history(ctx context.Context) ([]backyardHistory, error) {
	return backyardQuery(ctx, s, `SELECT date_trunc('hour', observed_at),
       (avg(equity_usd_micros) / 1e6)::float8,
       (avg(ltv_bps) / 100)::float8
     FROM loyal_yield.multiply_position_snapshots
     WHERE route_key = $1 AND observed_at > now() - interval '7 days'
     GROUP BY 1 ORDER BY 1`,
		func(rows pgx.Rows) (backyardHistory, error) {
			var p backyardHistory
			var hour time.Time
			err := rows.Scan(&hour, &p.EquityUsd, &p.LtvPct)
			p.At = backyardISO(hour)
			return p, err
		}, backyardRouteKey)
}

func backyardVoltrLoad(ctx context.Context, now time.Time) (backyardVoltr, error) {
	var price earnmax.SharePrice
	var priceErr error
	var wg sync.WaitGroup
	wg.Go(func() { price, priceErr = earnmax.FetchVaultSharePrice(ctx, backyardVoltrVault) })
	days, err := earnmax.FetchVaultDailyStats(ctx, backyardVoltrVault)
	wg.Wait()
	if err != nil {
		return backyardVoltr{}, err
	}
	if priceErr != nil {
		return backyardVoltr{}, priceErr
	}
	out := backyardVoltr{Daily: []backyardDailyPrice{}, SharePrice: price.PerLP, Tvl: []backyardTvlPoint{}}
	if price.TotalValue != nil {
		total := *price.TotalValue / 1e6
		out.TotalValueUsd = &total
	}
	today := now.UTC().Format("2006-01-02")
	for _, day := range days {
		if day.TVLRaw != 0 && day.LPRaw != 0 {
			out.Daily = append(out.Daily, backyardDailyPrice{Day: day.Day, Price: day.TVLRaw / day.LPRaw})
		}
		// Today's Voltr point is unfinished; the live total replaces it.
		if day.Day < today {
			out.Tvl = append(out.Tvl, backyardTvlPoint{At: day.Day + "T23:59:59.000Z", Value: day.TVLRaw / 1e6})
		}
	}
	if out.TotalValueUsd != nil {
		out.Tvl = append(out.Tvl, backyardTvlPoint{At: backyardISO(now), Value: *out.TotalValueUsd})
	}
	return out, nil
}

var backyardClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func backyardGet(ctx context.Context, target string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, backyardHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := backyardClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed", req.URL.Hostname())
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %d", req.URL.Hostname(), resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, backyardMaxBody+1))
	if err != nil || len(raw) > backyardMaxBody {
		return fmt.Errorf("%s response unreadable or too large", req.URL.Hostname())
	}
	return json.Unmarshal(raw, out)
}

func backyardBorrowing(ctx context.Context) ([]backyardBorrowRow, error) {
	rows := make([]backyardBorrowRow, len(backyardBorrowLanes))
	errs := make([]error, len(backyardBorrowLanes))
	var wg sync.WaitGroup
	for i, lane := range backyardBorrowLanes {
		wg.Go(func() {
			reserves := backyardLanes[lane]
			var metrics []map[string]any
			endpoint := fmt.Sprintf("https://api.kamino.finance/kamino-market/%s/reserves/metrics?env=mainnet-beta", url.PathEscape(reserves.market))
			if errs[i] = backyardGet(ctx, endpoint, &metrics); errs[i] != nil {
				return
			}
			var row map[string]any
			for _, item := range metrics {
				if item["reserve"] == reserves.debt {
					row = item
					break
				}
			}
			supply, borrow := backyardNum(row["totalSupplyUsd"]), backyardNum(row["totalBorrowUsd"])
			if supply == nil || borrow == nil || *supply <= 0 {
				errs[i] = fmt.Errorf("kamino has no metrics for the %s debt reserve", lane)
				return
			}
			token := "?"
			if s := backyardStr(row["liquidityToken"]); s != nil {
				token = *s
			}
			apy := 0.0
			if n := backyardNum(row["borrowApy"]); n != nil {
				apy = *n * 100
			}
			rows[i] = backyardBorrowRow{BorrowApyPct: apy, Lane: lane, RoomTo90Usd: math.Max(0.9**supply-*borrow, 0), SupplyUsd: *supply, Token: token, Utilization: *borrow / *supply}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

type backyardReserveDay struct {
	day                        string
	assetYield, supply, borrow float64
}

func backyardReserveHistory(ctx context.Context, market, reserve string, start, end time.Time) ([]backyardReserveDay, error) {
	var body struct {
		History []struct {
			Timestamp string         `json:"timestamp"`
			Metrics   map[string]any `json:"metrics"`
		} `json:"history"`
	}
	endpoint := fmt.Sprintf("https://api.kamino.finance/kamino-market/%s/reserves/%s/metrics/history?env=mainnet-beta&start=%s&end=%s&frequency=day",
		url.PathEscape(market), url.PathEscape(reserve), url.QueryEscape(start.UTC().Format(time.RFC3339)), url.QueryEscape(end.UTC().Format(time.RFC3339)))
	if err := backyardGet(ctx, endpoint, &body); err != nil {
		return nil, err
	}
	days := []backyardReserveDay{}
	or0 := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	for _, point := range body.History {
		if len(point.Timestamp) < 10 {
			continue
		}
		days = append(days, backyardReserveDay{
			day:        point.Timestamp[:10],
			assetYield: or0(backyardNum(point.Metrics["assetYieldApy"])),
			supply:     or0(backyardNum(point.Metrics["supplyInterestAPY"])),
			borrow:     or0(backyardNum(point.Metrics["borrowInterestAPY"])),
		})
	}
	return days, nil
}

// backyardBreakdownLoad compares, per day, what the lane's collateral earns
// (its own yield plus Kamino supply interest) with what its debt costs.
func backyardBreakdownLoad(ctx context.Context, lane string, start, end time.Time) (backyardBreakdown, error) {
	reserves, ok := backyardLanes[lane]
	if !ok {
		lane, reserves = backyardDefault, backyardLanes[backyardDefault]
	}
	var debt []backyardReserveDay
	var debtErr error
	var wg sync.WaitGroup
	wg.Go(func() { debt, debtErr = backyardReserveHistory(ctx, reserves.market, reserves.debt, start, end) })
	collateral, err := backyardReserveHistory(ctx, reserves.market, reserves.collateral, start, end)
	wg.Wait()
	if err != nil {
		return backyardBreakdown{}, err
	}
	if debtErr != nil {
		return backyardBreakdown{}, debtErr
	}
	borrowByDay := map[string]float64{}
	for _, d := range debt {
		borrowByDay[d.day] = d.borrow
	}
	out := backyardBreakdown{Lane: lane, Points: []backyardBreakdownPoint{}}
	for _, c := range collateral {
		if borrow, ok := borrowByDay[c.day]; ok {
			out.Points = append(out.Points, backyardBreakdownPoint{At: c.day + "T00:00:00.000Z", BorrowPct: borrow * 100, SupplyPct: (c.assetYield + c.supply) * 100})
		}
	}
	return out, nil
}

func (s backyardSource) page(ctx context.Context) (backyardPage, error) {
	now := time.Now()
	page := backyardPage{LoadedAt: backyardISO(now)}
	var wg sync.WaitGroup
	wg.Go(func() { page.Route = backyardLoad("route", func() (backyardRoute, error) { return s.route(ctx) }) })
	wg.Go(func() {
		page.NavSeries = backyardLoad("nav_series", func() ([]backyardNavPoint, error) { return s.navSeries(ctx) })
	})
	wg.Go(func() { page.Health = backyardLoad("health", func() (backyardHealth, error) { return s.health(ctx) }) })
	wg.Go(func() { page.Moves = backyardLoad("moves", func() ([]backyardMove, error) { return s.moves(ctx) }) })
	wg.Go(func() { page.MoveDays = backyardLoad("move_days", func() ([]string, error) { return s.moveDays(ctx) }) })
	wg.Go(func() {
		page.History = backyardLoad("history", func() ([]backyardHistory, error) { return s.history(ctx) })
	})
	wg.Go(func() {
		page.Voltr = backyardLoad("voltr", func() (backyardVoltr, error) { return backyardVoltrLoad(ctx, now) })
	})
	wg.Go(func() {
		page.Borrowing = backyardLoad("borrowing", func() ([]backyardBorrowRow, error) { return backyardBorrowing(ctx) })
	})
	wg.Wait()

	// The chart follows the lane the worker is in now.
	lane := backyardDefault
	if page.Route.Ok && page.Route.Value.CurrentApy != nil && page.Route.Value.CurrentApy.Lane != nil {
		lane = *page.Route.Value.CurrentApy.Lane
	} else if page.Moves.Ok {
		for _, m := range *page.Moves.Value {
			if m.StrategyKey != nil {
				lane = *m.StrategyKey
				break
			}
		}
	}
	start := now.Add(-30 * 24 * time.Hour)
	if page.Voltr.Ok && len(page.Voltr.Value.Daily) > 0 {
		if first, err := time.Parse("2006-01-02", page.Voltr.Value.Daily[0].Day); err == nil {
			start = first
		}
	}
	page.Breakdown = backyardLoad("breakdown", func() (backyardBreakdown, error) { return backyardBreakdownLoad(ctx, lane, start, now) })
	return page, nil
}

// backyardDashboard serves GET /api/backyard: one load per minute shared by
// every viewer, the last good page served while a refresh runs.
type backyardDashboard struct {
	source backyardSource
	cache  *earnmax.StaleWhileRevalidate[[]byte]
}

func newBackyardDashboard(pool *pgxpool.Pool) *backyardDashboard {
	return &backyardDashboard{
		source: backyardSource{pool: pool},
		cache:  &earnmax.StaleWhileRevalidate[[]byte]{Fresh: time.Minute, Stale: 10 * time.Minute, Wait: 3 * time.Second, Budget: 45 * time.Second},
	}
}

func (b *backyardDashboard) load(ctx context.Context) ([]byte, error) {
	page, err := b.source.page(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(page)
}

func (b *backyardDashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	body, have, _ := b.cache.Get(b.load)
	if !have {
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(`{"error":"backyard dashboard unavailable"}`))
		}
		return
	}
	h.Set("Cache-Control", "public, max-age=30, s-maxage=60, stale-while-revalidate=300")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// backyardHostOnly keeps backyard.askloyal.com to its one page: the page,
// its API and the build's static files. Everything else there is a 404.
func backyardHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "backyard.") {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		staticFile := strings.HasPrefix(p, "/assets/") || (strings.Contains(p[strings.LastIndex(p, "/")+1:], ".") && !strings.HasPrefix(p, "/api/"))
		if p == "/" || p == "/api/backyard" || p == "/healthz" || p == "/readyz" || staticFile {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
	})
}
