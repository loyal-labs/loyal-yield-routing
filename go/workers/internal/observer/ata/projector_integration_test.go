package ata

import (
	"context"
	"encoding/binary"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

func projectorFixturePool(t *testing.T, variable, database string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv(variable)
	if dsn == "" {
		t.Skip(variable + " is absent; requires root-registered isolated ATA fixture")
	}
	u, err := url.Parse(dsn)
	port, portErr := strconv.Atoi(uPort(u))
	if err != nil || u == nil || u.Scheme != "postgresql" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") ||
		portErr != nil || port < 1024 || port > 65535 || u.User == nil || u.User.Username() != "workers_v2" || u.Path != "/"+database || u.Fragment != "" {
		t.Fatal("ATA SQL tests require exact registered loopback role/database before connection")
	}
	if _, exists := u.User.Password(); exists {
		t.Fatal("ATA fixture URL must not carry a password")
	}
	for key, values := range u.Query() {
		if key != "sslmode" || len(values) != 1 || values[0] != "disable" {
			t.Fatal("ATA fixture URL contains an unapproved connection option")
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != u.Hostname() || config.ConnConfig.User != "workers_v2" || config.ConnConfig.Database != database || config.ConnConfig.Password != "" {
		t.Fatal("ATA fixture connection configuration changed registered identity")
	}
	config.MaxConns = 5
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var actualDB, role string
	if err := pool.QueryRow(ctx, `SELECT current_database(),current_user`).Scan(&actualDB, &role); err != nil || actualDB != database || role != "workers_v2" {
		t.Fatalf("configured ATA fixture identity/read failed: db=%s role=%s err=%v", actualDB, role, err)
	}
	return pool, ctx
}

func uPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Port()
}

func projectorFixture(t *testing.T) (*Projector, context.Context) {
	t.Helper()
	capture, ctx := projectorFixturePool(t, "ATA_CAPTURE_TEST_DATABASE_URL", "workers_v2_ata_capture")
	yield, _ := projectorFixturePool(t, "ATA_PROJECTOR_TEST_DATABASE_URL", "workers_v2_ata_projector")
	p, err := NewProjector(capture, yield, ProjectorConfig{Stream: "production", Cluster: "mainnet-beta", BatchLimit: 100, PollInterval: time.Millisecond, IOTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p, ctx
}

func seedProjectionTarget(t *testing.T, ctx context.Context, p *Projector) watch.ATATarget {
	t.Helper()
	key := func() string { return solana.NewWallet().PublicKey().String() }
	target := watch.ATATarget{Cluster: "mainnet-beta", Wallet: key(), WalletATA: key(), Vault: key(), VaultATA: key(), Mint: usdcMint}
	if err := p.yield.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets
(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,wallet,wallet_usdc_ata,vault_usdc_ata,
wallet_token_ata,vault_token_ata,token_mint,threshold,max_amount_per_period,desired_active,chain_status,last_seen_slot,last_seen_signature,cluster)
VALUES($1,$2,1,$3,1,$4,$2,$5,$6,$5,$6,$7,1,1000000,true,'active',1,'ata-projection-fixture',$8) RETURNING id`,
		key(), target.Wallet, key(), target.Vault, target.WalletATA, target.VaultATA, target.Mint, target.Cluster).Scan(&target.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Scope cleanup to this target; no fixture-wide reset or DDL.
		if _, err := p.capture.Exec(cleanup, `DELETE FROM loyal_prod.balance_sweep_wallet_ata_observation_dedupe WHERE wallet_usdc_ata=$1`, target.WalletATA); err != nil {
			t.Error(err)
		}
		if _, err := p.capture.Exec(cleanup, `DELETE FROM loyal_prod.balance_sweep_wallet_ata_observations WHERE wallet_usdc_ata=$1`, target.WalletATA); err != nil {
			t.Error(err)
		}
		if _, err := p.yield.Exec(cleanup, `DELETE FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.ID); err != nil {
			t.Error(err)
		}
	})
	return target
}

func captureProjectionObservation(t *testing.T, ctx context.Context, p *Projector, target watch.ATATarget, amount, slot uint64) int64 {
	t.Helper()
	data := make([]byte, 165)
	mint := solana.MustPublicKeyFromBase58(target.Mint)
	owner := solana.MustPublicKeyFromBase58(target.Wallet)
	copy(data[:32], mint[:])
	copy(data[32:64], owner[:])
	binary.LittleEndian.PutUint64(data[64:72], amount)
	programOwner := solana.TokenProgramID.String()
	observed := observation{target: target, pubkey: target.WalletATA, lamports: 2039280, amount: amount,
		owner: &programOwner, mint: target.Mint, slot: slot, source: laserStreamSource, data: data, received: time.Now().UTC()}
	out, err := NewHandler(p.capture, nil).persist(ctx, observed)
	if err != nil || !out.Inserted {
		t.Fatalf("actual capture failed: %+v %v", out, err)
	}
	return out.EventID
}

func assertProjectedDelta(t *testing.T, ctx context.Context, p *Projector, id int64, previous, delta *int64) {
	t.Helper()
	var actualPrevious, actualDelta *int64
	if err := p.yield.QueryRow(ctx, `SELECT previous_amount_raw,delta_amount_raw FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, id).Scan(&actualPrevious, &actualDelta); err != nil {
		t.Fatal(err)
	}
	if (previous == nil) != (actualPrevious == nil) || (delta == nil) != (actualDelta == nil) || previous != nil && *previous != *actualPrevious || delta != nil && *delta != *actualDelta {
		t.Fatalf("financial delta differs: previous=%v delta=%v wanted=%v/%v", actualPrevious, actualDelta, previous, delta)
	}
}

func TestProjectorCapturedPatchesPreserveUnknownAndMonotonicBalances(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	b := seedProjectionTarget(t, ctx, p)
	first := captureProjectionObservation(t, ctx, p, a, 100, 100)
	bFirst := captureProjectionObservation(t, ctx, p, b, 44, 100)
	if _, err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectedDelta(t, ctx, p, first, nil, nil)
	assertProjectedDelta(t, ctx, p, bFirst, nil, nil)
	newer := captureProjectionObservation(t, ctx, p, a, 150, 102)
	older := captureProjectionObservation(t, ctx, p, a, 20, 101)
	equal := captureProjectionObservation(t, ctx, p, a, 170, 102)
	last := captureProjectionObservation(t, ctx, p, a, 190, 103)
	if _, err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	prev, delta := int64(100), int64(50)
	assertProjectedDelta(t, ctx, p, newer, &prev, &delta)
	prev, delta = 150, -130 // Retained source records the event even when current must not regress.
	assertProjectedDelta(t, ctx, p, older, &prev, &delta)
	prev, delta = 150, 20
	assertProjectedDelta(t, ctx, p, equal, &prev, &delta)
	prev, delta = 170, 20
	assertProjectedDelta(t, ctx, p, last, &prev, &delta)
	var amount, slot int64
	if err := p.yield.QueryRow(ctx, `SELECT amount_raw,observed_slot FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2`, a.ID, a.Mint).Scan(&amount, &slot); err != nil || amount != 190 || slot != 103 {
		t.Fatalf("current regressed: %d/%d %v", amount, slot, err)
	}
	if err := p.yield.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2`, b.ID, b.Mint).Scan(&amount); err != nil || amount != 44 {
		t.Fatalf("partial patch reset omitted account: %d %v", amount, err)
	}
	if out, err := p.Tick(ctx); err != nil || out.InsertedEvents != 0 || out.LastEventID != last {
		t.Fatalf("empty capture invented balance events or lost checkpoint: %+v %v", out, err)
	}
}

func TestProjectorRollbackRetryAndStableIDConflict(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	b := seedProjectionTarget(t, ctx, p)
	before, err := p.readOffset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := captureProjectionObservation(t, ctx, p, a, 100, 200)
	bad := captureProjectionObservation(t, ctx, p, b, 10, 200)
	// The malformed source row models a destination-missing target after a
	// cross-DB capture commit. Destination must roll back the preceding good row.
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET target_id=9223372036854775807 WHERE event_id=$1`, bad); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = p.capture.Exec(context.Background(), `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET target_id=$2 WHERE event_id=$1`, bad, b.ID)
	}()
	if _, err := p.Tick(ctx); err == nil {
		t.Fatal("failed second event committed a partial batch")
	}
	var count int
	if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("batch failure retained first event: %d %v", count, err)
	}
	if offset, err := p.readOffset(ctx); err != nil || offset != before {
		t.Fatalf("failed batch advanced checkpoint: %d %v", offset, err)
	}
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET target_id=$2 WHERE event_id=$1`, bad, b.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := p.Tick(ctx); err != nil || out.InsertedEvents != 2 || out.LastEventID != bad {
		t.Fatalf("retry did not reuse exact captured IDs: %+v %v", out, err)
	}
	assertProjectedDelta(t, ctx, p, id, nil, nil)
	// Replay an exact already committed batch (lost response/repair). Offset is
	// deliberately reset in this isolated fixture; current and delta must survive.
	if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.projection_offsets SET last_event_id=$2 WHERE consumer_name=$1`, p.consumer, before); err != nil {
		t.Fatal(err)
	}
	if out, err := p.Tick(ctx); err != nil || out.InsertedEvents != 0 || out.LastEventID != bad {
		t.Fatalf("exact replay changed delta or checkpoint: %+v %v", out, err)
	}
	assertProjectedDelta(t, ctx, p, id, nil, nil)
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET amount_raw=999 WHERE event_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.projection_offsets SET last_event_id=$2 WHERE consumer_name=$1`, p.consumer, before); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tick(ctx); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("stable source ID collision accepted: %v", err)
	}
	if offset, err := p.readOffset(ctx); err != nil || offset != before {
		t.Fatalf("collision advanced checkpoint: %d %v", offset, err)
	}
	var amount int64
	if err := p.yield.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2`, a.ID, a.Mint).Scan(&amount); err != nil || amount != 100 {
		t.Fatalf("collision overwrote balance: %d %v", amount, err)
	}
	// Restore only this owned offset for later tests; the conflicting rows are
	// removed by target-scoped cleanup, never skipped by the production projector.
	if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.projection_offsets SET last_event_id=$2 WHERE consumer_name=$1`, p.consumer, bad); err != nil {
		t.Fatal(err)
	}
}

