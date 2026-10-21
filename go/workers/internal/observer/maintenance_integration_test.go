package observer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func maintenanceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("READMODELS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("READMODELS_TEST_DATABASE_URL requires the disposable fixture")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		t.Fatal("invalid disposable fixture URL")
	}
	password, hasPassword := parsed.User.Password()
	_ = password
	if (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/workers_v2_observer" || parsed.User.Username() != "workers_v2" || hasPassword || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("read-model tests refuse any non-loopback workers_v2 disposable fixture before connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}
func TestHealthPublicationUsesLegacyLockAndActualStatusPayload(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cluster := "readmodel-lock-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	m := &Maintenance{yield: pool, cluster: cluster, timeout: 10 * time.Second}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster=$1`, cluster)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.fleet_planning_clusters WHERE cluster=$1`, cluster)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.fleet_planning_clusters(cluster,registered_at,last_seen_at) VALUES($1,now()-interval '1 minute',now())`, cluster); err != nil {
		t.Fatal(err)
	}
	legacy, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Rollback(ctx)
	if _, err = legacy.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('fleet-health-projector:' || $1,0))`, cluster); err != nil {
		t.Fatal(err)
	}
	published, fence, err := m.RefreshHealth(ctx, time.Minute)
	if err != nil || published || fence != 0 {
		t.Fatalf("Go crossed legacy lock: %v %d %v", published, fence, err)
	}
	if err = legacy.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	published, fence, err = m.RefreshHealth(ctx, time.Minute)
	if err != nil || !published || fence != 1 {
		t.Fatalf("first publication: %v %d %v", published, fence, err)
	}
	var payload []byte
	var watermark map[string]int64
	var raw []byte
	var owner string
	var count int64
	if err = pool.QueryRow(ctx, `SELECT payload,source_watermark,refresh_owner,row_count FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster=$1`, cluster).Scan(&payload, &raw, &owner, &count); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &watermark); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err = json.Unmarshal(payload, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || count != 1 || len(rows[0]) != 60 || owner != "postgres-advisory-xact-lock" || len(watermark) != 3 {
		t.Fatalf("actual SQL/Rust shape differs: %s %s %s %d", payload, raw, owner, count)
	}
	report, err := StageHealth(payload, 5*time.Second, 5*time.Second, time.Now().UTC())
	if err != nil || len(report.StuckStages) != 1 || report.StuckStages[0].Stage != "market_epoch" {
		t.Fatalf("missing market stage not derived: %+v %v", report, err)
	}
	published, _, err = m.RefreshHealth(ctx, time.Minute)
	if err != nil || published {
		t.Fatalf("fresh snapshot advanced: %v %v", published, err)
	}
	if err = pool.QueryRow(ctx, `SELECT fencing_token FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster=$1`, cluster).Scan(&fence); err != nil || fence != 1 {
		t.Fatalf("fresh fence %d %v", fence, err)
	}
}
func TestSharePriceOlderOverlappingObserverCannotOverwriteState(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cluster := "readmodel-price-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reserve_share_prices WHERE cluster=$1`, cluster)
	})
	at := time.Now().UTC().Truncate(time.Second)
	p := SharePrice{Reserve: benchmarkReserve, Market: fleet.KaminoMainMarket, Mint: USDCMint, ObservedAt: at, ObservedHour: at.Truncate(time.Hour), Slot: 100, Price: 1.1}
	write := func(p SharePrice) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = persistSharePrices(ctx, tx, cluster, []SharePrice{p}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	write(p)
	stale := p
	stale.Slot = 99
	stale.ObservedAt = at.Add(time.Second)
	stale.Price = 9
	write(stale)
	stale.Slot = 100
	stale.ObservedAt = at.Add(-time.Second)
	write(stale)
	var price float64
	var slot int64
	var actual time.Time
	if err := pool.QueryRow(ctx, `SELECT share_price,slot,observed_at FROM loyal_yield.earn_reserve_share_prices WHERE cluster=$1 AND reserve=$2 AND observed_hour=$3`, cluster, benchmarkReserve, p.ObservedHour).Scan(&price, &slot, &actual); err != nil {
		t.Fatal(err)
	}
	if price != 1.1 || slot != 100 || !actual.Equal(at) {
		t.Fatalf("stale publication won: %v %d %v", price, slot, actual)
	}
}

