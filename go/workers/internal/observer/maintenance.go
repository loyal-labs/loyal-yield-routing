package observer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	workersdb "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
)

const benchmarkReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"

// Maintenance owns projections only. Pools are supplied/closed by the runtime;
// no keypair, signing, broadcast or application repair capability is accepted.
// The fixed stablecoin/benchmark catalog is mainnet-only. Configuration must
// positively bind its Yield/Timescale product namespace and RPC to mainnet-beta;
// the unscoped legacy source tables do not prove that binding themselves.
type Maintenance struct {
	yield, timescale   *pgxpool.Pool
	cluster            string
	markets            []string
	maxVaults, maxRows int
	timeout            time.Duration
	rpc                ReservePriceRPC
	poll, observation  time.Duration
	mu                 sync.Mutex
	pricesHour         time.Time
	logger             *slog.Logger
	onError            func()
	retryInterval      time.Duration
	validateNamespace  func(context.Context) error
}
type MaintenanceConfig struct {
	Cluster                         string
	MediumMarkets                   []string
	MaxVaults, MaxModelRows         int
	Timeout                         time.Duration
	PriceRPC                        ReservePriceRPC
	RecoveryPoll, HealthObservation time.Duration
	Logger                          *slog.Logger
	// OnError is called for every failed pass; it must return promptly.
	OnError       func()
	RetryInterval time.Duration
	// ValidateNamespace rechecks the custody namespace before any unscoped
	// product projection. The runtime supplies it because policy membership can
	// change after startup; a foreign or unknown scope must not publish.
	ValidateNamespace func(context.Context) error
}

