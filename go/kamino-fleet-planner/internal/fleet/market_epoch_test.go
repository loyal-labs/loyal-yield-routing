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
	verified := make([]VerifiedSupportedReserveRow, 0, len(identities))
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
		verified = append(verified, VerifiedSupportedReserveRow{
			StateEventID: int64(index + 1), AccountDataHash: state.DataHash,
			StateObservedAt: snapshot.ObservedAt, StateSlot: state.Slot,
			VerifiedAt: snapshot.ObservedAt, VerifiedSlot: state.Slot,
			VerificationCommitment: "confirmed", VerificationSource: "http_confirmed_refresh",
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
		CapturedAt: snapshot.ObservedAt, Catalog: catalog, VerifiedReserves: verified,
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
	if err := epoch.VerifyDirectObservation(drifted, source.Address, target.Address); err == nil {
		t.Fatal("changed direct account bytes were accepted under old durable evidence")
	}
	changedReserves := append([]MarketEpochReserve(nil), epoch.Reserves...)
	changedReserves[0].StateEventID++
	changedFingerprint := epochFingerprint(changedReserves, []string{USDCMint}, epoch.CatalogFingerprint, epoch.MintCoverage)
	if changedFingerprint == epoch.Fingerprint || positiveEpochID(changedFingerprint) == epoch.OptimizerEpochID {
		t.Fatal("changed monitor event identity reused the prior epoch identity")
	}
}

func TestMarketEvidenceStoreLoadsRealMonitorIdentity(t *testing.T) {
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
	if !validSQLIdentifier(schema) {
		t.Fatal("generated invalid schema")
	}
	_, err = pool.Exec(ctx, fmt.Sprintf(`
CREATE SCHEMA %s;
CREATE TABLE %s.supported_reserves(
 market text,liquidity_mint text,reserve text,market_name text,symbol text,risk_baskets text[],source text,fetched_at timestamptz,active boolean
);
CREATE TABLE %s.latest_verified_reserve_updates(
 event_id bigint,account_data_hash text,observed_at timestamptz,slot bigint,verified_at timestamptz,verified_slot bigint,
 verification_commitment text,verification_source text,reserve text,market text,market_name text,liquidity_mint text,symbol text,mint_decimals integer,
 reserve_last_update_slot bigint,reserve_last_update_stale boolean,reserve_price_status smallint,available_amount double precision,
 borrowed_amount double precision,total_supply_amount double precision,market_price_usd double precision,market_price_last_updated_ts bigint,
 utilization double precision,borrow_apy double precision,supply_apy double precision
)`, schema, schema, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index, name := range []string{"a", "b", "c"} {
		market, reserve := "market-"+name, "reserve-"+name
		_, err = pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.supported_reserves VALUES($1,$2,$3,$4,'USDC',ARRAY['safe'],'kamino-api',$5,true)`, schema), market, USDCMint, reserve, strings.ToUpper(name), now)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.latest_verified_reserve_updates VALUES($1,$2,$3,999,$3,1000,'confirmed','http_confirmed_refresh',$4,$5,$6,$7,'USDC',6,998,false,0,1800000000000,200000000000,2000000000000,1,1700000000,.1,.01,.008)`, schema), index+1, strings.Repeat(name, 64), now, reserve, market, strings.ToUpper(name), USDCMint)
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenMarketEvidenceStore(ctx, databaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	epoch, err := store.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(epoch.Reserves) != 3 || epoch.CatalogReserveCount != 3 || epoch.Reserves[0].StateEventID <= 0 {
		t.Fatalf("monitor identities or complete catalog were not retained: %+v", epoch)
	}
}
