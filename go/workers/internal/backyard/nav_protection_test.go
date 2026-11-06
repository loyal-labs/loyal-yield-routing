package backyard

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestNAVCertificateRejectsUnscopedIncompleteAndAgedEvidence(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	for _, tc := range []struct {
		name   string
		change func(*ExitCost)
	}{
		{"valid-zero-cost", func(c *ExitCost) {}},
		{"expired", func(c *ExitCost) { c.At = time.Now().Add(-exitQuoteInterval - time.Second) }},
		{"future-time", func(c *ExitCost) { c.At = time.Now().Add(time.Hour) }},
		{"future-slot", func(c *ExitCost) { c.Slot = 78 }},
		{"old-slot", func(c *ExitCost) { c.Slot = 1 }},
		{"other-vault", func(c *ExitCost) { c.scope.vault = "other" }},
		{"other-manifest", func(c *ExitCost) { c.scope.manifest = "other" }},
		{"usdc-basis", func(c *ExitCost) { c.scope.usdc[0]++ }},
		{"partial", func(c *ExitCost) { c.QuotedCollateralRaw-- }},
		{"larger", func(c *ExitCost) { c.QuotedCollateralRaw++ }},
		{"persisted", func(c *ExitCost) { c.scope = nil }},
		{"realized", func(c *ExitCost) { c.Source = exitCostRealized }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			costs := fixtureExitCosts(m, 77, accounts)
			q := costs[route.Lane].Quote
			if q == nil {
				t.Fatal("fixture has no exact quote")
			}
			tc.change(q)
			// A cheap historical receipt never replaces missing authoritative coverage.
			realized := *q
			realized.Source = exitCostRealized
			realized.BPS = 0
			costs[route.Lane] = laneExitCosts{Quote: q, Realized: &realized}
			nav, err := ComputeRouteNAVForRoutes(77, accounts, m, nil, []RuntimeRoute{route}, costs)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "valid-zero-cost" {
				if nav.net == nil || nav.ExitLiabilityRaw != 0 {
					t.Fatalf("free exit unknown: %+v", nav)
				}
				return
			}
			if nav.net != nil || nav.NAVUnavailable == "" || nav.PrimeIdleValueRaw == 0 || nav.Custodies.SquadsPRIMEraw == 0 {
				t.Fatalf("unknown lost book or certified bad evidence: %+v", nav)
			}
		})
	}
}

func TestUnknownNAVPreservesMandatoryRiskAndBlocksAddsReports(t *testing.T) {
	s := base()
	s.net = nil
	s.NAVUnavailable = exitUnpriced
	s.HasPosition = true
	s.PositionCollateralRaw = 100
	s.PositionCollateralValueRaw = 100
	s.PositionDebtRaw = 80
	s.PositionDebtValueRaw = 80
	s.LTVBPS = 8000
	if d := Decide(targeted(s)); d.Action != DeleverRouteStep || d.Reason != hardLTVDeleverReason {
		t.Fatal("risk blocked", d)
	}
	s.PositionDebtRaw = 40
	s.PositionDebtValueRaw = 40
	s.LTVBPS = 4000
	s.DebtTermEndUnix = 1
	s.ChainUnix = 2
	if d := Decide(targeted(s)); d.Action != DeleverRouteStep {
		t.Fatal("term reduction blocked", d)
	}
	s = base()
	s.net = nil
	s.NAVUnavailable = exitUnpriced
	s.LastReportAgeSeconds = 100000
	if d := Decide(targeted(s)); d.Action == ReportNAV || d.Action == HoldManualRecovery {
		t.Fatal("unknown became report or insolvency", d)
	}
	s.VoltrIdleRaw = 100
	s.Target = LaneTarget{Rule: ruleEntry, EntryRaw: 10}
	if d := Decide(s); d.Action != Hold || d.Reason != exitUnpriced {
		t.Fatal("uncertified add", d)
	}
}

func TestReportBuilderRequiresCertificateAndPlainStageDoesNot(t *testing.T) {
	r := bridgeTestRequest(ReportNAV, 0)
	r.net = nil
	if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err == nil {
		t.Fatal("numeric report bypass")
	}
	r = bridgeTestRequest(StageSquadsToVoltr, 1)
	r.net = nil
	r.Report = BridgeReport{}
	if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err != nil {
		t.Fatal("plain custody stage requires net", err)
	}
	v := *rwa
	v.Adaptor = looping.Adaptor
	r.AdaptorConfig = v.Strategy
	if _, _, err := trustfulBridgeInstructions(&v, r); err == nil {
		t.Fatal("Trustful stage has numeric bypass")
	}
	r = bridgeTestRequest(ReportNAV, 0)
	r.net.validUntil = time.Now().Add(-time.Second)
	if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err == nil {
		t.Fatal("expired before fresh sign")
	}
}

