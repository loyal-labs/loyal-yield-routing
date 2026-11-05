package earnstats

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	earnMaxValuationURL     = "https://api.voltr.xyz/vault/HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA/share-price"
	valuationTimeout        = 3 * time.Second
	statsQueryTimeout       = "SET LOCAL statement_timeout = '50000ms'"
	slackTimeout            = 5 * time.Second
	slackAttempts           = 2
	aumAlertThresholdRaw    = 5_000 * 1_000_000
	rawPerCent              = 10_000
	maxSafeInteger          = 1<<53 - 1
	refreshInterval         = time.Minute
	refreshBound            = 58 * time.Second
	statusSkippedNoPrevious = "skipped_no_previous_snapshot"
)

// refresh.sql is the exact query text the telegram app sent to Yield Neon, so the
// two writers cannot disagree on a number.
//
//go:embed refresh.sql
var yieldStatsSQL string

type SlackStatus string

const (
	SlackSkippedNoPrevious SlackStatus = statusSkippedNoPrevious
	SlackBelowThreshold    SlackStatus = "below_threshold"
	SlackNotConfigured     SlackStatus = "not_configured"
	SlackSent              SlackStatus = "sent"
	SlackFailed            SlackStatus = "failed"
)

type AumPoint struct {
	AumRaw    string `json:"aumRaw"`
	WeekEnd   string `json:"weekEnd"`
	WeekStart string `json:"weekStart"`
}

type Refreshed struct {
	TotalAumRaw             int64
	TotalOptimizedVolumeRaw int64
	ActivePrincipalRaw      int64
	TotalUsers              int
	UniqueEarnUsers         int
	UniqueEarnPolicies      int
	ActiveAutodepositPolicy int
	Series                  []AumPoint
}

type RefreshResult struct {
	Skipped     bool
	RefreshedAt time.Time
	Slack       SlackStatus
}

type Refresher struct {
	App          *pgxpool.Pool
	Yield        *pgxpool.Pool
	HTTP         *http.Client
	ValuationURL string
	SlackURL     string
}

func NewRefresher(app, yield *pgxpool.Pool, slackURL string) *Refresher {
	return &Refresher{App: app, Yield: yield, HTTP: &http.Client{}, ValuationURL: earnMaxValuationURL, SlackURL: slackURL}
}

func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		r.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Refresher) runOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, refreshBound)
	defer cancel()
	started := time.Now()
	result, err := r.Refresh(ctx)
	elapsed := time.Since(started).Milliseconds()
	switch {
	case err != nil:
		slog.Error("stats snapshot refresh failed", "elapsedMs", elapsed, "error", err)
	case result.Skipped:
		slog.Info("stats snapshot refresh already running", "elapsedMs", elapsed)
	default:
		slog.Info("stats snapshot refreshed", "elapsedMs", elapsed, "refreshedAt", result.RefreshedAt.Format(time.RFC3339Nano), "slackAlertStatus", result.Slack)
	}
}

