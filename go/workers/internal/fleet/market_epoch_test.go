package fleet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testImmutableMarketEpoch(t *testing.T, snapshot MarketSnapshot, identities ...ReserveIdentity) ImmutableMarketEpoch {
	t.Helper()
	catalog := make([]SupportedReserveCatalogRow, 0, len(identities))
	states := make([]SupportedReserveStateRow, 0, len(identities))
	for index, identity := range identities {
		state, ok := snapshot.Reserves[identity.Address]
		if !ok {
			t.Fatalf("missing test reserve %s", identity.Address)
		}
		market := identity.Market
		catalog = append(catalog, SupportedReserveCatalogRow{
			Market: identity.Market, LiquidityMint: identity.Mint, Reserve: identity.Address,
			RiskBaskets: []string{"safe"}, Source: "kamino-api", FetchedAt: snapshot.ObservedAt,
		})
		states = append(states, SupportedReserveStateRow{
			StateEventID: int64(index + 1), AccountDataHash: state.DataHash,
			StateObservedAt: snapshot.ObservedAt, StateSlot: state.Slot,
			Source: "laserstream_grpc", SourceCommitment: "confirmed",
			Reserve: identity.Address, Market: &market, LiquidityMint: identity.Mint, MintDecimals: 6,
			ReserveLastUpdateSlot: state.LastUpdateSlot, ReserveLastUpdateStale: state.LastUpdateStale,
			AvailableAmount: float64(state.TotalSupplyUSDMicros), TotalSupplyAmount: float64(state.TotalSupplyUSDMicros),
			MarketPriceUSD: 1, SupplyAPY: float64(state.SupplyAPYBPS) / 10_000,
		})
	}
	supportedMints := make([]string, 0, len(identities))
	for _, identity := range identities {
		if !contains(supportedMints, identity.Mint) {
			supportedMints = append(supportedMints, identity.Mint)
		}
	}
	epoch, err := BuildImmutableMarketEpoch(SupportedReserveMarketSnapshot{
		CapturedAt: snapshot.ObservedAt, Catalog: catalog, ReserveStates: states,
	}, supportedMints)
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestImmutableMarketEpochUsesDurableStateIdentityAndCanonicalFingerprint(t *testing.T) {
	now := time.Date(2026, 9, 2, 17, 8, 9, 123456000, time.UTC)
	source := ReserveIdentity{Address: testIdentity(11), Market: testIdentity(12), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(13), Market: testIdentity(14), Mint: USDCMint}
	snapshot := MarketSnapshot{Slot: 1_000, ObservedAt: now, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 1_000, LastUpdateSlot: 999, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 2_000_000_000_000, DataHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		target.Address: {ReserveIdentity: target, Slot: 1_000, LastUpdateSlot: 999, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 3_000_000_000_000, DataHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	if epoch.CatalogReserveCount != 2 || len(epoch.Reserves) != 2 || len(epoch.MintCoverage) != 1 || !epoch.MintCoverage[0].Complete {
		t.Fatalf("incomplete epoch: %+v", epoch)
	}
	if epoch.Reserves[0].StateEventID <= 0 || epoch.Reserves[0].AccountDataHash == "" || epoch.Fingerprint == "" || epoch.OptimizerEpochID <= 0 {
		t.Fatalf("state identity was not retained: %+v", epoch)
	}
	if err := epoch.VerifyDirectObservation(snapshot, source.Address, target.Address); err != nil {
		t.Fatal(err)
	}
	drifted := snapshot
	drifted.Reserves = map[string]ReserveState{}
	for address, reserve := range snapshot.Reserves {
		drifted.Reserves[address] = reserve
	}
	changed := drifted.Reserves[target.Address]
	changed.DataHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	drifted.Reserves[target.Address] = changed
	mismatch := epoch.VerifyDirectObservation(drifted, source.Address, target.Address)
	if mismatch == nil {
		t.Fatal("changed direct account bytes were accepted under old durable evidence")
	}
	// Rust has no direct-observation fence, so a bare hash difference must not
	// stop publish planning either; the durable epoch remains the planning input.
	if !tolerableObservationDifference(mismatch) {
		t.Fatal("a bare account-hash difference must be tolerated in every mode")
	}
	// A hash difference must not mask a fatal error in a later reserve.
	invalidSource := drifted.Reserves[source.Address]
	invalidSource.Market = target.Market
	drifted.Reserves[source.Address] = invalidSource
	identityErr := epoch.VerifyDirectObservation(drifted, target.Address, source.Address)
	if identityErr == nil || tolerableObservationDifference(identityErr) {
		t.Fatal("hash mismatch masked later reserve identity failure")
	}
	drifted.Reserves[source.Address] = snapshot.Reserves[source.Address]
	invalidSource = drifted.Reserves[source.Address]
	invalidSource.Slot = 1
	drifted.Reserves[source.Address] = invalidSource
	slotErr := epoch.VerifyDirectObservation(drifted, target.Address, source.Address)
	if slotErr == nil || tolerableObservationDifference(slotErr) {
		t.Fatal("hash mismatch masked later reserve slot failure")
	}
	drifted.Reserves[source.Address] = snapshot.Reserves[source.Address]
	drifted.ObservedAt = epoch.OptimizerEnvelopeExpiresAt().Add(time.Second)
	expiryErr := epoch.VerifyDirectObservation(drifted, target.Address, source.Address)
	if expiryErr == nil || tolerableObservationDifference(expiryErr) {
		t.Fatal("shadow accepted an expired epoch after a hash mismatch")
	}
	changedReserves := append([]MarketEpochReserve(nil), epoch.Reserves...)
	changedReserves[0].StateEventID++
	changedFingerprint := epochFingerprint(changedReserves, []string{USDCMint}, epoch.CatalogFingerprint, epoch.MintCoverage)
	if changedFingerprint == epoch.Fingerprint || positiveEpochID(changedFingerprint) == epoch.OptimizerEpochID {
		t.Fatal("changed monitor event identity reused the prior epoch identity")
	}
}

func TestImmutableMarketEpochKeepsHealthyMintRoutableWhenAnotherMintIsBlocked(t *testing.T) {
	now := time.Now().UTC()
	healthySource := ReserveIdentity{Address: testIdentity(21), Market: testIdentity(22), Mint: USDCMint}
	healthyTarget := ReserveIdentity{Address: testIdentity(23), Market: testIdentity(24), Mint: USDCMint}
	blocked := ReserveIdentity{Address: testIdentity(25), Market: testIdentity(26), Mint: USDTMint}
	catalog := []SupportedReserveCatalogRow{
		{Market: healthySource.Market, LiquidityMint: healthySource.Mint, Reserve: healthySource.Address, Source: "kamino-api", FetchedAt: now},
		{Market: healthyTarget.Market, LiquidityMint: healthyTarget.Mint, Reserve: healthyTarget.Address, Source: "kamino-api", FetchedAt: now},
		{Market: blocked.Market, LiquidityMint: blocked.Mint, Reserve: blocked.Address, Source: "kamino-api", FetchedAt: now},
	}
	states := make([]SupportedReserveStateRow, 0, 2)
	for index, identity := range []ReserveIdentity{healthySource, healthyTarget} {
		market := identity.Market
		states = append(states, SupportedReserveStateRow{
			StateEventID: int64(index + 1), AccountDataHash: strings.Repeat(fmt.Sprint(index+1), 64),
			StateObservedAt: now, StateSlot: 1_000,
			Source: "laserstream_grpc", SourceCommitment: "confirmed",
			Reserve: identity.Address, Market: &market, LiquidityMint: identity.Mint, MintDecimals: 6,
			ReserveLastUpdateSlot: 999, TotalSupplyAmount: 2_000_000_000_000,
			AvailableAmount: 2_000_000_000_000, MarketPriceUSD: 1, SupplyAPY: .08,
		})
	}
	epoch, err := BuildImmutableMarketEpoch(SupportedReserveMarketSnapshot{CapturedAt: now, Catalog: catalog, ReserveStates: states}, []string{USDCMint, USDTMint})
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		t.Fatalf("blocked mint invalidated healthy coverage: %v", err)
	}
	addresses, err := epoch.RoutableReserveAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if epoch.CatalogReserveCount != 3 || len(addresses) != 2 || !contains(addresses, healthySource.Address) || !contains(addresses, healthyTarget.Address) || contains(addresses, blocked.Address) {
		t.Fatalf("wrong per-mint routable frontier: catalog=%d addresses=%v", epoch.CatalogReserveCount, addresses)
	}
	for _, coverage := range epoch.MintCoverage {
		if coverage.Mint == USDTMint && coverage.Complete {
			t.Fatal("mint with a missing reserve state was marked complete")
		}
		if coverage.Mint == USDCMint && !coverage.Complete {
			t.Fatal("healthy mint was blocked by unrelated mint coverage")
		}
	}
}

// A stale-flagged reserve stays a refresh-before-withdraw source; it never
// becomes a target, so a mint whose only target is stale is incomplete.
func TestImmutableMarketEpochRefusesStaleFlaggedTarget(t *testing.T) {
	now := time.Now().UTC()
	reserve := ReserveIdentity{Address: testIdentity(31), Market: testIdentity(32), Mint: USDCMint}
	market := reserve.Market
	epoch, err := BuildImmutableMarketEpoch(SupportedReserveMarketSnapshot{
		CapturedAt: now,
		Catalog:    []SupportedReserveCatalogRow{{Market: reserve.Market, LiquidityMint: reserve.Mint, Reserve: reserve.Address, Source: "kamino-api", FetchedAt: now}},
		ReserveStates: []SupportedReserveStateRow{{
			StateEventID: 1, AccountDataHash: strings.Repeat("a", 64), StateObservedAt: now, StateSlot: 1_000,
			Source: "laserstream_grpc", SourceCommitment: "confirmed",
			Reserve: reserve.Address, Market: &market, LiquidityMint: reserve.Mint, MintDecimals: 6,
			ReserveLastUpdateSlot: 999, ReserveLastUpdateStale: true, TotalSupplyAmount: 2_000_000_000_000,
			AvailableAmount: 2_000_000_000_000, MarketPriceUSD: 1, SupplyAPY: .08,
		}},
	}, []string{USDCMint})
	if err != nil {
		t.Fatal(err)
	}
	coverage := epoch.MintCoverage[0]
	if coverage.Complete || coverage.EligibleTargetReserveCount != 0 || len(epoch.Reserves) != 0 {
		t.Fatalf("stale-flagged reserve was admitted as a target: %+v", epoch)
	}
	codes := map[string]bool{}
	for _, blocker := range coverage.Blockers {
		codes[blocker.Code] = true
	}
	if !codes["explicit_stale_economics"] || !codes["no_eligible_target"] {
		t.Fatalf("blockers = %+v, want explicit_stale_economics and no_eligible_target", coverage.Blockers)
	}
}

// The planner reads each reserve's latest confirmed stream row. A quiet
// reserve's hour-old row stays admissible while the stream is live; an older
// slot that arrived later (a replay after a restart) never replaces a newer
// one; and the epoch is refused once the stream frontier trails the chain.
func TestMarketEvidenceStoreReadsLatestStreamRows(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("kamino_epoch_%d", time.Now().UnixNano())
	_, err = pool.Exec(ctx, fmt.Sprintf(`
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.supported_reserves(
 market text,liquidity_mint text,reserve text,market_name text,symbol text,risk_baskets text[],source text,fetched_at timestamptz,active boolean
);
CREATE TABLE %[1]s.reserve_update_dedupe(event_id bigint,reserve text,slot bigint);
CREATE TABLE %[1]s.reserve_updates(
 event_id bigint,account_data_hash text,observed_at timestamptz,slot bigint,source text,source_commitment text,
 reserve text,market text,market_name text,liquidity_mint text,symbol text,mint_decimals integer,
 reserve_last_update_slot bigint,reserve_last_update_stale boolean,reserve_price_status smallint,available_amount double precision,
 borrowed_amount double precision,total_supply_amount double precision,market_price_usd double precision,market_price_last_updated_ts bigint,
 utilization double precision,borrow_apy double precision,supply_apy double precision
)`, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	now := time.Now().UTC().Truncate(time.Microsecond)
	eventID := 0
	insert := func(name, source string, slot int64, observedAt time.Time) {
		t.Helper()
		eventID++
		market, reserve := "market-"+name, "reserve-"+name
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.reserve_update_dedupe VALUES($1,$2,$3)`, schema), eventID, reserve, slot); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.reserve_updates VALUES($1,$2,$3,$4,$5,'confirmed',$6,$7,$8,$9,'USDC',6,$10,false,0,1800000000000,200000000000,2000000000000,1,1700000000,.1,.01,.008)`, schema), eventID, strings.Repeat(fmt.Sprintf("%x", eventID%16), 64), observedAt, slot, source, reserve, market, strings.ToUpper(name), USDCMint, slot-1); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.supported_reserves VALUES($1,$2,$3,$4,'USDC',ARRAY['safe'],'kamino-api',$5,true)`, schema), "market-"+name, USDCMint, "reserve-"+name, strings.ToUpper(name), now); err != nil {
			t.Fatal(err)
		}
	}
	insert("a", "http_snapshot", 2_000, now.Add(-time.Second))
	insert("a", "laserstream_grpc", 1_900, now) // a replayed older slot arriving after the seed
	insert("b", "laserstream_grpc", 2_100, now)
	insert("c", "laserstream_grpc", 1_200, now.Add(-time.Hour)) // quiet for an hour
	store, err := NewMarketEvidenceStoreFromPool(pool, schema)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(epoch.Reserves) != 3 || epoch.StreamFrontierSlot != 2_100 {
		t.Fatalf("stream rows were not all admitted: frontier=%d %+v", epoch.StreamFrontierSlot, epoch)
	}
	for _, reserve := range epoch.Reserves {
		want := map[string]int64{"reserve-a": 2_000, "reserve-b": 2_100, "reserve-c": 1_200}[reserve.Reserve]
		if reserve.StateSlot != want || reserve.Slot != want {
			t.Fatalf("reserve %s read slot %d/%d, want its latest row at %d", reserve.Reserve, reserve.StateSlot, reserve.Slot, want)
		}
	}
	if err := epoch.VerifyDirectObservation(MarketSnapshot{Slot: epoch.StreamFrontierSlot + maximumReserveEconomicSlotLag}); err != nil {
		t.Fatalf("live stream frontier was refused: %v", err)
	}
	if err := epoch.VerifyDirectObservation(MarketSnapshot{Slot: epoch.StreamFrontierSlot + maximumReserveEconomicSlotLag + 1}); err == nil {
		t.Fatal("a stream frontier trailing the chain was accepted")
	}
}

// A direct read that differs from the stream row at or after its slot is the
// chain at a known slot, so it is what gets planned; an older one is not.
func TestPlanningUsesTheDirectReadWhereItDiffersFromTheStreamRow(t *testing.T) {
	now := time.Now().UTC()
	source := ReserveIdentity{Address: testIdentity(41), Market: testIdentity(42), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(43), Market: testIdentity(44), Mint: USDCMint}
	stream := MarketSnapshot{Slot: 1_000, ObservedAt: now, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 1_000, LastUpdateSlot: 999, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 2_000_000_000_000, DataHash: strings.Repeat("a", 64)},
		target.Address: {ReserveIdentity: target, Slot: 1_000, LastUpdateSlot: 999, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 3_000_000_000_000, DataHash: strings.Repeat("b", 64)},
	}}
	epoch := testImmutableMarketEpoch(t, stream, source, target)
	planned, err := marketSnapshotFromEpoch(epoch, source.Address, target.Address)
	if err != nil {
		t.Fatal(err)
	}
	moved := ReserveState{ReserveIdentity: target, Slot: 1_010, LastUpdateSlot: 1_010, SupplyAPYBPS: 300, TotalSupplyUSDMicros: 3_100_000_000_000, DataHash: strings.Repeat("c", 64)}
	older := stream.Reserves[source.Address]
	older.Slot, older.DataHash = 999, strings.Repeat("d", 64)
	direct := MarketSnapshot{Slot: 1_010, ObservedAt: now.Add(time.Second), Reserves: map[string]ReserveState{source.Address: older, target.Address: moved}}
	planDirectReads(planned, direct)
	if got := planned.Reserves[target.Address]; got.DataHash != moved.DataHash || got.SupplyAPYBPS != 300 || got.Slot != 1_010 || !got.ObservedAt.Equal(direct.ObservedAt) {
		t.Fatalf("planned target = %+v, want the direct read at slot 1010", got)
	}
	if got := planned.Reserves[source.Address]; got.Slot != 1_000 || !got.ObservedAt.Equal(now) {
		t.Fatalf("a direct read older than the stream row replaced it: %+v", got)
	}
}

// A reserve the stream evidence makes target-ineligible is never planned as a
// target even when its direct read is fresh: planning and Publish read the one
// epoch flag, so the cycle skips it instead of failing at publication.
func TestStreamIneligibleTargetIsConsistentlySkippedDespiteAFreshDirectRead(t *testing.T) {
	now := time.Now().UTC()
	market := testIdentity(52)
	source := ReserveIdentity{Address: testIdentity(51), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(53), Market: market, Mint: USDCMint}
	row := func(identity ReserveIdentity, event int64, hash string, apy float64, stale bool) SupportedReserveStateRow {
		return SupportedReserveStateRow{StateEventID: event, AccountDataHash: strings.Repeat(hash, 64), StateObservedAt: now, StateSlot: 1_000,
			Source: "laserstream_grpc", SourceCommitment: "confirmed", Reserve: identity.Address, Market: &market, LiquidityMint: USDCMint, MintDecimals: 6,
			ReserveLastUpdateSlot: 999, ReserveLastUpdateStale: stale, TotalSupplyAmount: 2_000_000_000_000, AvailableAmount: 2_000_000_000_000, MarketPriceUSD: 1, SupplyAPY: apy}
	}
	plan := func(targetStale bool) (FleetPlan, []string) {
		t.Helper()
		catalog := []SupportedReserveCatalogRow{
			{Market: market, LiquidityMint: USDCMint, Reserve: source.Address, Source: "kamino-api", FetchedAt: now},
			{Market: market, LiquidityMint: USDCMint, Reserve: target.Address, Source: "kamino-api", FetchedAt: now},
		}
		epoch, err := BuildImmutableMarketEpoch(SupportedReserveMarketSnapshot{CapturedAt: now, Catalog: catalog, ReserveStates: []SupportedReserveStateRow{row(source, 1, "a", .02, false), row(target, 2, "b", .08, targetStale)}}, []string{USDCMint})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := marketSnapshotFromEpoch(epoch, source.Address, target.Address)
		if err != nil {
			t.Fatal(err)
		}
		fresh := ReserveState{ReserveIdentity: target, Slot: 1_010, LastUpdateSlot: 1_010, SupplyAPYBPS: 800, TotalSupplyUSDMicros: 2_000_000_000_000, EconomicLifetimeMillis: 300_000, DataHash: strings.Repeat("c", 64)}
		planDirectReads(snapshot, MarketSnapshot{Slot: 1_010, ObservedAt: now, Reserves: map[string]ReserveState{target.Address: fresh}})
		allowed := sameMintTargets(epoch, USDCMint, []string{market})
		result, err := PlanFleet(snapshot, []FleetVault{oracleVault(snapshot, 1, source.Address, 1_000_000_000, allowed...)})
		if err != nil {
			t.Fatal(err)
		}
		for _, opportunity := range result.Opportunities {
			if evidence, ok := epoch.Reserve(opportunity.Decision.TargetReserve); !ok || !evidence.TargetEligible {
				t.Fatalf("planned a target Publish refuses: %+v", opportunity.Decision)
			}
		}
		return result, allowed
	}
	if result, allowed := plan(false); len(allowed) != 2 || len(result.Opportunities) != 1 || result.Opportunities[0].Decision.TargetReserve != target.Address {
		t.Fatalf("eligible target was not planned: allowed=%v plan=%+v", allowed, result)
	}
	if result, allowed := plan(true); contains(allowed, target.Address) || len(result.Opportunities) != 0 {
		t.Fatalf("stream-ineligible target was planned: allowed=%v plan=%+v", allowed, result)
	}
}
