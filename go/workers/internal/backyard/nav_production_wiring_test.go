package backyard

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func TestProductionObserverAcquiresScopedQuotesAndPreparesReport(t *testing.T) {
	m := readyWorkerManifest(t)
	m.RuntimeActivation.SelectedLane = PrimeUSDCLaneID
	accounts := viewRouteBatch(t, func(a []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(a, primePRIMEUSDC.CollateralCustody).Data[64:72], 1_000_000_000)
	})
	rpc := budgetBuildRPCWithAccounts(t, 5000, 77, accounts)
	view := fixtureView(t, rpc)
	calls := 0
	client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/quote" {
			t.Fatalf("valuation called execution API %s", r.URL.Path)
		}
		calls++
		return response(fmt.Sprintf(`{"inputMint":%q,"outputMint":%q,"inAmount":%q,"outAmount":"1000000000","otherAmountThreshold":"995000000","swapMode":"ExactIn","slippageBps":50,"contextSlot":77,"routePlan":[{}],"platformFee":null}`, primePRIMEUSDC.Kamino.CollateralMint, fleet.USDCMint, r.URL.Query().Get("amount"))), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	state := &vaultState{exits: newExitQuoter(nil, client, m, nil)}
	o, _, err := ObserveConfirmedRouteSnapshot(context.Background(), rpc, view, m, state, 0, targetInputs{})
	if err != nil || calls != 1 || !o.Snapshot.netKnown(time.Now()) {
		t.Fatalf("production book remained uncertified: calls=%d known=%v err=%v reason=%s", calls, o.Snapshot.netKnown(time.Now()), err, o.Snapshot.NAVUnavailable)
	}
	o.planning = &routePlanningState{inputs: targetInputs{ExitCosts: nil}}
	o.policies = testPolicies(t)
	_, e, err := prepareBridgeFromObservedAccounts(context.Background(), rpc, m, Decision{Action: ReportNAV, StrategyKey: PrimeUSDCLaneID}, o, accounts)
	if err != nil {
		t.Fatal("fresh production quote was lost before report construction", err)
	}
	if _, err := compileBridgeMessageForDelegate(m.vault, e.Request, mustKey(backyardDelegate)); err != nil {
		t.Fatal("certified report did not compile", err)
	}
	o.exitCosts()[PrimeUSDCLaneID].Quote.scope.vault = "mutated caller evidence"
	next, _, err := ObserveConfirmedRouteSnapshot(context.Background(), rpc, view, m, state, 0, targetInputs{})
	if err != nil || !next.Snapshot.netKnown(time.Now()) || calls != 1 {
		t.Fatalf("shared ephemeral quote lost: calls=%d err=%v", calls, err)
	}
	if got := next.exitCosts()[PrimeUSDCLaneID].Quote; got == nil || strconv.FormatUint(got.QuotedCollateralRaw, 10) == "0" {
		t.Fatal("production observation lost exact quantity")
	}
}

func TestBridgeCertificateBindsFinalEconomicRequest(t *testing.T) {
	for _, action := range []Action{VoltrAllocateToSquads, VoltrRestoreIdle, ReportNAV} {
		r := bridgeTestRequest(action, 20)
		if action == ReportNAV {
			r.AmountRaw = 0
			r = bindBridgeRequest(r)
		}
		if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*BridgeBuildRequest){func(r *BridgeBuildRequest) { r.AmountRaw = 50 }, func(r *BridgeBuildRequest) { r.Action = bridgeRestoreIdle }, func(r *BridgeBuildRequest) { r.Policy = backyardSettings }, func(r *BridgeBuildRequest) { r.Report.NAVAfterRaw++ }, func(r *BridgeBuildRequest) { r.LastValidBlockHeight++ }} {
			copied := r
			change(&copied)
			if copied == r {
				continue
			}
			if _, err := compileBridgeMessageForDelegate(rwa, copied, mustKey(backyardDelegate)); err == nil {
				t.Fatal("mutated certified bridge economics produced wire", action)
			}
		}
	}
	r := bridgeTestRequest(StageSquadsToVoltr, 20)
	r.net.vault = looping.squads
	r = bindBridgeRequest(r)
	if err := r.validateNAV(looping, time.Now()); err != nil {
		t.Fatal("Trustful fixture", err)
	}
	r.AmountRaw = 50
	if err := r.validateNAV(looping, time.Now()); err == nil {
		t.Fatal("Trustful stage amount escaped seal")
	}
}