func (r *Refresher) Refresh(ctx context.Context) (RefreshResult, error) {
	previousAum, previousSeries, hasPrevious, err := r.loadPrevious(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	totalUsers, err := r.countUsers(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	stats, ok, err := r.loadYield(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	if !ok {
		return RefreshResult{Skipped: true}, nil
	}
	nav, err := r.loadValuation(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	if stats.TotalAumRaw > math.MaxInt64-nav {
		return RefreshResult{}, errors.New("total AUM overflows bigint")
	}
	stats.TotalAumRaw += nav
	stats.TotalUsers = totalUsers
	if current := len(stats.Series); current > 0 {
		stats.Series[current-1].AumRaw = strconv.FormatInt(stats.TotalAumRaw, 10)
	}
	stats.Series = preserveCompletedWeeks(stats.Series, previousSeries)
	refreshedAt := time.Now().UTC()
	if err := r.upsert(ctx, stats, refreshedAt); err != nil {
		return RefreshResult{}, err
	}
	slack := SlackStatus(statusSkippedNoPrevious)
	if hasPrevious {
		slack = r.alert(ctx, previousAum, stats.TotalAumRaw)
	}
	return RefreshResult{RefreshedAt: refreshedAt, Slack: slack}, nil
}

func (r *Refresher) loadPrevious(ctx context.Context) (aum int64, series []AumPoint, ok bool, err error) {
	var raw json.RawMessage
	err = r.App.QueryRow(ctx, `SELECT total_aum_raw, earn_aum_series FROM loyal_stats_snapshots WHERE snapshot_key = 'current' LIMIT 1`).Scan(&aum, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	if series, err = parseSeries(raw); err != nil {
		return 0, nil, false, err
	}
	return aum, series, true, nil
}

func (r *Refresher) countUsers(ctx context.Context) (int, error) {
	var n int64
	if err := r.App.QueryRow(ctx, `SELECT count(*) FROM app_users`).Scan(&n); err != nil {
		return 0, err
	}
	if n < 0 || n > math.MaxInt32 {
		return 0, errors.New("invalid Loyal stats query result")
	}
	return int(n), nil
}

func (r *Refresher) loadYield(ctx context.Context) (*Refreshed, bool, error) {
	tx, err := r.Yield.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, statsQueryTimeout); err != nil {
		return nil, false, err
	}
	var aum, principal, volume, users, policies, autodeposit string
	var series json.RawMessage
	err = tx.QueryRow(ctx, yieldStatsSQL).Scan(&aum, &principal, &volume, &users, &policies, &autodeposit, &series)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	out := &Refreshed{}
	if out.TotalAumRaw, err = parseRaw(aum, "total AUM"); err != nil {
		return nil, false, err
	}
	if out.ActivePrincipalRaw, err = parseRaw(principal, "active principal"); err != nil {
		return nil, false, err
	}
	if out.TotalOptimizedVolumeRaw, err = parseRaw(volume, "total optimized volume"); err != nil {
		return nil, false, err
	}
	if out.UniqueEarnUsers, err = parseCount(users, "unique Earn user count"); err != nil {
		return nil, false, err
	}
	if out.UniqueEarnPolicies, err = parseCount(policies, "unique Earn policy count"); err != nil {
		return nil, false, err
	}
	if out.ActiveAutodepositPolicy, err = parseCount(autodeposit, "active Autodeposit policy count"); err != nil {
		return nil, false, err
	}
	if out.Series, err = parseSeries(series); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func parseRaw(value, label string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s returned by Yield Neon", label)
	}
	return n, nil
}

func parseCount(value, label string) (int, error) {
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s returned by Yield Neon", label)
	}
	return int(n), nil
}

func parseSeries(raw json.RawMessage) ([]AumPoint, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return nil, errors.New("invalid Earn AUM series returned by Yield Neon")
	}
	var points []*struct {
		AumRaw    *string `json:"aumRaw"`
		WeekEnd   *string `json:"weekEnd"`
		WeekStart *string `json:"weekStart"`
	}
	if err := json.Unmarshal(raw, &points); err != nil {
		return nil, errors.New("invalid Earn AUM series point returned by Yield Neon")
	}
	out := make([]AumPoint, len(points))
	for i, p := range points {
		if p == nil || p.AumRaw == nil || p.WeekEnd == nil || p.WeekStart == nil {
			return nil, errors.New("invalid Earn AUM series point returned by Yield Neon")
		}
		if _, err := parseRaw(*p.AumRaw, "Earn AUM series value"); err != nil {
			return nil, err
		}
		out[i] = AumPoint{AumRaw: *p.AumRaw, WeekEnd: *p.WeekEnd, WeekStart: *p.WeekStart}
	}
	return out, nil
}

func preserveCompletedWeeks(current, previous []AumPoint) []AumPoint {
	if len(current) == 0 || len(previous) == 0 {
		return current
	}
	currentWeek := current[len(current)-1].WeekStart
	byWeek := make(map[string]AumPoint, len(previous))
	for _, p := range previous {
		byWeek[p.WeekStart] = p
	}
	out := make([]AumPoint, len(current))
	for i, p := range current {
		out[i] = p
		if p.WeekStart != currentWeek {
			if old, ok := byWeek[p.WeekStart]; ok {
				out[i] = old
			}
		}
	}
	return out
}

func (r *Refresher) loadValuation(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, valuationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ValuationURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("Earn MAX valuation failed: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Success any `json:"success"`
		Data    *struct {
			TotalValue any `json:"totalValue"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return 0, err
	}
	var value json.Number
	if body.Data != nil {
		value, _ = body.Data.TotalValue.(json.Number)
	}
	f, err := value.Float64()
	if body.Success != true || value == "" || err != nil || f < 0 || f > maxSafeInteger || f != math.Trunc(f) {
		return 0, errors.New("invalid Earn MAX valuation returned by Voltr")
	}
	return int64(f), nil
}

func (r *Refresher) upsert(ctx context.Context, s *Refreshed, refreshedAt time.Time) error {
	series, err := json.Marshal(s.Series)
	if err != nil {
		return err
	}
	_, err = r.App.Exec(ctx, `INSERT INTO loyal_stats_snapshots (
  snapshot_key, total_aum_raw, total_users, total_optimized_volume_raw, active_principal_raw,
  unique_earn_users, unique_earn_policies, active_autodeposit_policies, earn_aum_series, refreshed_at, updated_at
) VALUES ('current', $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $9)
ON CONFLICT (snapshot_key) DO UPDATE SET
  total_aum_raw = EXCLUDED.total_aum_raw,
  total_users = EXCLUDED.total_users,
  total_optimized_volume_raw = EXCLUDED.total_optimized_volume_raw,
  active_principal_raw = EXCLUDED.active_principal_raw,
  unique_earn_users = EXCLUDED.unique_earn_users,
  unique_earn_policies = EXCLUDED.unique_earn_policies,
  active_autodeposit_policies = EXCLUDED.active_autodeposit_policies,
  earn_aum_series = EXCLUDED.earn_aum_series,
  refreshed_at = EXCLUDED.refreshed_at,
  updated_at = EXCLUDED.updated_at
WHERE loyal_stats_snapshots.refreshed_at <= EXCLUDED.refreshed_at`,
		s.TotalAumRaw, s.TotalUsers, s.TotalOptimizedVolumeRaw, s.ActivePrincipalRaw,
		s.UniqueEarnUsers, s.UniqueEarnPolicies, s.ActiveAutodepositPolicy, string(series), refreshedAt)
	return err
}

func (r *Refresher) alert(ctx context.Context, previous, current int64) SlackStatus {
	delta := current - previous
	magnitude := delta
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if magnitude < aumAlertThresholdRaw {
		return SlackBelowThreshold
	}
	if r.SlackURL == "" {
		slog.Error("stats Slack AUM alert is not configured", "currentAumRaw", current, "deltaRaw", delta)
		return SlackNotConfigured
	}
	direction, icon := "increased", "📈"
	if delta < 0 {
		direction, icon = "decreased", "📉"
	}
	text := fmt.Sprintf("%s Earn AUM %s by %s\nCurrent Earn AUM: %s", icon, direction, formatUSDC(magnitude), formatUSDC(current))
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return SlackFailed
	}
	for attempt := 0; attempt < slackAttempts; attempt++ {
		retry, sent := r.postSlack(ctx, body)
		if sent {
			return SlackSent
		}
		if !retry {
			break
		}
	}
	slog.Error("stats Slack AUM alert failed", "currentAumRaw", current, "deltaRaw", delta)
	return SlackFailed
}

func (r *Refresher) postSlack(ctx context.Context, body []byte) (retry, sent bool) {
	ctx, cancel := context.WithTimeout(ctx, slackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.SlackURL, bytes.NewReader(body))
	if err != nil {
		return false, false
	}
	req.Header.Set("content-type", "application/json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return true, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return false, true
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, false
}

func formatUSDC(raw int64) string {
	cents := (raw + rawPerCent/2) / rawPerCent
	whole := strconv.FormatInt(cents/100, 10)
	var grouped strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return fmt.Sprintf("$%s.%02d", grouped.String(), cents%100)
}