// telemetryChain serves accounts by address at one slot, as getMultipleAccounts
// would.
type telemetryChain struct {
	slot     uint64
	accounts map[string]chain.Account
}

func (telemetryChain) BlockTime(context.Context, uint64) (time.Time, error) {
	return time.Time{}, errors.New("telemetry reads no block time")
}

func (c telemetryChain) Accounts(_ context.Context, keys []solana.PublicKey, _ rpc.CommitmentType, _ uint64) (uint64, []*chain.Account, error) {
	out := make([]*chain.Account, len(keys))
	for i, key := range keys {
		if account, ok := c.accounts[key.String()]; ok {
			out[i] = &account
		}
	}
	return c.slot, out, nil
}

type telemetryCatalog []fleet.ReserveIdentity

func (c telemetryCatalog) LoadCatalog(context.Context) ([]fleet.ReserveIdentity, error) {
	return c, nil
}

func telemetryKey(label string) solana.PublicKey {
	sum := sha256.Sum256([]byte(label))
	return solana.PublicKeyFromBytes(sum[:])
}

// The hourly allocation is what the chain holds at the hour: each vault's
// obligation collateral, what it redeems at the reserve's exchange rate, and
// its idle Earn cash. A vault the chain cannot name is missing, not empty.
func TestTelemetryReadsHoldingsFromTheChainAndForwardAllocationClock(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	cluster := "readmodel-allocation-" + suffix
	now := time.Now().UTC().Truncate(time.Second)
	vaultKey, reserveKey, market := telemetryKey("vault"+suffix), telemetryKey("reserve"+suffix), solana.MustPublicKeyFromBase58(fleet.KaminoMainMarket)
	reserve := reserveKey.String()
	usdc := solana.MustPublicKeyFromBase58(USDCMint)
	// 1,000 collateral over 1,200 liquidity: one unit redeems 1.2.
	reserveData := make([]byte, kamino.ReserveSize)
	copy(reserveData, kamino.ReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(reserveData[8:16], 1)
	copy(reserveData[32:64], market[:])
	copy(reserveData[128:160], usdc[:])
	binary.LittleEndian.PutUint64(reserveData[224:232], 1_200_000_000_000)
	binary.LittleEndian.PutUint64(reserveData[2592:2600], 1_000_000_000_000)
	obligation, err := kamino.VanillaObligation(vaultKey, market)
	if err != nil {
		t.Fatal(err)
	}
	obligationData := make([]byte, kamino.ObligationSize)
	copy(obligationData, kamino.ObligationDiscriminator[:])
	copy(obligationData[32:64], market[:])
	copy(obligationData[64:96], vaultKey[:])
	copy(obligationData[96:128], reserveKey[:])
	binary.LittleEndian.PutUint64(obligationData[128:136], 1_000_000_000)
	idle, err := spl.AssociatedTokenAddress(vaultKey, usdc, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	idleData := make([]byte, 165)
	copy(idleData[0:32], usdc[:])
	copy(idleData[32:64], vaultKey[:])
	binary.LittleEndian.PutUint64(idleData[64:72], 7_000_000)
	idleData[108] = 1
	node := telemetryChain{slot: 4_000, accounts: map[string]chain.Account{
		reserve:             {Key: reserveKey, Owner: kamino.ProgramID, Lamports: 1, Data: reserveData},
		obligation.String(): {Key: obligation, Owner: kamino.ProgramID, Lamports: 1, Data: obligationData},
		idle.String():       {Key: idle, Owner: solana.TokenProgramID, Lamports: 1, Data: idleData},
	}}
	m := &Maintenance{yield: pool, cluster: cluster, timeout: 10 * time.Second, maxVaults: 10000, rpc: node, catalog: telemetryCatalog{{Address: reserve, Market: fleet.KaminoMainMarket, Mint: USDCMint}}}
	var policy int64
	var vaults []int64
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reserve_share_prices WHERE cluster=$1`, cluster)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1`, cluster)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.managed_vaults WHERE id=ANY($1)`, vaults)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.route_policies WHERE id=$1`, policy)
	})
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$2,1,$3,1,100,$1) RETURNING id`, "readmodel-"+suffix, "readmodel-policy-"+suffix, vaultKey.String()).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	for i, pubkey := range []string{vaultKey.String(), "readmodel-unnamed-" + suffix} {
		var vault int64
		if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,first_seen_at) VALUES($1,1,$2,$3,$4) RETURNING id`, fmt.Sprintf("readmodel-%s-%d", suffix, i), pubkey, policy, now.Add(-time.Hour)).Scan(&vault); err != nil {
			t.Fatal(err)
		}
		vaults = append(vaults, vault)
	}
	sample, prices, err := m.RecordTelemetry(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if sample.ReserveAmounts[reserve] != "1200000000" || sample.IdleAmountRaw != "7000000" || sample.Included < 1 || sample.Missing < 1 || prices != 1 {
		t.Fatalf("chain holdings not aggregated: %+v prices=%d", sample, prices)
	}
	var actual, priceAt time.Time
	var amount string
	var price float64
	var slot int64
	if err = pool.QueryRow(ctx, `SELECT observed_at,reserve_amounts->>$3 FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1 AND observed_hour=$2`, cluster, now.Truncate(time.Hour), reserve).Scan(&actual, &amount); err != nil {
		t.Fatal(err)
	}
	if !actual.Equal(now) || amount != "1200000000" {
		t.Fatalf("allocation forward clock lost: %v %s", actual, amount)
	}
	if err = pool.QueryRow(ctx, `SELECT observed_at,share_price,slot FROM loyal_yield.earn_reserve_share_prices WHERE cluster=$1 AND reserve=$2`, cluster, reserve).Scan(&priceAt, &price, &slot); err != nil || !priceAt.Equal(now) || price != 1.2 || slot != 4_000 {
		t.Fatalf("share price is not the chain's at the read: %v %v %d %v", priceAt, price, slot, err)
	}
	if _, _, err = m.RecordTelemetry(ctx, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT observed_at FROM loyal_yield.earn_fleet_allocations_hourly WHERE cluster=$1 AND observed_hour=$2`, cluster, now.Truncate(time.Hour)).Scan(&actual); err != nil || !actual.Equal(now) {
		t.Fatalf("older overlapping allocation overwrote: %v %v", actual, err)
	}
	// maxVaults=0 is a deliberate under-bound read, not publication permission.
	m.maxVaults = 0
	if _, err = m.loadVaults(ctx, now); err == nil || !strings.Contains(fmt.Sprint(err), "refusing partial allocation") {
		t.Fatalf("bounded partial fleet admitted: %v", err)
	}
}

func TestPublicForecastNewerModelSurvivesDelayedPublication(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A distinct future fixture date avoids touching another suite's current-day
	// modeled snapshot. No historical production sample is backfilled.
	end := time.Date(2091, 3, 7, 12, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_forecast_snapshots WHERE strategy='medium_fee_aware_1bps' AND risk_profile='medium' AND fee_bps=1 AND snapshot_date=$1`, end.Format("2006-01-02"))
	})
	build := func(rate float64, at time.Time) PublicModel {
		t.Helper()
		model, err := SimulatePublicModel([]ModelReserve{{Reserve: benchmarkReserve, Mint: USDCMint, ObservedAt: start, APY: rate, TotalSupplyUSD: 200001, Active: true}}, start, at)
		if err != nil {
			t.Fatal(err)
		}
		model.Benchmark, err = BenchmarkModel([]ModelReserve{{ObservedAt: start, APY: .05}}, start, at)
		if err != nil {
			t.Fatal(err)
		}
		return model
	}
	newer := build(.1, end)
	older := build(.4, end.Add(-time.Minute))
	if err := persistPublicModel(ctx, pool, newer); err != nil {
		t.Fatal(err)
	}
	if err := persistPublicModel(ctx, pool, older); err != nil {
		t.Fatal(err)
	}
	var generated time.Time
	var apy int32
	var samples, series []byte
	if err := pool.QueryRow(ctx, `SELECT generated_at,apy_bps,samples,series FROM loyal_yield.earn_forecast_snapshots WHERE strategy='medium_fee_aware_1bps' AND risk_profile='medium' AND fee_bps=1 AND snapshot_date=$1`, end.Format("2006-01-02")).Scan(&generated, &apy, &samples, &series); err != nil {
		t.Fatal(err)
	}
	if !generated.Equal(end) || apy != 1000 || !strings.Contains(string(series), "cumulative_annualized_apy_bps") || !strings.Contains(string(series), "Kamino Main USDC") || !strings.Contains(string(samples), "2091-03-07T12:00:00Z") {
		t.Fatalf("older forecast changed product: %v %d %s %s", generated, apy, samples, series)
	}
}