func TestQuoteCandidateWaitsForAccountSlotCatchupWithoutRequoting(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	basis, held, err := readHeldExitBasis(m, accounts, route, 77)
	if err != nil || !held {
		t.Fatal(err)
	}
	calls := 0
	client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		out := strconv.FormatUint(basis.markValue, 10)
		floor := strconv.FormatUint(jupiterOutputFloor(basis.markValue, 50), 10)
		return response(fmt.Sprintf(`{"inputMint":%q,"outputMint":%q,"inAmount":%q,"outAmount":%q,"otherAmountThreshold":%q,"swapMode":"ExactIn","slippageBps":50,"contextSlot":79,"routePlan":[{}],"platformFee":null}`, route.Kamino.CollateralMint, fleet.USDCMint, r.URL.Query().Get("amount"), out, floor)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	q := newExitQuoter(nil, client, m, nil)
	now := time.Now()
	q.now = func() time.Time { return now }
	costs := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
	quoted := costs[route.Lane].Quote
	if calls != 1 || quoted == nil || quoted.Slot != 79 {
		t.Fatal("normal API progression discarded actual quote slot", calls, quoted)
	}
	at := quoted.At
	navAccounts, err := selectRouteNAVAccountsForRoutes(m.vault, accounts, []RuntimeRoute{route})
	if err != nil {
		t.Fatal(err)
	}
	before, err := ComputeRouteNAVForRoutes(77, navAccounts, m, nil, []RuntimeRoute{route}, costs)
	if err != nil || before.net != nil {
		t.Fatal("future quote certified current account book", err)
	}
	now = now.Add(exitQuoteRetryInterval + time.Second)
	waiting := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
	if calls != 1 || waiting[route.Lane].Quote == nil || !waiting[route.Lane].Quote.At.Equal(at) {
		t.Fatal("pending candidate refreshed forward forever", calls)
	}
	caughtUp := q.quote(context.Background(), m, 79, accounts, []string{route.Lane}, nil)
	after, err := ComputeRouteNAVForRoutes(79, navAccounts, m, nil, []RuntimeRoute{route}, caughtUp)
	if err != nil || after.net == nil || calls != 1 {
		t.Fatal("account catchup failed to certify original scoped candidate", err, calls)
	}
	now = now.Add(exitQuoteInterval)
	q.quote(context.Background(), m, 79, accounts, []string{route.Lane}, nil)
	if calls != 2 {
		t.Fatal("candidate exceeded its original lifetime", calls)
	}
}

func TestProductionObserverCatchesUpToExitQuotes(t *testing.T) {
	for _, scenario := range []string{"unchanged holdings", "custody changed", "quotes keep advancing"} {
		t.Run(scenario, func(t *testing.T) {
			m := readyWorkerManifest(t)
			m.RuntimeActivation.SelectedLane = PrimeUSDCLaneID
			bank := int64(77)
			batch := func(slot int64) []ConfirmedAccount {
				return productionRouteBatchAccounts(t, slot, func(a []ConfirmedAccount) {
					cash := uint64(1_000_000_000)
					if scenario == "custody changed" && slot >= 79 {
						cash += 50
					}
					if scenario == "quotes keep advancing" {
						cash += uint64(slot-77) * 50
					}
					binary.LittleEndian.PutUint64(accountAt(a, primePRIMEUSDC.CollateralCustody).Data[64:72], cash)
				})
			}
			quotes, refreshes := 0, 0
			var quoted []uint64
			q := &exitQuoter{now: time.Now, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
				quotes++
				quoted = append(quoted, b.heldRaw)
				quoteSlot := slot
				if quotes == 1 || scenario == "quotes keep advancing" {
					quoteSlot += 2
				}
				return exitCostOf(b.st.route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, quoteSlot, at)
			}}
			o, accounts, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
				state: &vaultState{}, exits: q, now: time.Now,
				read: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, []programAccount, error) {
					bank = max(bank, minSlot)
					return fixtureBatchRuntime(bank, batch(bank))(ctx, addresses, minSlot)
				},
				refreshValuation: func(ctx context.Context, routes []RuntimeRoute, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
					refreshes++
					return fixtureRefresh(bank, batch(bank))(ctx, routes, addresses, minSlot)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "quotes keep advancing" {
				if refreshes != 3 || o.Snapshot.netKnown(time.Now()) || o.Snapshot.NAVUnavailable != exitUnpriced {
					t.Fatal("unbounded catchup or future quote certified", refreshes, o.Snapshot.NAVUnavailable)
				}
				return
			}
			if !o.Snapshot.netKnown(time.Now()) || o.Snapshot.Slot != 79 || refreshes != 2 {
				t.Fatal("fresh quote and coherent recapture did not certify", refreshes, o.Snapshot.Slot, o.Snapshot.NAVUnavailable)
			}
			if scenario == "custody changed" {
				if quotes != 2 || quoted[1] != quoted[0]+50 || o.Snapshot.CollateralIdleRaw != 1_000_000_050 {
					t.Fatal("recapture reused a quote for the old holding", quotes, quoted, o.Snapshot.CollateralIdleRaw)
				}
			} else if quotes != 1 {
				t.Fatal("unchanged holdings were needlessly requoted", quotes)
			}
			for _, account := range accounts {
				if account.ValuationSlot != o.Snapshot.Slot {
					t.Fatal("mixed banks after recapture", account.Address, account.ValuationSlot)
				}
			}
		})
	}
}