func TestProjectorRetainedFenceContentionAndCancellation(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	// Install the exact retained consumer row, then lock it using the Rust SQL.
	if _, err := p.yield.Exec(ctx, `INSERT INTO loyal_yield.projection_offsets(consumer_name,last_event_id) VALUES($1,0) ON CONFLICT DO NOTHING`, p.consumer); err != nil {
		t.Fatal(err)
	}
	id := captureProjectionObservation(t, ctx, p, a, 100, 300)
	tx, err := p.yield.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var offset int64
	if err := tx.QueryRow(ctx, `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1 FOR UPDATE`, p.consumer).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = p.Tick(blocked)
	cancel()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked retained projector did not cancel: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := p.Tick(ctx); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent projection duplicated event: %d %v", count, err)
	}
	assertProjectedDelta(t, ctx, p, id, nil, nil)
	// No work remains; cancellation still joins the owned polling loop promptly.
	runCtx, stop := context.WithCancel(ctx)
	stop()
	if err := p.Run(runCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled projector runtime did not stop: %v", err)
	}
}

func TestProjectorRunKeepsCaptureAliveAcrossDestinationOutage(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	id := captureProjectionObservation(t, ctx, p, a, 100, 500)
	if _, err := p.yield.Exec(ctx, `INSERT INTO loyal_yield.projection_offsets(consumer_name,last_event_id) VALUES($1,0) ON CONFLICT DO NOTHING`, p.consumer); err != nil {
		t.Fatal(err)
	}
	tx, err := p.yield.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var before int64
	if err := tx.QueryRow(ctx, `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1 FOR UPDATE`, p.consumer).Scan(&before); err != nil {
		t.Fatal(err)
	}
	health := make(chan bool, 16)
	errorsSeen := make(chan error, 16)
	p.ioTimeout = 50 * time.Millisecond
	p.onHealth = func(value bool) {
		select {
		case health <- value:
		default:
		}
	}
	p.onError = func(err error) {
		select {
		case errorsSeen <- err:
		default:
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	completed := make(chan error, 1)
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case <-completed:
			case <-time.After(time.Second):
				t.Error("projector runtime failed to join after cancellation")
			}
		}
	}()
	go func() { completed <- p.Run(runCtx) }()
	select {
	case ok := <-health:
		if ok {
			t.Fatal("destination outage was reported healthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("destination outage did not lower projection health")
	}
	select {
	case err := <-errorsSeen:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unexpected projection failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("projection failure not reported")
	}
	select {
	case err := <-completed:
		joined = true
		t.Fatalf("ordinary projection failure terminated runtime: %v", err)
	default:
	}
	// Capture commits independently while the destination is unavailable.
	second := captureProjectionObservation(t, ctx, p, a, 150, 501)
	if offset, err := p.readOffset(ctx); err != nil || offset != before {
		t.Fatalf("outage advanced destination checkpoint: %d %v", offset, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-health:
		if !ok {
			t.Fatal("restored destination did not become healthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("projection did not recover after destination outage")
	}
	assertProjectedDelta(t, ctx, p, id, nil, nil)
	previous, delta := int64(100), int64(50)
	assertProjectedDelta(t, ctx, p, second, &previous, &delta)
	if offset, err := p.readOffset(ctx); err != nil || offset != second {
		t.Fatalf("recovered projection lost exact IDs: %d %v", offset, err)
	}
	stop()
	select {
	case err := <-completed:
		joined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("projection runtime cancellation failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("projection runtime did not join promptly")
	}
}

func TestProjectorCanonicalNamespaceSkipsForeignAndHoldsUnknown(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	foreign := captureProjectionObservation(t, ctx, p, a, 999, 600)
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET cluster='devnet',target_id=9223372036854775807 WHERE event_id=$1`, foreign); err != nil {
		t.Fatal(err)
	}
	known := captureProjectionObservation(t, ctx, p, a, 100, 601)
	out, err := p.Tick(ctx)
	if err != nil || out.LastEventID != known || out.FetchedEvents != 2 || out.InsertedEvents != 1 {
		t.Fatalf("foreign namespace did not advance without financial writes: %+v %v", out, err)
	}
	var count int
	if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, foreign).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign-cluster event contaminated Yield: count=%d err=%v", count, err)
	}
	assertProjectedDelta(t, ctx, p, known, nil, nil)
	unknown := captureProjectionObservation(t, ctx, p, a, 200, 602)
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET cluster='unknown' WHERE event_id=$1`, unknown); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tick(ctx); err == nil || !strings.Contains(err.Error(), "unknown captured cluster") {
		t.Fatalf("unknown source namespace accepted: %v", err)
	}
	if offset, err := p.readOffset(ctx); err != nil || offset != known {
		t.Fatalf("unknown namespace advanced checkpoint: %d %v", offset, err)
	}
	if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET cluster='mainnet-beta' WHERE event_id=$1`, unknown); err != nil {
		t.Fatal(err)
	}
	if out, err := p.Tick(ctx); err != nil || out.LastEventID != unknown || out.InsertedEvents != 1 {
		t.Fatalf("authoritative repaired namespace did not retry same source ID: %+v %v", out, err)
	}
	prev, delta := int64(100), int64(100)
	assertProjectedDelta(t, ctx, p, unknown, &prev, &delta)
}

func TestProjectorLocksExactTargetCustodyIdentity(t *testing.T) {
	p, ctx := projectorFixture(t)
	// Each case captures a fact before a target identity change. None may
	// write an event or current balance under the stale captured identity.
	for _, field := range []string{"cluster", "wallet", "wallet_token_ata", "vault_pubkey", "vault_token_ata", "token_mint"} {
		t.Run(field, func(t *testing.T) {
			a := seedProjectionTarget(t, ctx, p)
			before, err := p.readOffset(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id := captureProjectionObservation(t, ctx, p, a, 100, 700)
			var original *string
			if err := p.yield.QueryRow(ctx, `SELECT `+field+` FROM loyal_yield.balance_sweep_targets WHERE id=$1`, a.ID).Scan(&original); err != nil {
				t.Fatal(err)
			}
			var changed any = solana.NewWallet().PublicKey().String()
			if field == "cluster" {
				changed = nil
			}
			if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET `+field+`=$2 WHERE id=$1`, a.ID, changed); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Tick(ctx); err == nil || !strings.Contains(err.Error(), "custody identity differs") {
				t.Fatalf("changed target identity accepted: %v", err)
			}
			var count int
			if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale target event committed: %d %v", count, err)
			}
			if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1`, a.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale target current balance committed: %d %v", count, err)
			}
			if offset, err := p.readOffset(ctx); err != nil || offset != before {
				t.Fatalf("stale identity advanced checkpoint: %d %v", offset, err)
			}
			if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET `+field+`=$2 WHERE id=$1`, a.ID, original); err != nil {
				t.Fatal(err)
			}
			if out, err := p.Tick(ctx); err != nil || out.LastEventID != id {
				t.Fatalf("restored custody did not retry exact captured ID: %+v %v", out, err)
			}
		})
	}
	// A real target row lock must fence identity validation even when no
	// current balance exists; locking only current would miss this race.
	a := seedProjectionTarget(t, ctx, p)
	id := captureProjectionObservation(t, ctx, p, a, 100, 701)
	tx, err := p.yield.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_token_ata=$2 WHERE id=$1`, a.ID, solana.NewWallet().PublicKey().String()); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = p.Tick(blocked)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("target identity lock did not fence projector: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tick(ctx); err == nil || !strings.Contains(err.Error(), "custody identity differs") {
		t.Fatalf("committed changed custody bypassed target lock: %v", err)
	}
	if _, err := p.yield.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_token_ata=$2,desired_active=false,chain_status='closed' WHERE id=$1`, a.ID, a.WalletATA); err != nil {
		t.Fatal(err)
	}
	// Closed desired state does not invalidate an otherwise matching captured
	// balance fact; the financial executor applies its own desired-state gate.
	if out, err := p.Tick(ctx); err != nil || out.LastEventID != id {
		t.Fatalf("valid historical closed target fact held: %+v %v", out, err)
	}
}

func TestProjectorRunDrainsForeignFullBatchesWithoutPollDelay(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	for i := range 2 {
		id := captureProjectionObservation(t, ctx, p, a, 999, uint64(800+i))
		if _, err := p.capture.Exec(ctx, `UPDATE loyal_prod.balance_sweep_wallet_ata_observations SET cluster='devnet' WHERE event_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	known := captureProjectionObservation(t, ctx, p, a, 100, 802)
	p.batchLimit = 1
	p.pollInterval = time.Minute
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("foreign batch runtime failed to join")
			}
		}
	}()
	go func() { done <- p.Run(runCtx) }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id=$1`, known).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("foreign-heavy full batches waited for the one-minute poll")
		case <-ticker.C:
		}
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("foreign backlog runtime did not cancel: %v", err)
	}
	joined = true
	assertProjectedDelta(t, ctx, p, known, nil, nil)
}
