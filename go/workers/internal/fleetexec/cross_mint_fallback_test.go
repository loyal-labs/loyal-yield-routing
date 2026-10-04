package fleetexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type fallbackEpochSource struct {
	epoch fleet.ImmutableMarketEpoch
	err   error
	calls int
}

func (s *fallbackEpochSource) LoadImmutableMarketEpoch(context.Context) (fleet.ImmutableMarketEpoch, error) {
	s.calls++
	return s.epoch, s.err
}

func fallbackEpochFixture(t *testing.T, now time.Time, reserves []fleet.MarketEpochReserve) fleet.ImmutableMarketEpoch {
	t.Helper()
	snapshot := fleet.SupportedReserveMarketSnapshot{CapturedAt: now}
	for i, r := range reserves {
		market := *r.Market
		snapshot.Catalog = append(snapshot.Catalog, fleet.SupportedReserveCatalogRow{Reserve: r.Reserve, Market: market, LiquidityMint: r.LiquidityMint, RiskBaskets: []string{"safe"}, Source: "kamino-api", FetchedAt: now})
		snapshot.VerifiedReserves = append(snapshot.VerifiedReserves, fleet.VerifiedSupportedReserveRow{StateEventID: int64(i + 1), AccountDataHash: r.AccountDataHash, StateObservedAt: now, StateSlot: r.Slot, VerifiedAt: now, VerifiedSlot: r.Slot, VerificationCommitment: "confirmed", VerificationSource: "http_confirmed_refresh", Reserve: r.Reserve, Market: &market, LiquidityMint: r.LiquidityMint, MintDecimals: 6, ReserveLastUpdateSlot: r.Slot, ReserveLastUpdateStale: !r.TargetEligible, AvailableAmount: 1000000000000, TotalSupplyAmount: 1000000000000, MarketPriceUSD: 1, SupplyAPY: float64(r.SupplyAPYBPS) / 10000})
	}
	e, err := fleet.BuildImmutableMarketEpoch(snapshot, []string{reserves[0].LiquidityMint})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Validate(); err != nil {
		t.Fatalf("%v coverage=%+v", err, e.MintCoverage)
	}
	return e
}

func TestCrossMintFallbackRanksSameMintEligibleReservesDeterministically(t *testing.T) {
	market := "market"
	m := CrossMintMovement{TargetMint: fleet.USDTMint, ActiveTargetReserve: "primary", IntendedTargetReserve: "primary"}
	makeReserve := func(address, mint string, apy int64, eligible bool) fleet.MarketEpochReserve {
		return fleet.MarketEpochReserve{Reserve: address, Market: &market, LiquidityMint: mint, SupplyAPYBPS: apy, TargetEligible: eligible, Slot: 1000, StateSlot: 1000, AccountDataHash: strings.Repeat("a", 64), TotalSupplyUSDMicros: 1000000000000}
	}
	rows := []fleet.MarketEpochReserve{makeReserve("primary", m.TargetMint, 9999, true), makeReserve("wrong-mint", fleet.USDCMint, 9999, true), makeReserve("blocked", m.TargetMint, 9999, false), makeReserve("a", m.TargetMint, 400, true), makeReserve("z", m.TargetMint, 400, true), makeReserve("lower", m.TargetMint, 399, true)}
	for i := 0; i < 2; i++ {
		r, err := selectCrossMintFallback(rows, m)
		if err != nil || r.Reserve != "z" {
			t.Fatalf("wrong fallback %s %v", r.Reserve, err)
		}
		for a, b := 0, len(rows)-1; a < b; a, b = a+1, b-1 {
			rows[a], rows[b] = rows[b], rows[a]
		}
	}
	m.ActiveTargetReserve = "z"
	if _, err := selectCrossMintFallback(rows, m); err == nil {
		t.Fatal("bound fallback oscillated to another reserve")
	}
	m.ActiveTargetReserve = "primary"
	if _, err := selectCrossMintFallback(rows[:0], m); err == nil {
		t.Fatal("unknown reserve universe authorized fallback")
	}
}

func TestCrossMintFallbackRequiresCurrentCompleteEpochAndPreservesSourceErrors(t *testing.T) {
	now := time.Now().UTC()
	market := "market"
	anchor := int64(900)
	m := CrossMintMovement{Phase: CrossMintTargetIdle, TargetMint: fleet.USDTMint, CustodyMint: fleet.USDTMint, CustodyReconciledSlot: &anchor, ActiveTargetReserve: "primary", IntendedTargetReserve: "primary"}
	e := fallbackEpochFixture(t, now, []fleet.MarketEpochReserve{{Reserve: "primary", Market: &market, LiquidityMint: m.TargetMint, SupplyAPYBPS: 100, TargetEligible: false, Slot: 1000, AccountDataHash: strings.Repeat("a", 64)}, {Reserve: "fallback", Market: &market, LiquidityMint: m.TargetMint, SupplyAPYBPS: 200, TargetEligible: true, Slot: 1000, AccountDataHash: strings.Repeat("b", 64)}})
	if err := validateCrossMintFallbackEpoch(e, m, now); err != nil {
		t.Fatal(err)
	}
	if crossMintActiveTargetEligible(e, m) {
		t.Fatal("stale source-only primary admitted as target")
	}
	if err := validateCrossMintFallbackEpoch(e, m, e.OptimizerEnvelopeExpiresAt()); err == nil {
		t.Fatal("expired market epoch admitted")
	}
	changed := e
	changed.Fingerprint = "forged"
	if err := validateCrossMintFallbackEpoch(changed, m, now); err == nil {
		t.Fatal("forged epoch admitted")
	}
	m.CustodyReconciledSlot = nil
	if err := validateCrossMintFallbackEpoch(e, m, now); err == nil {
		t.Fatal("unknown custody clock admitted")
	}
	transient := errors.New("source timeout")
	c := CrossMintController{marketEvidence: &fallbackEpochSource{err: transient}}
	if _, err := c.loadFallbackEpoch(context.Background(), m); !errors.Is(err, transient) {
		t.Fatal("source outage became deterministic unavailable")
	}
	if errors.Is(transient, ErrCrossMintTargetUnavailable) {
		t.Fatal("transport error authorizes fallback")
	}
}