func NewMaintenance(yield, timescale *pgxpool.Pool, c MaintenanceConfig) (*Maintenance, error) {
	if yield == nil || timescale == nil || c.PriceRPC == nil || c.Cluster != "mainnet-beta" || len(c.MediumMarkets) == 0 || len(c.MediumMarkets) > 32 {
		return nil, errors.New("maintenance requires read-only RPC, scoped databases and mainnet-beta product namespace")
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = time.Minute
	}
	if c.OnError == nil {
		return nil, errors.New("maintenance requires a failure consumer")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.RecoveryPoll == 0 {
		c.RecoveryPoll = 5 * time.Second
	}
	if c.HealthObservation == 0 {
		c.HealthObservation = 5 * time.Second
	}
	if c.RecoveryPoll < time.Millisecond || c.RecoveryPoll > time.Minute || c.HealthObservation < time.Millisecond || c.HealthObservation > time.Minute || c.RetryInterval < c.HealthObservation || c.RetryInterval > time.Minute {
		return nil, errors.New("invalid health cadence")
	}
	if c.MaxVaults == 0 {
		c.MaxVaults = 10_000
	}
	if c.MaxModelRows == 0 {
		c.MaxModelRows = 100_000
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxVaults < 1 || c.MaxVaults > 10_000 || c.MaxModelRows < 1 || c.MaxModelRows > 100_000 || c.Timeout <= 0 || c.Timeout > time.Minute {
		return nil, errors.New("invalid maintenance bounds")
	}
	markets := append([]string(nil), c.MediumMarkets...)
	for _, market := range markets {
		if _, err := solana.PublicKeyFromBase58(market); err != nil {
			return nil, errors.New("invalid mainnet medium market identity")
		}
	}
	return &Maintenance{yield: yield, timescale: timescale, cluster: c.Cluster, markets: markets, maxVaults: c.MaxVaults, maxRows: c.MaxModelRows, timeout: c.Timeout, rpc: c.PriceRPC, poll: c.RecoveryPoll, observation: c.HealthObservation, logger: c.Logger, onError: c.OnError, retryInterval: c.RetryInterval, validateNamespace: c.ValidateNamespace}, nil
}
func (m *Maintenance) RequireSchema(ctx context.Context) error {
	if err := workersdb.RequireTables(ctx, m.yield, "loyal_yield.managed_vaults", "loyal_yield.user_yield_positions", "loyal_yield.vault_position_snapshots", "loyal_yield.vault_position_snapshot_positions", "loyal_yield.vault_idle_token_balances_current", "loyal_yield.earn_fleet_allocations_hourly", "loyal_yield.earn_reserve_share_prices", "loyal_yield.earn_forecast_snapshots", "loyal_yield.fleet_orchestration_status", "loyal_yield.fleet_orchestration_health_snapshots", "loyal_yield.rebalance_opportunities", "loyal_yield.signed_route_submissions", "loyal_yield.orchestration_outbox"); err != nil {
		return err
	}
	return workersdb.RequireTables(ctx, m.timescale, "kamino.reserve_updates", "kamino.supported_reserves")
}

type MaintenanceReport struct {
	HealthPublished    bool
	HealthFence        int64
	Allocation         *AllocationSample
	SharePrices        int
	ModelPublished     bool
	MissingPrices      []string
	StageHealth        *StageHealthReport
	AllocationComplete bool
}

// Run joins all work by construction: each maintenance pass is synchronous and
// timeout bounded. Tick errors return to the runtime; no missing data is hidden.
func (m *Maintenance) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastStages := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		report, err := m.Tick(ctx, time.Now().UTC())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.onError()
			m.logger.WarnContext(ctx, "maintenance pass unavailable", "cluster", m.cluster, "error", err)
			timer.Reset(m.retryInterval)
			continue
		}
		stages := []string{}
		if report.StageHealth != nil {
			for _, stage := range report.StageHealth.StuckStages {
				stages = append(stages, stage.Stage)
			}
		}
		signature := strings.Join(stages, ",")
		if signature != lastStages {
			m.logger.WarnContext(ctx, "fleet derived stage health changed", "cluster", m.cluster, "stuck_stages", stages)
			lastStages = signature
		}
		if len(report.MissingPrices) > 0 {
			m.logger.WarnContext(ctx, "maintenance price evidence incomplete", "cluster", m.cluster, "missing_reserve_count", len(report.MissingPrices))
			timer.Reset(m.retryInterval)
		} else {
			timer.Reset(m.observation)
		}
	}
}
func (m *Maintenance) Tick(ctx context.Context, now time.Time) (MaintenanceReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r MaintenanceReport
	if now.IsZero() {
		return r, errors.New("maintenance observation time missing")
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if m.validateNamespace != nil {
		if err := m.validateNamespace(ctx); err != nil {
			return r, err
		}
	}
	published, fence, err := m.RefreshHealth(ctx, m.observation)
	r.HealthPublished, r.HealthFence = published, fence
	if err != nil {
		return r, err
	}
	var health []byte
	if err = m.yield.QueryRow(ctx, `SELECT payload FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster=$1`, m.cluster).Scan(&health); err != nil {
		return r, err
	}
	stage, err := StageHealth(health, m.poll, m.observation, now)
	if err != nil {
		return r, err
	}
	r.StageHealth = &stage
	var due bool
	if err = m.yield.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1 AND observed_hour=$2)`, m.cluster, now.UTC().Truncate(time.Hour)).Scan(&due); err != nil {
		return r, err
	}
	if due {
		sample, prices, err := m.RecordTelemetry(ctx, now)
		if err != nil {
			return r, err
		}
		r.Allocation = &sample
		r.SharePrices = prices
	}
	if err = m.yield.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1 AND observed_hour=$2 AND observed_at<=$3 AND vaults_total=vaults_included)`, m.cluster, now.UTC().Truncate(time.Hour), now).Scan(&r.AllocationComplete); err != nil {
		return r, err
	}
	hour := now.UTC().Truncate(time.Hour)
	if !m.pricesHour.Equal(hour) {
		count, missing, err := m.RecordSharePrices(ctx, now)
		if err != nil {
			return r, err
		}
		r.SharePrices += count
		r.MissingPrices = missing
		if len(missing) == 0 {
			m.pricesHour = hour
		}
	}
	// The public model is one snapshot per UTC day (the Apps cron ran it daily
	// at 08:17), so it is due once per snapshot_date, not every hour.
	if err = m.yield.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM loyal_yield.earn_forecast_snapshots WHERE strategy='medium_fee_aware_1bps' AND risk_profile='medium' AND fee_bps=1 AND snapshot_date=$1::date)`, now.UTC().Format("2006-01-02")).Scan(&due); err != nil {
		return r, err
	}
	if due {
		// The 30-day evidence read takes tens of seconds; it gets its own
		// budget instead of the remainder of the 5 s health tick's.
		if err = m.RecordPublicModel(parent, now); err != nil {
			return r, err
		}
		r.ModelPublished = true
	}
	return r, nil
}

func (m *Maintenance) loadVaults(ctx context.Context, tx pgx.Tx, now time.Time) ([]FleetVault, error) {
	rows, err := tx.Query(ctx, `