func TestTimescaleModelQueriesPreserveSourceSlotAndCandidateUniverse(t *testing.T) {
	// This additional gate requires the real source migration fixture, including
	// TimescaleDB. It never substitutes hand-written DDL or an ordinary schema.
	dsn := os.Getenv("TEST_TIMESCALE_DATABASE_URL")
	if dsn == "" {
		t.Skip("actual TEST_TIMESCALE_DATABASE_URL fixture is required for model/candidate SQL proof")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		t.Fatal("invalid Timescale fixture URL")
	}
	_, password := parsed.User.Password()
	if (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/workers_v2_timescale" || parsed.User.Username() != "workers_v2" || password || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("refuse Timescale connection outside disposable fixture")
	}
	yield := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if err = ts.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	var extension bool
	if err = ts.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&extension); err != nil || !extension {
		t.Fatalf("source Timescale extension missing: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	market := "readmodel-market-" + suffix
	reserve := "readmodel-model-" + suffix
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	m := &Maintenance{yield: yield, timescale: ts, cluster: "readmodel-model-" + suffix, markets: []string{market}, maxRows: 10, timeout: 10 * time.Second}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = ts.Exec(cleanup, `DELETE FROM kamino.reserve_updates WHERE reserve=$1`, reserve)
		_, _ = ts.Exec(cleanup, `DELETE FROM kamino.supported_reserves WHERE market=$1`, market)
	})
	if _, err = ts.Exec(ctx, `INSERT INTO kamino.supported_reserves(market,liquidity_mint,reserve,active) VALUES($1,$2,$3,true)`, market, USDCMint, reserve); err != nil {
		t.Fatal(err)
	}
	// Modeled-input fixtures exercise table consumers, not chain verification.
	// Raw/USD values carry the same six-decimal mint and unit-consistent supply.
	insert := func(at time.Time, slot int64, stale bool) {
		t.Helper()
		_, err := ts.Exec(ctx, `INSERT INTO kamino.reserve_updates(observed_at,slot,kind,source,reserve,market,liquidity_mint,mint_decimals,reserve_last_update_slot,reserve_last_update_stale,reserve_price_status,available_amount,borrowed_amount,borrowed_amount_sf,total_supply_amount,market_price_usd,market_price_last_updated_ts,cumulative_borrow_rate_bsf,total_supply_usd_estimate,total_borrow_usd_estimate,utilization,borrow_apr,supply_apr,borrow_apy,supply_apy,protocol_take_rate_pct,host_fixed_interest_rate_bps,diff_changed,diff_summary,diff,target,snapshot,record) VALUES($1,$2,'reserve_update','modeled_consumer_fixture',$3,$4,$5,6,$2,$6,0,200001000000,0,'0',200001000000,1,0,'0:0:0:0',200001,0,0,0,0,0,0.1,0,0,false,'modeled fixture','{}','{}','{}','{}')`, at, slot, reserve, market, USDCMint, stale)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(start.Add(-2*time.Minute), 99, false)
	insert(start.Add(-time.Minute), 100, false)
	insert(start.Add(10*time.Minute), 101, false)
	insert(start.Add(30*time.Minute), 102, true)
	// One seed row before start, then the latest row of the hour: 101 is
	// binned away and the stale 102 is kept so it still evicts the reserve.
	rows, err := m.loadModelRows(ctx, start, now)
	if err != nil || len(rows) != 2 || rows[0].Slot != 100 || rows[1].Slot != 102 {
		t.Fatalf("source sequence query: %+v %v", rows, err)
	}
	model, err := SimulatePublicModel(rows, start, now)
	if err != nil {
		t.Fatal(err)
	}
	if model.APYBPS >= 1000 || model.APYBPS <= 0 {
		t.Fatalf("latest source stale row did not evict: %+v", model)
	}
	m.maxRows = 1
	if _, err = m.loadModelRows(ctx, start, now); err == nil {
		t.Fatal("truncated model evidence admitted")
	}
}
