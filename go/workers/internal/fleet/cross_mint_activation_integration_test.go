package fleet

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func crossMintActivationFixtureStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires registered disposable C fleet database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || u.Port() != "51913" || u.Path != "/fleet" || u.User == nil || u.User.Username() != "workers_v2" || u.Fragment != "" {
		t.Fatal("requires root-registered loopback workers_v2 /fleet before connecting")
	}
	if _, present := u.User.Password(); present {
		t.Fatal("disposable fixture cannot contain credentials")
	}
	for key, values := range u.Query() {
		if key != "sslmode" || len(values) != 1 || values[0] != "disable" {
			t.Fatal("unregistered database connection option")
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("configured registered database unavailable:", err)
	}
	store, err := NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

// These are actual SQL lease/identity tests. The mutated plan is deliberately
// structural test data and does not assert a chain certificate or simulation.
func TestCrossMintActivationClaimKeepsFamilyAndLeaseOwnership(t *testing.T) {
	store, ctx := crossMintActivationFixtureStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "cross-activation-" + suffix
	source := ReserveIdentity{Address: testIdentity(43), Market: testIdentity(42), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(44), Market: source.Market, Mint: USDCMint}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 1000, ObservedAt: now, Hash: suffix, Reserves: map[string]ReserveState{source.Address: {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)}, target.Address: {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)}}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	seed := func(name string) (PublishResult, VaultPosition) {
		id := seedWorkerVault(t, ctx, store, suffix+name, source.Market, source.Address)
		position, err := store.LoadVaultPosition(ctx, cluster, id, source, target)
		if err != nil {
			t.Fatal(err)
		}
		decision := Plan(snapshot, position, source.Address, target.Address)
		if !decision.Eligible {
			t.Fatal(decision.Reason)
		}
		published, err := store.Publish(ctx, cluster, epoch, position, decision)
		if err != nil || !published.Inserted {
			t.Fatalf("publish=%+v err=%v", published, err)
		}
		return published, position
	}
	same, _ := seed("same")
	cross, position := seed("cross")
	const signer = "activation-test-signer"
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true,delegated_signers=ARRAY[$3]::text[] WHERE id=$1`, position.PolicyID, cluster, signer); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET source_liquidity_mint=$2,target_liquidity_mint=$3,liquidity_mint=$3,execution_plan=jsonb_set(jsonb_set(jsonb_set(execution_plan,'{kind}',to_jsonb('cross_mint_jupiter'::text)),'{route_kind}',to_jsonb('cross_mint_jupiter'::text)),'{policy_bindings}',jsonb_build_object('delegated_signer',$4::text,'withdraw',jsonb_build_object('policy_account',$5::text))) WHERE id=$1`, cross.OpportunityID, USDCMint, USDTMint, signer, position.PolicyAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_movement_controls(cluster,start_new_movements,continue_or_recover_existing,generation) VALUES($1,false,true,1)`, cluster); err != nil {
		t.Fatal(err)
	}
	claim := func(owner string) *RevalidationLease {
		t.Helper()
		l, err := store.ClaimCrossMintActivation(ctx, cluster, owner, time.Minute, signer)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if got := claim("disabled"); got != nil {
		t.Fatal("disabled movement creation took a lease")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET start_new_movements=true,generation=2 WHERE cluster=$1`, cluster); err != nil {
		t.Fatal(err)
	}
	if got := claim("unprepared"); got != nil {
		t.Fatal("unprepared revalidation candidate was promoted to execute")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='ready',route_fingerprint=$2,requirements_fingerprint=$3 WHERE id=$1`, cross.OpportunityID, strings.Repeat("c", 64), strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	l := claim("first")
	if l == nil || l.OpportunityID != cross.OpportunityID || l.RouteKind != "cross_mint_jupiter" || l.SourceLiquidityMint != USDCMint || l.TargetLiquidityMint != USDTMint || l.SourceSnapshotID == nil {
		t.Fatalf("incorrect dedicated claim: %+v", l)
	}
	if got := claim("competitor"); got != nil {
		t.Fatalf("competitor took existing lease: %+v", got)
	}
	var sameState, kind string
	if err := store.pool.QueryRow(ctx, `SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id=$1`, same.OpportunityID).Scan(&sameState); err != nil || sameState != "revalidate" {
		t.Fatalf("dedicated claim changed same-mint row: %s %v", sameState, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT lease_kind FROM loyal_yield.rebalance_opportunities WHERE id=$1`, cross.OpportunityID).Scan(&kind); err != nil || kind != "execute" {
		t.Fatalf("wrong activation authority: %s %v", kind, err)
	}
	if generation, err := store.CheckCrossMintActivationLease(ctx, *l); err != nil || generation != 2 {
		t.Fatalf("actual execute guard: generation=%d err=%v", generation, err)
	}
	changed := *l
	changed.Owner = "another-owner"
	if _, err := store.CheckCrossMintActivationLease(ctx, changed); err == nil {
		t.Fatal("foreign owner passed source execute guard")
	}
	changed = *l
	changed.SourceSnapshotID = nil
	if _, err := store.CheckCrossMintActivationLease(ctx, changed); err == nil {
		t.Fatal("substituted source snapshot passed guard")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET continue_or_recover_existing=false,generation=3 WHERE cluster=$1`, cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckCrossMintActivationLease(ctx, *l); err == nil {
		t.Fatal("revoked generation permitted activation")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET continue_or_recover_existing=true,generation=4 WHERE cluster=$1`, cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET lease_expires_at=clock_timestamp()-interval '1 millisecond',lease_kind='revalidate' WHERE id=$1`, cross.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if got := claim("wrong-lane-recovery"); got != nil {
		t.Fatal("activation stole expired revalidation lease")
	}
	revalidated, err := store.ClaimRevalidation(ctx, cluster, "old-revalidator", time.Minute, false, true, signer)
	if err != nil || revalidated == nil || revalidated.OpportunityID != l.OpportunityID {
		t.Fatalf("ordinary claim lost previous recovery behavior: %+v %v", revalidated, err)
	}
	if _, err := store.CheckCrossMintActivationLease(ctx, *revalidated); err == nil {
		t.Fatal("revalidation lease passed execute guard")
	}
	if generation, err := store.CheckCrossMintPreflightLease(ctx, *revalidated); err != nil || generation != 4 {
		t.Fatalf("actual preflight lease guard: generation=%d err=%v", generation, err)
	}
	oldGeneration := int64(3)
	commit := RevalidationCommit{CrossMintControlGeneration: &oldGeneration, Disposition: "ready", Preparation: &RoutePreparation{}, ExpectedOpportunityKey: revalidated.IdempotencyKey, ExpectedEpochFingerprint: revalidated.OptimizerEpochKey}
	if err := store.CommitRevalidation(ctx, *revalidated, commit); err == nil || !strings.Contains(err.Error(), "control generation changed") {
		t.Fatalf("old control generation reached capacity or source ready publication: %v", err)
	}
	commit.CrossMintControlGeneration = nil
	if err := store.CommitRevalidation(ctx, *revalidated, commit); err == nil || !strings.Contains(err.Error(), "captured movement control generation") {
		t.Fatalf("missing control generation reached ready publication: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='ready',lease_owner=NULL,lease_expires_at=NULL,lease_kind=NULL WHERE id=$1`, cross.OpportunityID); err != nil {
		t.Fatal(err)
	}
	ready := claim("ready-activation")
	if ready == nil || ready.OpportunityID != l.OpportunityID || ready.FencingToken <= revalidated.FencingToken {
		t.Fatalf("ready cross-mint candidate not fenced correctly: %+v", ready)
	}
}