SELECT vault.id, snapshot.observed_at, snapshot.observed_slot, snapshot.context_idle,
 COALESCE((SELECT jsonb_agg(jsonb_build_object('reserve',position.reserve,'market',COALESCE(position.market,''),'liquidityMint',position.liquidity_mint,'amountRaw',position.amount_raw::text,'metadata',position.planning_metadata)) FROM (SELECT * FROM loyal_yield.vault_position_snapshot_positions WHERE snapshot_id=snapshot.id AND has_value=true ORDER BY reserve LIMIT 129) AS position),'[]'::jsonb),
 COALESCE((SELECT jsonb_agg(jsonb_build_object('mint',idle.mint,'amountRaw',idle.amount_raw::text,'slot',idle.observed_slot)) FROM (SELECT * FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=vault.id ORDER BY mint LIMIT 129) AS idle),'[]'::jsonb)
FROM loyal_yield.managed_vaults AS vault
LEFT JOIN LATERAL (
 SELECT source.id,source.observed_at,source.observed_slot,source.context->>'idle_vault_liquidity_amount_raw' AS context_idle
 FROM loyal_yield.vault_position_snapshots AS source
 WHERE source.vault_id=vault.id AND source.observed_at <= $1
 AND source.context->>'publication_scope'='complete_product_vault'
 ORDER BY source.observed_at DESC,source.observed_slot DESC,source.id DESC LIMIT 1
) AS snapshot ON true
WHERE vault.vault_index=1 AND vault.active=true AND vault.first_seen_at <= $1
ORDER BY vault.id LIMIT $2`, now, m.maxVaults+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FleetVault
	for rows.Next() {
		var v FleetVault
		var observed *time.Time
		var slot *int64
		var idle *string
		var positions, current []byte
		if err = rows.Scan(&v.ID, &observed, &slot, &idle, &positions, &current); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(current, &v.CurrentIdle); err != nil {
			return nil, err
		}
		if observed != nil {
			if slot == nil {
				return nil, errors.New("complete snapshot has no slot")
			}
			s := &FleetSnapshot{ObservedAt: *observed, Slot: *slot, ContextIdle: idle}
			if err = json.Unmarshal(positions, &s.Positions); err != nil {
				return nil, err
			}
			v.Snapshot = s
		}
		result = append(result, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) > m.maxVaults {
		return nil, errors.New("fleet exceeds configured telemetry bound; refusing partial allocation")
	}
	return result, nil
}
func (m *Maintenance) RecordTelemetry(ctx context.Context, now time.Time) (AllocationSample, int, error) {
	var sample AllocationSample
	prices := 0
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	err := workersdb.WithTx(ctx, m.yield, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		var acquired bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('earn-hourly-telemetry:' || $1,0))`, m.cluster).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return errors.New("another hourly telemetry observer owns this pass")
		}
		vaults, err := m.loadVaults(ctx, tx, now)
		if err != nil {
			return err
		}
		sample, err = AggregateAllocation(vaults, now)
		if err != nil {
			return err
		}
		amounts, err := json.Marshal(sample.ReserveAmounts)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.earn_fleet_allocations_hourly(cluster,observed_hour,observed_at,reserve_amounts,idle_amount_raw,vaults_total,vaults_included,vaults_missing,vaults_invalid,vaults_stale,excluded_amount_raw,oldest_source_at,newest_source_at,calc_version)
VALUES($1,$2,$3,$4::jsonb,$5::numeric,$6,$7,$8,$9,$10,$11::numeric,$12,$13,$14)
ON CONFLICT(cluster,observed_hour) DO UPDATE SET observed_at=excluded.observed_at,reserve_amounts=excluded.reserve_amounts,idle_amount_raw=excluded.idle_amount_raw,vaults_total=excluded.vaults_total,vaults_included=excluded.vaults_included,vaults_missing=excluded.vaults_missing,vaults_invalid=excluded.vaults_invalid,vaults_stale=excluded.vaults_stale,excluded_amount_raw=excluded.excluded_amount_raw,oldest_source_at=excluded.oldest_source_at,newest_source_at=excluded.newest_source_at,calc_version=excluded.calc_version
WHERE excluded.observed_at >= loyal_yield.earn_fleet_allocations_hourly.observed_at`, m.cluster, sample.ObservedHour, sample.ObservedAt, amounts, sample.IdleAmountRaw, sample.Total, sample.Included, sample.Missing, sample.Invalid, sample.Stale, sample.ExcludedAmountRaw, sample.OldestSourceAt, sample.NewestSourceAt, sample.CalcVersion); err != nil {
			return err
		}
		rows := SnapshotSharePrices(vaults, now)
		if err := persistSharePrices(ctx, tx, m.cluster, rows); err != nil {
			return err
		}
		prices = len(rows)
		return nil
	})
	return sample, prices, err
}

// loadModelRows reads the model evidence as the latest update per reserve per
// hour (buckets anchored at start) plus each reserve's last update before
// start. Every raw update in 30 days is ~1.7M rows on mainnet, far past the
// evidence bound; this is the binning the Apps earnings path adopted for the
// same reason (ASK-2209). The latest row of each hour is kept whether stale
// or not, so a stale reserve is still evicted at that hour.
func (m *Maintenance) loadModelRows(ctx context.Context, start, end time.Time) ([]ModelReserve, error) {
	rows, err := m.timescale.Query(ctx, `