func TestCertifiedNAVCopiesQuoteEvidence(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	costs := fixtureExitCosts(m, 77, accounts)
	nav, err := ComputeRouteNAVForRoutes(77, accounts, m, nil, []RuntimeRoute{route}, costs)
	if err != nil || nav.net == nil {
		t.Fatal(err)
	}
	q := costs[route.Lane].Quote
	q.scope.vault = "changed"
	q.At = time.Now().Add(-time.Hour)
	q.BPS = 100
	if !nav.net.valid(time.Now(), 77) || nav.net.quotes[0].BPS != 0 {
		t.Fatal("mutable evidence changed certificate")
	}
}

func TestDemandRefreshDoesNotUseHourlyAttemptSuppression(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, attempted: map[string]time.Time{route.Lane: now}, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		calls++
		return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	costs := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
	if calls != 1 || costs[route.Lane].Quote == nil {
		t.Fatal("hourly attempt suppressed demand")
	}
	now = now.Add(exitQuoteInterval + time.Second)
	q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	if calls != 2 {
		t.Fatal("expired certificate did not refresh", calls)
	}
}

func TestSnapshotNumbersCannotBorrowCertificateFromAnotherBook(t *testing.T) {
	s := base()
	s.StrategyNAVRaw = 10_000_000
	s.TotalVaultNAVRaw = 10_000_000
	s.net = fixtureSnapshotCertificate(s)
	if !s.netKnown(time.Now()) {
		t.Fatal("fixture unknown")
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.TotalVaultNAVRaw++ }, func(s *Snapshot) { s.PositionCollateralValueRaw++ }, func(s *Snapshot) { s.SquadsIdleRaw++ }, func(s *Snapshot) { s.RouteLane = onreONycUSDC }} {
		copied := s
		change(&copied)
		if copied.netKnown(time.Now()) {
			t.Fatal("mutated public numeric book retained certificate")
		}
	}
}

func TestSharedCollateralAttributionDoesNotFollowWorkingLane(t *testing.T) {
	m, accounts := multiLaneFixture(t, nil)
	a := fundLane(t, &accounts, autoAUTOPYUSD.Lane, 100_000_000, 20_000_000)
	b := fundLane(t, &accounts, autoAUTOWYLDSLane, 50_000_000, 10_000_000)
	custody := tokenAccountFixture(t, a.CollateralCustody, a.Kamino.CollateralMint, m.vault.squads, 3_000_000)
	custody.Owner = a.CollateralTokenProgram
	upsertConfirmedAccount(&accounts, custody)
	costs := fixtureExitCosts(m, 77, accounts)
	var value uint64
	for i, routes := range [][]RuntimeRoute{{a, b}, {b, a}} {
		navAccounts, err := selectRouteNAVAccountsForRoutes(m.vault, accounts, routes)
		if err != nil {
			t.Fatal(err)
		}
		nav, err := ComputeRouteNAVForRoutes(77, navAccounts, m, nil, routes, costs)
		if err != nil || nav.net == nil {
			t.Fatalf("book %d unavailable %v %+v", i, err, nav)
		}
		if i == 0 {
			value = nav.StrategyNAVRaw
		} else if nav.StrategyNAVRaw != value {
			t.Fatal("working view duplicated custody", value, nav.StrategyNAVRaw)
		}
	}
	qa, qb := costs[a.Lane].Quote, costs[b.Lane].Quote
	if qa == nil || qb == nil || qa.QuotedCollateralRaw+qb.QuotedCollateralRaw != 153_000_000 {
		t.Fatal("shared idle quoted more than once", qa, qb)
	}
}

func TestAddAdmissionExpiresAndBindsFinalRequestEconomics(t *testing.T) {
	m := readyWorkerManifest(t)
	s := base()
	s.net = fixtureSnapshotCertificate(s)
	r := JupiterSwapRequest{Action: SwapStableToCollateralStep, AmountRaw: 1, RouteLane: s.RouteLane}
	bound, err := authorizeAddRequest(r, Decision{Action: r.Action}, s)
	if err != nil {
		t.Fatal(err)
	}
	a, required := requestAddAdmission(bound)
	if !required || validateAddAdmission(m, bound, a, time.Now(), s.Slot) != nil {
		t.Fatal("fresh admission rejected")
	}
	changed := bound.(JupiterSwapRequest)
	changed.AmountRaw++
	if validateAddAdmission(m, changed, a, time.Now(), s.Slot) == nil {
		t.Fatal("request amount bypassed binding")
	}
	if validateAddAdmission(m, bound, a, time.Now().Add(exitQuoteInterval+time.Second), s.Slot) == nil {
		t.Fatal("expired optional add signed")
	}
}

