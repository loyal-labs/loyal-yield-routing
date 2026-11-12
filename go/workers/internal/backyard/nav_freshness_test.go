package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func TestExitQuoteRefreshKeepsNAVValidThroughPreparation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		age           time.Duration
		slot, buildAt int64
	}{
		{"slot-expiry", 7 * time.Second, 108, 110},
		{"time-expiry", exitQuoteInterval - time.Second, 77, 78},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := readyWorkerManifest(t)
			route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
			accounts = append(accounts, clockFixture())
			now := time.Now().Add(-tc.age)
			q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
				return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
			}}
			costs := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
			old := *costs[route.Lane].Quote
			now = now.Add(tc.age)
			buildTime := now.Add(2 * time.Second)
			if !old.validAt(now, tc.slot) || old.validAt(buildTime, tc.buildAt) {
				t.Fatal("fixture must be valid at observation and expired at build")
			}
			costs = q.quote(context.Background(), m, tc.slot, accounts, []string{route.Lane}, costs)
			navAccounts, err := selectRouteNAVAccountsForRoutes(m.vault, accounts, []RuntimeRoute{route})
			if err != nil {
				t.Fatal(err)
			}
			nav, err := ComputeRouteNAVForRoutes(tc.slot, navAccounts, m, nil, []RuntimeRoute{route}, costs)
			if err != nil || nav.net == nil || !nav.net.valid(buildTime, tc.buildAt) {
				t.Fatalf("reused a quote that expires during preparation: %v", err)
			}
			request := bridgeTestRequest(ReportNAV, 0)
			request.net, request.Report = nav.net, nav.Report
			request = bindBridgeRequest(request)
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
			delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
			signed, err := buildAndSignBridgeTransactionForDelegate(request, key, delegate)
			if err != nil {
				t.Fatal("refreshed report cannot sign", err)
			}
			result, err := signed.BuildResult(tc.buildAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.validateForDelegate(delegate); err != nil {
				t.Fatal("refreshed report fails exact-wire persistence validation", err)
			}
		})
	}
}

func TestPublishedNAVStopsAtOldestQuoteSlot(t *testing.T) {
	db := withdrawalCostTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	costs := fixtureExitCosts(m, 77, accounts)
	nav, err := ComputeRouteNAVForRoutes(90, accounts, m, nil, []RuntimeRoute{route}, costs)
	if err != nil || nav.net == nil {
		t.Fatalf("fixture must certify an older, still valid quote: %v", err)
	}
	o := Observation{Snapshot: Snapshot{Slot: 90, Fresh: true, TotalVaultNAVRaw: int64(nav.TotalVaultNAVRaw), net: nav.net}, routeBatch: &routeObservationBatch{Accounts: accounts}}
	bindSnapshotNAV(&o.Snapshot)
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_certified_nav WHERE vault=$1`, m.vault.VoltrVault); err != nil {
		t.Fatal(err)
	}
	if err := db.recordNAVEvidence(ctx, m.vault, o); err != nil {
		t.Fatal(err)
	}
	var certified, maximum int64
	if err := db.pool.QueryRow(ctx, `SELECT certified_slot,max_slot FROM loyal_yield.backyard_certified_nav WHERE vault=$1`, m.vault.VoltrVault).Scan(&certified, &maximum); err != nil {
		t.Fatal(err)
	}
	if certified != 90 || maximum != 109 || !nav.net.valid(time.Now(), maximum) || nav.net.valid(time.Now(), maximum+1) {
		t.Fatalf("published validity exceeds underlying quote: certified=%d max=%d", certified, maximum)
	}
}