WITH supported AS(SELECT DISTINCT reserve,liquidity_mint FROM kamino.supported_reserves WHERE active=true AND market=ANY($3::text[]) AND liquidity_mint=ANY($4::text[])),
seed AS(SELECT supported.reserve,supported.liquidity_mint,sample.observed_at,sample.slot,sample.supply_apy,sample.total_supply_usd_estimate,sample.reserve_last_update_stale FROM supported CROSS JOIN LATERAL(SELECT observed_at,slot,supply_apy,total_supply_usd_estimate,reserve_last_update_stale FROM kamino.reserve_updates AS source WHERE source.reserve=supported.reserve AND source.liquidity_mint=supported.liquidity_mint AND source.observed_at<$1 ORDER BY source.observed_at DESC,source.slot DESC LIMIT 1) AS sample),
recent AS(SELECT supported.reserve,supported.liquidity_mint,sample.observed_at,sample.slot,sample.supply_apy,sample.total_supply_usd_estimate,sample.reserve_last_update_stale FROM supported CROSS JOIN generate_series($1::timestamptz,$2::timestamptz,interval '1 hour') AS bucket(started_at) CROSS JOIN LATERAL(SELECT observed_at,slot,supply_apy,total_supply_usd_estimate,reserve_last_update_stale FROM kamino.reserve_updates AS source WHERE source.reserve=supported.reserve AND source.liquidity_mint=supported.liquidity_mint AND source.observed_at>=bucket.started_at AND source.observed_at<bucket.started_at+interval '1 hour' AND source.observed_at<=$2 ORDER BY source.observed_at DESC,source.slot DESC LIMIT 1) AS sample)
SELECT * FROM (SELECT * FROM seed UNION ALL SELECT * FROM recent) AS samples ORDER BY observed_at,reserve,slot LIMIT $5`, start, end, m.markets, stableMints, m.maxRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ModelReserve
	for rows.Next() {
		var r ModelReserve
		if err = rows.Scan(&r.Reserve, &r.Mint, &r.ObservedAt, &r.Slot, &r.APY, &r.TotalSupplyUSD, &r.Stale); err != nil {
			return nil, err
		}
		r.Active = true
		result = append(result, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) > m.maxRows {
		return nil, errors.New("public model exceeds bounded evidence window")
	}
	return result, nil
}

// publicModelTimeout bounds the daily public model: the hourly-binned 30-day
// evidence read is one index probe per reserve-hour (~17k on mainnet, ~25 s).
const publicModelTimeout = 3 * time.Minute

func (m *Maintenance) RecordPublicModel(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, publicModelTimeout)
	defer cancel()
	rows, err := m.loadModelRows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil {
		return err
	}
	model, err := SimulatePublicModel(rows, now.Add(-30*24*time.Hour), now)
	if err != nil {
		return err
	}
	// Benchmark points are actual hourly observations, not an invented series.
	benchmark, err := m.timescale.Query(ctx, `WITH previous_sample AS(SELECT observed_at,slot,supply_apy FROM kamino.reserve_updates WHERE reserve=$1 AND observed_at<$2 AND reserve_last_update_stale=false AND supply_apy>=0 AND supply_apy<0.5 ORDER BY observed_at DESC,slot DESC LIMIT 1), latest_sample AS(SELECT observed_at,slot,supply_apy FROM kamino.reserve_updates WHERE reserve=$1 AND observed_at<=$3 AND reserve_last_update_stale=false AND supply_apy>=0 AND supply_apy<0.5 ORDER BY observed_at DESC,slot DESC LIMIT 1), range_candidates AS(SELECT date_bin(interval '1 hour',observed_at,$2::timestamptz) AS sample_bucket,observed_at,slot,supply_apy FROM kamino.reserve_updates WHERE reserve=$1 AND observed_at >=$2 AND observed_at<=$3 AND reserve_last_update_stale=false AND supply_apy>=0 AND supply_apy<0.5), range_samples AS(SELECT DISTINCT ON(sample_bucket) observed_at,slot,supply_apy FROM range_candidates ORDER BY sample_bucket,observed_at DESC,slot DESC) SELECT * FROM(SELECT * FROM previous_sample UNION SELECT * FROM range_samples UNION SELECT * FROM latest_sample) AS samples ORDER BY observed_at,slot LIMIT 723`, benchmarkReserve, model.WindowStartedAt, now)
	if err != nil {
		return err
	}
	var actual []ModelReserve
	for benchmark.Next() {
		var row ModelReserve
		if err = benchmark.Scan(&row.ObservedAt, &row.Slot, &row.APY); err != nil {
			benchmark.Close()
			return err
		}
		actual = append(actual, row)
	}
	err = benchmark.Err()
	benchmark.Close()
	if err != nil {
		return err
	}
	if len(actual) > 722 {
		return errors.New("benchmark source bound exceeded")
	}
	model.Benchmark, err = BenchmarkModel(actual, model.WindowStartedAt, now)
	if err != nil {
		return err
	}

	return persistPublicModel(ctx, m.yield, model)
}

// RefreshHealth uses the same transaction lock, source view and physical-key
// classification as Rust fleet-health-projector, so overlapping legacy/Go
// observers cannot publish interleaved snapshots or advance the fence twice.
func (m *Maintenance) RefreshHealth(ctx context.Context, minimum time.Duration) (bool, int64, error) {
	if minimum <= 0 || minimum > 300*time.Second {
		return false, 0, errors.New("health refresh interval outside source contract")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	published := false
	var fence int64
	err := workersdb.WithTx(ctx, m.yield, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		var acquired bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('fleet-health-projector:' || $1,0))`, m.cluster).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		var fresh bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster=$1 AND refreshed_at >= clock_timestamp()-($2::bigint*interval '1 millisecond'))`, m.cluster, minimum.Milliseconds()).Scan(&fresh); err != nil {
			return err
		}
		if fresh {
			return nil
		}
		var payload, watermark []byte
		started := time.Now().UTC()
		if err := tx.QueryRow(ctx, `