func TestWithdrawalCashReadyStillAcquiresNAVForRestore(t *testing.T) {
	m, accounts := multiLaneFixture(t, nil)
	binary.LittleEndian.PutUint64(accountAt(accounts, m.vault.strategyATA).Data[64:72], 1_000_000)
	address, data := receiptFixture(t, m.vault.VoltrVault, testPublicKey(44), 500, 50<<48)
	quoted := 0
	q := &exitQuoter{now: func() time.Time { return time.Now() }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		quoted++
		return exitCostOf(b.st.route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{state: testVaultState, inputs: targetInputs{}, now: func() time.Time { return time.Now() }, exits: q,
		read: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, []programAccount, error) {
			slot, a, _, err := fixtureBatchRuntime(77, accounts)(ctx, addresses, minSlot)
			return slot, a, []programAccount{{Address: address, Account: ConfirmedAccount{Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: data}}}, err
		}, refreshValuation: fixtureRefresh(77, accounts)})
	if err != nil || quoted == 0 || !o.Snapshot.netKnown(time.Now()) || o.Snapshot.WithdrawalDemandRaw == 0 || !o.Snapshot.HasPosition {
		t.Fatalf("cash-ready withdrawal never repriced remaining held book: quotes=%d err=%v known=%v", quoted, err, o.Snapshot.netKnown(time.Now()))
	}
	// Report-bearing restore now has full remaining-holding evidence available.
	o.policies = testPolicies(t)
	o.planning = &routePlanningState{inputs: targetInputs{ExitCosts: qCostsFromCertificate(o.Snapshot.net)}}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 77, accounts)
	_, e, err := prepareBridgeFromObservedAccounts(context.Background(), rpc, m, Decision{Action: VoltrRestoreIdle, AmountRaw: 1_000_000, StrategyKey: o.Snapshot.RouteLane}, o, accounts)
	if err != nil || e.Request.net == nil {
		t.Fatalf("certified restore did not prepare: %v", err)
	}
}

func qCostsFromCertificate(c *certifiedNAV) exitCosts {
	out := exitCosts{}
	for _, q := range c.quotes {
		copied := q
		out[q.Lane] = laneExitCosts{Quote: &copied}
	}
	return out
}

func TestNAVQuoteRequestsFullHoldingWithoutBuildingExecution(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	basis, held, err := readHeldExitBasis(m, accounts, route, 77)
	if err != nil || !held {
		t.Fatal(err)
	}
	// This holding exceeds the per-leg execution amount; valuation does not construct that wire.
	basis.heldRaw = max(stepLegCap(route).raw, 1_000_000_000_000) * 2
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := prices.collateralMicros(basis.heldRaw)
	if err != nil {
		t.Fatal(err)
	}
	basis.markValue = uint64(mark)
	requests := 0
	client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/quote" || r.URL.Query().Get("amount") != strconv.FormatUint(basis.heldRaw, 10) {
			t.Fatalf("execution or capped quote requested: %s", r.URL.Path)
		}
		return response(fmt.Sprintf(`{"inputMint":%q,"outputMint":%q,"inAmount":%q,"outAmount":%q,"otherAmountThreshold":%q,"swapMode":"ExactIn","slippageBps":50,"contextSlot":77,"routePlan":[{}],"platformFee":null}`, route.Kamino.CollateralMint, fleet.USDCMint, strconv.FormatUint(basis.heldRaw, 10), strconv.FormatUint(basis.markValue, 10), strconv.FormatUint(jupiterOutputFloor(basis.markValue, 50), 10))), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	cost, err := (lanePricer{client: client, manifest: m}).priceHeldExit(context.Background(), basis, 77, time.Now())
	if err != nil || requests != 1 || cost.QuotedCollateralRaw != basis.heldRaw || cost.BPS != 0 {
		t.Fatalf("full holding unavailable: %+v %v requests=%d", cost, err, requests)
	}
}

func TestOrdinaryWithdrawalReductionAcquiresItsCostGateNAV(t *testing.T) {
	m, accounts := multiLaneFixture(t, nil)
	address, data := receiptFixture(t, m.vault.VoltrVault, testPublicKey(44), 500, 50<<48)
	quoted := 0
	q := &exitQuoter{now: func() time.Time { return time.Now() }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		quoted++
		return exitCostOf(b.st.route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{state: testVaultState, inputs: targetInputs{}, now: func() time.Time { return time.Now() }, exits: q, read: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, []programAccount, error) {
		slot, a, _, err := fixtureBatchRuntime(77, accounts)(ctx, addresses, minSlot)
		return slot, a, []programAccount{{Address: address, Account: ConfirmedAccount{Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: data}}}, err
	}, refreshValuation: fixtureRefresh(77, accounts)})
	if err != nil || quoted == 0 || !o.Snapshot.netKnown(time.Now()) {
		t.Fatalf("ordinary withdrawal never acquired cost-gate NAV: %d %v", quoted, err)
	}
	d := Decision{Action: DeleverRouteStep, Reason: releaseReason, StrategyKey: o.Snapshot.RouteLane}
	if !ordinaryWithdrawalCostRequired(o.Snapshot, d) {
		t.Fatal("fixture missed production ordinary-cost classification", o.Snapshot.Target)
	}
}