WITH holders AS(SELECT opportunity_id,fee_payer,writable_account_keys FROM loyal_yield.signed_route_submissions WHERE cluster=$1 AND decision_id IS NOT NULL AND submission_state IN('signed','submitted','confirmed') UNION ALL SELECT opportunity_id,fee_payer,writable_account_keys FROM loyal_yield.signed_route_submissions WHERE cluster=$1 AND decision_id IS NOT NULL AND submission_state IN('reconciliation_pending','expiry_check_pending','effect_ambiguous')),
congestion AS(SELECT key AS writable_account_key,min(CASE WHEN key=holder.fee_payer THEN 0 WHEN key=opportunity.target_reserve THEN 1 ELSE 2 END) AS rank,count(*)::bigint AS active_submission_count,sum(opportunity.principal_usd_micros)::bigint AS principal_usd_micros,(sum(opportunity.annual_yield_gain_usd_micros)/8760)::bigint AS recoverable_yield_usd_micros_per_hour FROM holders AS holder JOIN loyal_yield.rebalance_opportunities AS opportunity ON opportunity.id=holder.opportunity_id CROSS JOIN LATERAL unnest(holder.writable_account_keys) AS key GROUP BY key),
hot AS(SELECT writable_account_key,CASE rank WHEN 0 THEN 'payer' WHEN 1 THEN 'target' ELSE 'other' END AS classification,active_submission_count,principal_usd_micros,recoverable_yield_usd_micros_per_hour FROM congestion ORDER BY active_submission_count DESC,recoverable_yield_usd_micros_per_hour DESC,principal_usd_micros DESC,writable_account_key LIMIT 16)
SELECT COALESCE(jsonb_agg(to_jsonb(status)||jsonb_build_object('active_physical_writable_key_count',(SELECT count(*) FROM congestion),'top_physical_writable_key_congestion',COALESCE((SELECT jsonb_agg(to_jsonb(hot)) FROM hot),'[]'::jsonb)) ORDER BY opportunity_state NULLS LAST),'[]'::jsonb)
FROM loyal_yield.fleet_orchestration_status AS status WHERE cluster=$1`, m.cluster).Scan(&payload); err != nil {
			return err
		}
		var err error
		payload, err = normalizeHealthPayload(payload)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT jsonb_build_object('opportunityMaxId',COALESCE((SELECT max(id) FROM loyal_yield.rebalance_opportunities WHERE cluster=$1),0),'submissionMaxId',COALESCE((SELECT max(id) FROM loyal_yield.signed_route_submissions WHERE cluster=$1),0),'outboxMaxId',COALESCE((SELECT max(id) FROM loyal_yield.orchestration_outbox WHERE cluster=$1),0))`, m.cluster).Scan(&watermark); err != nil {
			return err
		}
		var count []json.RawMessage
		if err := json.Unmarshal(payload, &count); err != nil {
			return fmt.Errorf("health payload: %w", err)
		}
		finished := time.Now().UTC()
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.fleet_orchestration_health_snapshots(cluster,payload,source_watermark,refresh_started_at,refreshed_at,refresh_duration_milliseconds,refresh_owner,fencing_token,row_count) VALUES($1,$2::jsonb,$3::jsonb,$4,$5,$6,'postgres-advisory-xact-lock',1,$7)
ON CONFLICT(cluster) DO UPDATE SET payload=excluded.payload,source_watermark=excluded.source_watermark,refresh_started_at=excluded.refresh_started_at,refreshed_at=excluded.refreshed_at,refresh_duration_milliseconds=excluded.refresh_duration_milliseconds,refresh_owner=excluded.refresh_owner,fencing_token=loyal_yield.fleet_orchestration_health_snapshots.fencing_token+1,row_count=excluded.row_count,updated_at=now() RETURNING fencing_token`, m.cluster, payload, watermark, started, finished, finished.Sub(started).Milliseconds(), len(count)).Scan(&fence); err != nil {
			return err
		}
		published = true
		return nil
	})
	return published, fence, err
}

const benchmarkMarket = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
const priceProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

// ReservePriceRPC is the observer's read-only capability, not a sender.
type ReservePriceRPC interface {
	MultipleAccounts(context.Context, []string, string, *uint64) (solanarpc.AccountsResponse, error)
	BlockTime(context.Context, uint64) (*int64, error)
}
type MaintenancePriceRPC struct {
	*solanarpc.Client
	endpoint string
	http     *http.Client
}

func NewMaintenancePriceRPC(endpoint string, timeout time.Duration) (*MaintenancePriceRPC, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || timeout <= 0 || timeout > time.Minute {
		return nil, errors.New("invalid read-only price RPC endpoint/timeout")
	}
	return &MaintenancePriceRPC{Client: solanarpc.New(endpoint, timeout), endpoint: endpoint, http: &http.Client{Timeout: timeout}}, nil
}
func (r *MaintenancePriceRPC) BlockTime(ctx context.Context, slot uint64) (*int64, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getBlockTime", "params": []uint64{slot}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, errors.New("price block-time RPC transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("block-time RPC HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var result struct {
		Result *int64          `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if len(result.Error) > 0 && string(result.Error) != "null" {
		return nil, errors.New("block-time RPC error")
	}
	return result.Result, nil
}
func reservePrice(reserve string, account *solanarpc.Account, contextSlot uint64) (SharePrice, error) {
	var result SharePrice
	disc := sha256.Sum256([]byte("account:Reserve"))
	if account == nil || account.Owner != priceProgram || account.Executable || len(account.Data) != 8624 || !bytes.Equal(account.Data[:8], disc[:8]) {
		return result, errors.New("reserve owner/layout invalid")
	}
	data := account.Data
	update := binary.LittleEndian.Uint64(data[16:24])
	if update == 0 || update > contextSlot || update > math.MaxInt64 || data[24] != 0 {
		return result, errors.New("reserve update slot/staleness invalid")
	}
	market := solana.PublicKeyFromBytes(data[32:64]).String()
	mint := solana.PublicKeyFromBytes(data[128:160]).String()
	if !supportedMint(mint) {
		return result, errors.New("unsupported price mint")
	}
	little := func(raw []byte) *big.Int {
		reversed := make([]byte, len(raw))
		for i, b := range raw {
			reversed[len(raw)-1-i] = b
		}
		return new(big.Int).SetBytes(reversed)
	}
	scale := new(big.Int).Lsh(big.NewInt(1), 60)
	total := new(big.Int).Mul(new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[224:232])), scale)
	total.Add(total, little(data[232:248]))
	for _, offset := range []int{344, 360, 376} {
		total.Sub(total, little(data[offset:offset+16]))
	}
	collateral := binary.LittleEndian.Uint64(data[2592:2600])
	if collateral == 0 || total.Sign() <= 0 {
		return result, errors.New("reserve has no proven exchange supply")
	}
	probe := big.NewInt(1_000_000_000_000)
	redeem := new(big.Int).Mul(probe, total)
	redeem.Quo(redeem, new(big.Int).Mul(new(big.Int).SetUint64(collateral), scale))
	price, _ := new(big.Rat).SetFrac(redeem, probe).Float64()
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return result, errors.New("reserve exchange price invalid")
	}
	return SharePrice{Reserve: reserve, Market: market, Mint: mint, Slot: int64(update), Price: price}, nil
}

// ProbeSharePrices batches at the source's 100-account bound and caches actual
// block times. Missing/stale/unknown reserves are reported, never timestamped now.
func ProbeSharePrices(ctx context.Context, rpc ReservePriceRPC, reserves []string, now time.Time) ([]SharePrice, []string, error) {
	if rpc == nil || now.IsZero() || len(reserves) > 10_000 {
		return nil, nil, errors.New("price probe capability/clock/bound invalid")
	}
	seen := map[string]bool{}
	keys := make([]string, 0, len(reserves))
	for _, key := range reserves {
		if _, err := solana.PublicKeyFromBase58(key); err != nil {
			return nil, nil, errors.New("invalid price candidate identity")
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	prices := []SharePrice{}
	missing := []string{}
	clocks := map[uint64]*int64{}
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		response, err := rpc.MultipleAccounts(ctx, chunk, "confirmed", nil)
		if err != nil {
			return nil, nil, err
		}
		if len(response.Accounts) != len(chunk) {
			return nil, nil, errors.New("price account response cardinality mismatch")
		}
		for i, key := range chunk {
			price, err := reservePrice(key, response.Accounts[i], response.Slot)
			if err != nil {
				missing = append(missing, key)
				continue
			}
			slot := uint64(price.Slot)
			clock, known := clocks[slot]
			if !known {
				clock, err = rpc.BlockTime(ctx, slot)
				if err != nil {
					clock = nil
				}
				clocks[slot] = clock
			}
			if clock == nil || *clock <= 0 {
				missing = append(missing, key)
				continue
			}
			at := time.Unix(*clock, 0).UTC()
			if at.After(now) || now.Sub(at) > 3*time.Hour {
				missing = append(missing, key)
				continue
			}
			price.ObservedAt = at
			price.ObservedHour = at.Truncate(time.Hour)
			prices = append(prices, price)
		}
	}
	return prices, missing, nil
}
func (m *Maintenance) RecordSharePrices(ctx context.Context, now time.Time) (int, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	reserves := map[string]bool{benchmarkReserve: true}
	// The App cron also widens probes with its just-recorded allocation, covering
	// positions whose App projection/candidate catalog has not caught up yet.
	var allocation []byte
	err := m.yield.QueryRow(ctx, `SELECT reserve_amounts FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1 AND observed_at<=$2 ORDER BY observed_at DESC LIMIT 1`, m.cluster, now).Scan(&allocation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, err
	}
	if err == nil {
		var amounts map[string]string
		if err = json.Unmarshal(allocation, &amounts); err != nil {
			return 0, nil, err
		}
		if len(amounts) > 10000 {
			return 0, nil, errors.New("allocation price candidates exceed bound")
		}
		for key := range amounts {
			reserves[key] = true
		}
	}
	// Compatibility with current App AUM candidate widening; no float weights are
	// needed merely to identify reserves with positive raw principal.
	held, err := m.yield.Query(ctx, `SELECT DISTINCT current_reserve FROM loyal_yield.user_yield_positions WHERE status='active' AND vault_index=1 AND current_amount_raw>0 ORDER BY current_reserve LIMIT 10001`)
	if err != nil {
		return 0, nil, err
	}
	for held.Next() {
		var key string
		if err = held.Scan(&key); err != nil {
			held.Close()
			return 0, nil, err
		}
		reserves[key] = true
	}
	err = held.Err()
	held.Close()
	if err != nil {
		return 0, nil, err
	}
	// A candidate catalog outage is not silently treated as an empty universe.
	// Returning it leaves legacy cron ownership and the pass retryable.
	candidates, err := m.timescale.Query(ctx, `SELECT DISTINCT reserve FROM kamino.supported_reserves WHERE active=true AND market=ANY($1::text[]) AND liquidity_mint=ANY($2::text[]) ORDER BY reserve LIMIT 10001`, m.markets, stableMints)
	if err != nil {
		return 0, nil, err
	}
	for candidates.Next() {
		var key string
		if err = candidates.Scan(&key); err != nil {
			candidates.Close()
			return 0, nil, err
		}
		reserves[key] = true
	}
	err = candidates.Err()
	candidates.Close()
	if err != nil {
		return 0, nil, err
	}
	if len(reserves) > 10_000 {
		return 0, nil, errors.New("price universe exceeds bound")
	}
	keys := make([]string, 0, len(reserves))
	for key := range reserves {
		keys = append(keys, key)
	}
	rows, missing, err := ProbeSharePrices(ctx, m.rpc, keys, now)
	if err != nil {
		return 0, missing, err
	}
	if err = workersdb.WithTx(ctx, m.yield, pgx.TxOptions{}, func(tx pgx.Tx) error { return persistSharePrices(ctx, tx, m.cluster, rows) }); err != nil {
		return 0, missing, err
	}
	return len(rows), missing, nil
}

func persistSharePrices(ctx context.Context, tx pgx.Tx, cluster string, rows []SharePrice) error {
	for _, p := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.earn_reserve_share_prices(cluster,reserve,market,liquidity_mint,observed_hour,observed_at,slot,share_price) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(cluster,reserve,observed_hour) DO UPDATE SET observed_at=excluded.observed_at,slot=excluded.slot,share_price=excluded.share_price WHERE excluded.slot>loyal_yield.earn_reserve_share_prices.slot OR (excluded.slot=loyal_yield.earn_reserve_share_prices.slot AND excluded.observed_at>=loyal_yield.earn_reserve_share_prices.observed_at)`, cluster, p.Reserve, p.Market, p.Mint, p.ObservedHour, p.ObservedAt, p.Slot, p.Price); err != nil {
			return err
		}
	}
	return nil
}

func marshalPublicModel(model PublicModel) ([]byte, []byte, error) {
	samples, err := json.Marshal(model.Samples)
	if err != nil {
		return nil, nil, err
	}
	series, err := json.Marshal([]map[string]any{{"key": "loyal", "label": "Loyal Earn", "metadata": map[string]string{"metric": "cumulative_annualized_apy_bps"}, "samples": model.Samples}, {"key": "mainUsdcReserve", "label": "Kamino Main USDC", "metadata": map[string]string{"metric": "cumulative_annualized_apy_bps", "market": benchmarkMarket, "reserve": benchmarkReserve, "liquidityMint": USDCMint}, "samples": model.Benchmark}})
	if err != nil {
		return nil, nil, err
	}
	return samples, series, nil
}

func persistPublicModel(ctx context.Context, pool *pgxpool.Pool, model PublicModel) error {
	samples, series, err := marshalPublicModel(model)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.earn_forecast_snapshots(strategy,risk_profile,fee_bps,snapshot_date,window_started_at,window_ended_at,generated_at,apy_bps,range_low_bps,range_high_bps,samples,series)
VALUES('medium_fee_aware_1bps','medium',1,$1,$2,$3,$3,$4,$5,$6,$7::jsonb,$8::jsonb)
ON CONFLICT(strategy,risk_profile,fee_bps,snapshot_date) DO UPDATE SET window_started_at=excluded.window_started_at,window_ended_at=excluded.window_ended_at,generated_at=excluded.generated_at,apy_bps=excluded.apy_bps,range_low_bps=excluded.range_low_bps,range_high_bps=excluded.range_high_bps,samples=excluded.samples,series=excluded.series
WHERE excluded.generated_at >= loyal_yield.earn_forecast_snapshots.generated_at`, model.GeneratedAt.UTC().Format("2006-01-02"), model.WindowStartedAt, model.WindowEndedAt, model.APYBPS, model.LowBPS, model.HighBPS, samples, series)
	return err
}
