package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func enableInvestmentExit(m RouteManifest) RouteManifest {
	v := *m.vault
	v.CheckInvestmentExits = true
	m.vault = &v
	return m
}

func investmentQuoteFixture(t *testing.T, client *jupiter.Client, quote func(*http.Request, uint64) (uint64, uint64, error)) {
	t.Helper()
	base := fixtureHTTP(client).Transport
	fixtureHTTP(client).Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/quote" || r.URL.Query().Get("slippageBps") != "20" {
			return base.RoundTrip(r)
		}
		amount, err := strconv.ParseUint(r.URL.Query().Get("amount"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out, slot, err := quote(r, amount)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(jupiter.Quote{InputMint: r.URL.Query().Get("inputMint"), OutputMint: r.URL.Query().Get("outputMint"), InAmount: strconv.FormatUint(amount, 10), OutAmount: strconv.FormatUint(out, 10), OtherAmountThreshold: strconv.FormatUint(jupiterOutputFloor(out, 20), 10), SwapMode: "ExactIn", SlippageBPS: 20, ContextSlot: slot, RoutePlan: investmentTestPath(r.URL.Query().Get("inputMint"), r.URL.Query().Get("outputMint"))})
		if err != nil {
			t.Fatal(err)
		}
		return response(string(raw)), nil
	})
}

func TestInvestmentExitProjectsWholeHoldingAndRepaysDebt(t *testing.T) {
	for _, size := range []uint64{10_000_000_000, 50_000_000_000, 100_000_000_000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			route, _ := runtimeRoute(MapleSyrupUSDCLaneID)
			m, _, client, accounts := selectorDestinationFixtureForLane(t, route.Lane, func(accounts []ConfirmedAccount) {
				o := accountAt(accounts, route.Kamino.Obligation)
				putKey(t, o.Data[96:128], route.Kamino.CollateralReserve)
				binary.LittleEndian.PutUint64(o.Data[128:136], size)
				putKey(t, o.Data[1208:1240], route.Kamino.DebtReserve)
				putScaledFraction(o.Data[1240:1272], new(big.Int).Lsh(big.NewInt(1), 60))
				putScaledFraction(o.Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(size/3), 60))
				binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 123)
			})
			m = enableInvestmentExit(m)
			state, err := readExitStepState(m.vault, accounts, route, 42)
			if err != nil {
				t.Fatal(err)
			}
			held, err := state.collateral.redeemLiquidityRaw(size)
			if err != nil {
				t.Fatal(err)
			}
			added, debtAdded := uint64(1_000_000_000), uint64(500_000_000)
			requested := uint64(0)
			investmentQuoteFixture(t, client, func(r *http.Request, amount uint64) (uint64, uint64, error) {
				requested = max(requested, amount)
				return amount, 42, nil
			})
			before := hashConfirmedAccounts(accounts)
			r, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, added, debtAdded)
			if err != nil {
				t.Fatal(err)
			}
			if requested != held+123+added || r.CollateralRaw != requested || r.RepaymentUSDCRaw < state.position.DebtRaw+debtAdded || r.MinimumEquityRaw != investmentExitFloor(r.ReferenceUSDCRaw)-r.RepaymentUSDCRaw || hashConfirmedAccounts(accounts) != before {
				t.Fatalf("incomplete projected holding/debt: %+v requested=%d held=%d", r, requested, held)
			}
		})
	}
}

func TestInvestmentExitRejectsBadPricesAndUnknownQuotes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output uint64
		slot   uint64
		age    time.Duration
		fail   bool
		want   string
	}{
		{"boundary", 998_000_000, 42, 0, false, ""},
		{"one_unit_under", 997_999_999, 42, 0, false, investmentExitExpensive},
		{"old_slot", 1_000_000_000, 1, 0, false, investmentExitUnavailable},
		{"future_slot", 1_000_000_000, 1000, 0, false, investmentExitUnavailable},
		{"missing_slot", 1_000_000_000, 0, 0, false, investmentExitUnavailable},
		{"expired", 1_000_000_000, 42, time.Minute, false, investmentExitUnavailable},
		{"no_route", 1_000_000_000, 42, 0, true, investmentExitUnavailable},
		{"zero_output", 0, 42, 0, false, investmentExitUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, client, accounts := selectorDestinationFixture(t)
			route, _ := m.route(MapleSyrupUSDCLaneID)
			investmentQuoteFixture(t, client, func(*http.Request, uint64) (uint64, uint64, error) {
				if tc.fail {
					return 0, 0, errors.New("route unavailable")
				}
				return tc.output, tc.slot, nil
			})
			o := flatEntryObservation(42, 1_000_000_000)
			o.ObservedAt = o.ObservedAt.Add(-tc.age)
			_, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), o, route, accounts, 42, 1_000_000_000, 0)
			assertInvestmentHold(t, err, tc.want)
		})
	}
}

func assertInvestmentHold(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("%v (cause: %v)", err, errors.Unwrap(err))
		}
		return
	}
	var hold *PreSendHold
	if !errors.As(err, &hold) || holdCause(hold) != want {
		t.Fatalf("want %s, got %v (cause: %v)", want, err, errors.Unwrap(err))
	}
}

func TestInvestmentExitLadderBuysLessWithoutChangingCostBudget(t *testing.T) {
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	m = enableInvestmentExit(m)
	p := testLanePricer(t, rpc, client, m, 42)
	var amounts []uint64
	investmentQuoteFixture(t, client, func(r *http.Request, amount uint64) (uint64, uint64, error) {
		amounts = append(amounts, amount)
		if amount > 1_000_000 {
			return amount * 997 / 1000, 42, nil
		}
		return amount, 42, nil
	})
	policy.UncertaintyBPS = 0
	quotes, reason := p.ladder(context.Background(), o, []LaneEconomics{market}, policy, market, nil, 10_000_000, true)
	if len(quotes) != 1 || quotes[0].EquityRaw != 1_000_000 || quotes[0].InvestmentExit == nil || len(amounts) != 2 || amounts[0] <= amounts[1] {
		t.Fatalf("did not reduce purchase: quotes=%+v amounts=%v reason=%s", quotes, amounts, reason)
	}
	plain := *m.vault
	plain.CheckInvestmentExits = false
	p.manifest.vault = &plain
	original, err := p.priceEntry(context.Background(), o, market, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if original.CostRaw != quotes[0].CostRaw || original.Cost != quotes[0].Cost {
		t.Fatal("exit check changed the profitability budget")
	}
}

func TestInvestmentExitLadderDoesNotTreatOutageAsCheapSmallerTrade(t *testing.T) {
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	m = enableInvestmentExit(m)
	calls := 0
	investmentQuoteFixture(t, client, func(*http.Request, uint64) (uint64, uint64, error) {
		calls++
		return 0, 0, errors.New("quote service unavailable")
	})
	q, _ := testLanePricer(t, rpc, client, m, 42).ladder(context.Background(), o, []LaneEconomics{market}, policy, market, nil, 10_000_000, true)
	if len(q) != 0 || calls != 1 {
		t.Fatalf("unknown exit admitted or retried: %d quotes %d calls", len(q), calls)
	}
}

func TestInvestmentExitDoesNotControlWithdrawalsOrExistingPositions(t *testing.T) {
	m := enableInvestmentExit(readyWorkerManifest(t))
	for _, d := range []Decision{
		{Action: SwapCollateralToStableStep, StrategyKey: onreONycUSDC},
		{Action: SwapCollateralToDebtStep, StrategyKey: onreONycUSDC},
		{Action: DeleverRouteStep, Reason: hardLTVDeleverReason, StrategyKey: onreONycUSDC},
		{Action: OpenRouteStep, Reason: termKeepAliveReason, StrategyKey: onreONycUSDC},
		{Action: OpenRouteStep, StrategyKey: onreONycUSDC},
		{Action: ReportNAV},
	} {
		if err := (lanePricer{manifest: m}).protectInvestment(context.Background(), d, Observation{}, nil); err != nil {
			t.Fatalf("existing activity blocked: %+v %v", d, err)
		}
	}
	for _, a := range [][2]uint64{{math.MaxInt64, 1}, {math.MaxUint64, 0}} {
		if _, err := investmentSum(a[0], a[1]); err == nil {
			t.Fatal("overflow accepted")
		}
	}
}

func TestInvestmentExitPreparationRechecksAfterRestartAndPreservesRecovery(t *testing.T) {
	o, d, e, m, _, _, _ := entrySwapAdmissionFixture(t)
	m = enableInvestmentExit(m)
	o.Snapshot.net = fixtureSnapshotCertificate(o.Snapshot)
	checks := 0
	for restart := 0; restart < 2; restart++ {
		worker := &Worker{manifest: m, runtime: tickRuntime{
			prepareJupiter: func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error) {
				return o, e, nil
			},
			protectInvestment: func(_ context.Context, got Decision, _ Observation, request any) error {
				checks++
				if got != d {
					t.Fatal("changed entry intent")
				}
				if _, ok := request.(JupiterSwapRequest); !ok {
					t.Fatalf("wrong request %T", request)
				}
				return staleHold(investmentExitExpensive)
			},
		}}
		_, _, _, err := worker.prepare(context.Background(), d, o)
		assertInvestmentHold(t, err, investmentExitExpensive)
	}
	if checks != 2 {
		t.Fatal("restart reused exit approval")
	}
	worker := &Worker{manifest: m, runtime: tickRuntime{
		prepareJupiter: func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error) {
			return o, e, nil
		},
	}}
	_, _, _, err := worker.prepare(context.Background(), d, o)
	assertInvestmentHold(t, err, investmentExitUnavailable)
	journal := loadRestartJournal(t)
	advanced := false
	worker.runtime = tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			op := journalOperation(journal, Signed)
			return &op, nil
		},
		advance: func(context.Context, PersistedOperation) error { advanced = true; return nil },
		protectInvestment: func(context.Context, Decision, Observation, any) error {
			t.Fatal("new-entry check interrupted signed recovery")
			return nil
		},
		observe: func(context.Context) (Observation, error) {
			t.Fatal("recovery opened a fresh observation")
			return Observation{}, nil
		},
	}
	if err := worker.Tick(context.Background()); err != nil || !advanced {
		t.Fatalf("signed recovery stopped: %v", err)
	}
}

func TestInvestmentExitIncludesActualBorrowPurchaseInSelector(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	m = enableInvestmentExit(m)
	investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) { return amount, 42, nil })
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(MapleSyrupUSDCLaneID), 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if q.InvestmentExit == nil || q.BorrowReceiveRaw == 0 || q.InvestmentExit.CollateralRaw != 100_000_000+q.BorrowReceiveRaw || q.InvestmentExit.RepaymentUSDCRaw < q.BorrowReceiveRaw+q.BorrowFeeRaw {
		t.Fatalf("borrowed purchase omitted: %+v exit=%+v", q, q.InvestmentExit)
	}
}

func TestInvestmentExitHoldsWhenSharedCollateralLaneIsMissing(t *testing.T) {
	m, route, accounts := autoPayoffBatch(t, 42, nil)
	accounts = append(accounts, autoPayoffMints(t, route)...)
	_, _, client, _ := selectorDestinationFixture(t)
	o := flatEntryObservation(42, 1_000_000_000)
	sibling, ok := overflowLaneOf(route.Lane)
	if !ok {
		t.Fatal("fixture has no shared-collateral overflow")
	}
	o.Snapshot.HeldLanes = route.Lane + "," + sibling
	investmentQuoteFixture(t, client, func(*http.Request, uint64) (uint64, uint64, error) {
		t.Fatal("incomplete holdings reached quote service")
		return 0, 0, nil
	})
	_, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), o, route, accounts, 42, 1_000_000_000, 0)
	assertInvestmentHold(t, err, investmentExitUnavailable)
}

func TestInvestmentExitPricesNonUSDCRepayment(t *testing.T) {
	for _, expensive := range []bool{false, true} {
		t.Run(fmt.Sprint(expensive), func(t *testing.T) {
			m, route, accounts := autoPayoffBatch(t, 42, nil)
			accounts = append(accounts, autoPayoffMints(t, route)...)
			prices, err := batchLanePrices(accounts, route)
			if err != nil {
				t.Fatal(err)
			}
			_, _, client, _ := selectorDestinationFixture(t)
			loanQuotes := 0
			investmentQuoteFixture(t, client, func(req *http.Request, amount uint64) (uint64, uint64, error) {
				if req.URL.Query().Get("outputMint") == route.Kamino.DebtMint {
					loanQuotes++
					out, err := valueBetweenTokenRaw(amount, prices.collateralDecimals, prices.debtDecimals, prices.collateral, prices.debt, false)
					if expensive {
						out = out * 99 / 100
					}
					return out, 42, err
				}
				out, err := valueBetweenTokenRaw(amount, prices.debtDecimals, 6, prices.debt, prices.usdc, false)
				return out, 42, err
			})
			r, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, 10_000_000_000, 5_000_000)
			want := ""
			if expensive {
				want = investmentExitExpensive
			}
			assertInvestmentHold(t, err, want)
			if loanQuotes < 1 || !expensive && (r.RepaymentUSDCRaw == 0 || r.MinimumEquityRaw == 0) {
				t.Fatalf("loan conversion omitted: %+v calls=%d", r, loanQuotes)
			}
		})
	}
}

func TestInvestmentExitCountsSharedCollateralCustodyOnce(t *testing.T) {
	m, route, accounts := autoPayoffBatch(t, 77, nil)
	accounts = append(accounts, autoPayoffMints(t, route)...)
	siblingID, _ := overflowLaneOf(route.Lane)
	sibling, err := m.route(siblingID)
	if err != nil {
		t.Fatal(err)
	}
	otherDebt := accountAt(accounts, route.Kamino.DebtReserve)
	otherDebt.Address, otherDebt.Data = sibling.Kamino.DebtReserve, append([]byte(nil), otherDebt.Data...)
	putKey(t, otherDebt.Data[128:160], sibling.Kamino.DebtMint)
	upsertConfirmedAccount(&accounts, otherDebt)
	upsertConfirmedAccount(&accounts, kaminoObligationImage(t, route, 77, 10_000_000, 0))
	upsertConfirmedAccount(&accounts, kaminoObligationImage(t, sibling, 77, 20_000_000, 0))
	a, err := readExitStepState(m.vault, accounts, route, 77)
	if err != nil {
		t.Fatal(err)
	}
	b, err := readExitStepState(m.vault, accounts, sibling, 77)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := a.collateral.redeemLiquidityRaw(a.position.CollateralDepositedRaw)
	second, _ := b.collateral.redeemLiquidityRaw(b.position.CollateralDepositedRaw)
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	_, _, client, _ := selectorDestinationFixture(t)
	requested := uint64(0)
	investmentQuoteFixture(t, client, func(req *http.Request, amount uint64) (uint64, uint64, error) {
		requested = max(requested, amount)
		out, err := valueBetweenTokenRaw(amount, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
		return out, 77, err
	})
	o := flatEntryObservation(77, 1_000_000_000)
	o.Snapshot.HeldLanes = route.Lane + "," + sibling.Lane
	o.routeBatch = &routeObservationBatch{Slot: 77, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
	r, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), o, route, accounts, 77, 10_000_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if requested != first+second+a.collateralBalance+10_000_000_000 || r.CollateralRaw != requested {
		t.Fatalf("shared holding miscounted: %+v first=%d second=%d custody=%d", r, first, second, a.collateralBalance)
	}
}

func TestInvestmentExitUsesHastraRepaymentPriceAndRejectsStaleness(t *testing.T) {
	for _, stale := range []bool{false, true} {
		at := int64(kaminoFixtureUnix - 60)
		if stale {
			at -= 86400 * 10
		}
		p := lanePricer{manifest: readyWorkerManifest(t), view: hastraTestView(42, hastraPriceAccount(hastraTestPrice, at), clockFixture())}
		out, err := p.investmentExitQuote(context.Background(), autoAUTOWYLDSLane, SwapCollateralToDebtStep, 1_000_000, 42)
		if stale {
			assertInvestmentHold(t, err, investmentExitUnavailable)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := new(big.Int).Mul(big.NewInt(1_000_000), hastraTestPrice)
		want.Quo(want, big.NewInt(1_000_000_000_000_000_000))
		if out != want.Uint64() {
			t.Fatalf("Hastra repayment mispriced: %d vs %s", out, want)
		}
	}
}

func investmentTestPath(input, output string) []json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"percent": 100, "swapInfo": map[string]string{"ammKey": input + ":" + output, "inputMint": input, "outputMint": output}})
	return []json.RawMessage{raw}
}

func TestInvestmentExitRejectsExpensiveLastPortionDespiteGoodAverage(t *testing.T) {
	m, _, client, accounts := selectorDestinationFixture(t)
	route, _ := m.route(MapleSyrupUSDCLaneID)
	investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) {
		if amount == 100_000_000_000 {
			return amount - 50_000_000, 42, nil
		}
		return amount, 42, nil
	})
	r, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, 100_000_000_000, 0)
	assertInvestmentHold(t, err, investmentExitExpensive)
	if r.QuotedUSDCRaw < investmentExitFloor(r.ReferenceUSDCRaw) || r.LastPortionUSDCRaw >= investmentExitFloor(r.LastPortionReferenceRaw) {
		t.Fatalf("expected acceptable average but expensive tail: %+v", r)
	}
}

func TestInvestmentExitRepaysPYUSDBeforePricingDollarConversion(t *testing.T) {
	m, route, accounts := autoPayoffBatch(t, 42, nil)
	accounts = append(accounts, autoPayoffMints(t, route)...)
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	_, _, client, _ := selectorDestinationFixture(t)
	var fullDebtOut, largestCashInput uint64
	investmentQuoteFixture(t, client, func(req *http.Request, amount uint64) (uint64, uint64, error) {
		if req.URL.Query().Get("inputMint") == route.Kamino.DebtMint {
			largestCashInput = max(largestCashInput, amount)
			out, err := valueBetweenTokenRaw(amount, prices.debtDecimals, 6, prices.debt, prices.usdc, false)
			return out, 42, err
		}
		if req.URL.Query().Get("outputMint") != route.Kamino.DebtMint {
			t.Fatal("sent borrowed PYUSD through dollar conversion")
		}
		out, err := valueBetweenTokenRaw(amount, prices.collateralDecimals, prices.debtDecimals, prices.collateral, prices.debt, false)
		out = out * 999 / 1000
		fullDebtOut = max(fullDebtOut, out)
		return out, 42, err
	})
	r, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, 100_000_000_000, 50_000_000)
	assertInvestmentHold(t, err, "")
	if largestCashInput == 0 || largestCashInput >= fullDebtOut || r.CashAfterRepaymentRaw != r.QuotedUSDCRaw-r.RepaymentUSDCRaw {
		t.Fatalf("wrong repayment order: %+v converted=%d full=%d", r, largestCashInput, fullDebtOut)
	}
}

func TestInvestmentExitRequiresComparableIndependentRoutes(t *testing.T) {
	path := investmentQuotePath(jupiter.Quote{InputMint: "a", OutputMint: "b", RoutePlan: investmentTestPath("a", "b")})
	if len(path) != 1 || investmentPathsDisjoint(path, path) || investmentPathsDisjoint(nil, path) {
		t.Fatal("unsafe overlapping/unknown route accepted")
	}
	if p := investmentQuotePath(jupiter.Quote{InputMint: "wrong", OutputMint: "b", RoutePlan: investmentTestPath("a", "b")}); p != nil {
		t.Fatal("disconnected route accepted")
	}
	raw := json.RawMessage(`{"percent":50,"swapInfo":{"ammKey":"pool","inputMint":"a","outputMint":"b"}}`)
	if p := investmentQuotePath(jupiter.Quote{InputMint: "a", OutputMint: "b", RoutePlan: []json.RawMessage{raw}}); p != nil {
		t.Fatal("split route treated as comparable")
	}
}

func TestInvestmentExitDoesNotConfuseBankChangesWithPriceImpact(t *testing.T) {
	m, _, client, accounts := selectorDestinationFixture(t)
	route, _ := m.route(MapleSyrupUSDCLaneID)
	investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) {
		slot := uint64(42)
		if amount < 100_000_000_000 {
			slot = 43
		}
		return amount, slot, nil
	})
	_, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, 100_000_000_000, 0)
	assertInvestmentHold(t, err, investmentExitSlotsChanged)
}

func TestInvestmentExitNormalizesLiveSplitQuotes(t *testing.T) {
	var samples []struct {
		Lane   string          `json:"lane"`
		Size   int             `json:"size"`
		Quotes []jupiter.Quote `json:"quotes"`
	}
	raw, err := os.ReadFile("testdata/investment_exit_quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		a, b := investmentQuotePath(sample.Quotes[0]), investmentQuotePath(sample.Quotes[1])
		if len(a) == 0 || !slices.Equal(a, b) {
			t.Fatalf("equivalent live routing rejected: %s %d: %+v vs %+v", sample.Lane, sample.Size, a, b)
		}
	}
}

func TestInvestmentExitRejectsDisconnectedCyclicOrIncompleteRouting(t *testing.T) {
	for _, plan := range []string{
		`[{"bps":10000,"swapInfo":{"ammKey":"p","inputMint":"a","outputMint":"b"}},{"bps":10000,"swapInfo":{"ammKey":"q","inputMint":"x","outputMint":"y"}}]`,
		`[{"bps":10000,"swapInfo":{"ammKey":"p","inputMint":"a","outputMint":"c"}},{"bps":5000,"swapInfo":{"ammKey":"q","inputMint":"c","outputMint":"a"}},{"bps":5000,"swapInfo":{"ammKey":"r","inputMint":"c","outputMint":"b"}}]`,
		`[{"bps":9900,"swapInfo":{"ammKey":"p","inputMint":"a","outputMint":"b"}}]`,
		`[{"bps":10000,"swapInfo":{"ammKey":"p","inputMint":"a","outputMint":"c"}},{"bps":10000,"swapInfo":{"ammKey":"p","inputMint":"c","outputMint":"b"}}]`,
	} {
		q := jupiter.Quote{InputMint: "a", OutputMint: "b"}
		if err := json.Unmarshal([]byte(plan), &q.RoutePlan); err != nil {
			t.Fatal(err)
		}
		if investmentQuotePath(q) != nil {
			t.Fatalf("invalid route accepted: %s", plan)
		}
	}
}

func TestInvestmentExitRetriesOneChangedSlotWithoutWeakeningPrices(t *testing.T) {
	for _, expensive := range []bool{false, true} {
		m, _, client, accounts := selectorDestinationFixture(t)
		route, _ := m.route(MapleSyrupUSDCLaneID)
		calls := 0
		investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) {
			calls++
			slot := uint64(43)
			if calls == 1 {
				slot = 42
			}
			if expensive && calls == 3 {
				return amount * 997 / 1000, slot, nil
			}
			return amount, slot, nil
		})
		_, err := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), flatEntryObservation(42, 1_000_000_000), route, accounts, 42, 100_000_000_000, 0)
		want := ""
		if expensive {
			want = investmentExitExpensive
		}
		assertInvestmentHold(t, err, want)
		if calls != 4 {
			t.Fatalf("slot retry unbounded or missing: %d", calls)
		}
	}
}

func TestInvestmentExitPreparedWireRechecksAndBindsApproval(t *testing.T) {
	if !productionRWAInvestmentExit || !productionLoopingInvestmentExit {
		t.Fatal("investment protection must be enabled for both deployed vaults")
	}
	m, _, client, accounts := selectorDestinationFixture(t)
	m = enableInvestmentExit(m)
	o, d := executionFixture(t, m.vault)
	o.Snapshot.Target = LaneTarget{Rule: ruleEntry}
	o.Snapshot.WithdrawalDemandRaw = 0
	o.Snapshot.CollateralIdleRaw, o.Snapshot.CollateralIdleValueRaw = 0, 0
	d.Action, d.Reason, d.AmountRaw = SwapStableToCollateralStep, "entry_swap", 1_000_000_000
	e := JupiterExecutionEvidence{Request: executionSwap(t, m.vault, d.Action, uint64(d.AmountRaw), uint64(d.AmountRaw))}
	route, err := m.route(d.StrategyKey)
	if err != nil {
		t.Fatal(err)
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	o.ObservedAt = time.Now()
	o.Snapshot.net = fixtureSnapshotCertificate(o.Snapshot)
	o.routeBatch = &routeObservationBatch{Slot: o.Snapshot.Slot, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
	expensive := false
	investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) {
		out, err := valueBetweenTokenRaw(amount, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
		if expensive {
			out = out * 9900 / 10000
		}
		return out, uint64(o.Snapshot.Slot), err
	})
	worker := &Worker{manifest: m, runtime: tickRuntime{
		prepareJupiter: func(_ context.Context, _ RouteManifest, _ Decision, current Observation) (Observation, JupiterExecutionEvidence, error) {
			return current, e, nil
		},
		protectInvestment: (lanePricer{client: client, manifest: m}).protectInvestment,
	}}
	request, _, _, err := worker.prepare(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	guarded := request.(JupiterSwapRequest)
	if _, _, _, err := compileRequest(m, guarded); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{62}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	message, err := compileJupiterMessageForDelegate(m.vault, guarded, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(key, delegate, message, guarded.RecentBlockhash, guarded.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	built, err := signed.BuildResult(o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if err = built.validateForDelegate(delegate); err != nil {
		t.Fatal(err)
	}
	changed := guarded
	changed.QuotedOutputRaw++
	if _, _, _, err = compileRequest(m, changed); err == nil {
		t.Fatal("approval survived changed purchase economics")
	}
	expensive = true
	_, _, _, err = worker.prepare(context.Background(), d, o)
	assertInvestmentHold(t, err, investmentExitExpensive)
	expensive = false
	stale := o
	stale.ObservedAt = time.Now().Add(-2 * exitQuoteInterval)
	_, _, _, err = worker.prepare(context.Background(), d, stale)
	assertInvestmentHold(t, err, investmentExitUnavailable)
	guarded.admission.net.validUntil = time.Now().Add(-time.Second)
	if _, _, _, err = compileRequest(m, guarded); err == nil {
		t.Fatal("approval survived delay before signing")
	}
}

func TestInvestmentExitRepaysBothAUTOAssetsWithoutSellingWYLDS(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m, route, accounts := autoPayoffBatch(t, 77, nil)
			accounts = append(accounts, autoPayoffMints(t, route)...)
			siblingID, _ := overflowLaneOf(route.Lane)
			sibling, err := m.route(siblingID)
			if err != nil {
				t.Fatal(err)
			}
			otherDebt := accountAt(accounts, route.Kamino.DebtReserve)
			otherDebt.Address, otherDebt.Data = sibling.Kamino.DebtReserve, append([]byte(nil), otherDebt.Data...)
			putKey(t, otherDebt.Data[128:160], sibling.Kamino.DebtMint)
			upsertConfirmedAccount(&accounts, otherDebt)
			mint := accountAt(accounts, route.Kamino.DebtMint)
			mint.Address = sibling.Kamino.DebtMint
			upsertConfirmedAccount(&accounts, mint)
			pyusdDebt := uint64(0)
			if mixed {
				pyusdDebt = 1_000_000
			}
			upsertConfirmedAccount(&accounts, kaminoObligationImage(t, route, 77, 10_000_000, pyusdDebt))
			upsertConfirmedAccount(&accounts, kaminoObligationImage(t, sibling, 77, 20_000_000, 2_000_000))
			prices, err := batchLanePrices(accounts, sibling)
			if err != nil {
				t.Fatal(err)
			}
			rateRaw, err := valueBetweenTokenRaw(1_000_000_000_000_000_000, prices.collateralDecimals, prices.debtDecimals, prices.collateral, prices.debt, false)
			if err != nil {
				t.Fatal(err)
			}
			rate := new(big.Int).SetUint64(rateRaw)
			view := hastraTestView(77, hastraPriceAccount(rate, kaminoFixtureUnix-60), clockFixture())
			_, _, client, _ := selectorDestinationFixture(t)
			investmentQuoteFixture(t, client, func(req *http.Request, amount uint64) (uint64, uint64, error) {
				input := req.URL.Query().Get("inputMint")
				if input == hastra.WYLDSMint.String() || req.URL.Query().Get("outputMint") == hastra.WYLDSMint.String() {
					t.Fatal("wYLDS must use exact redemption, never a market sale")
				}
				if input == route.Kamino.CollateralMint {
					output, err := valueBetweenTokenRaw(amount, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
					return output, 77, err
				}
				return amount, 77, nil
			})
			o := flatEntryObservation(77, 1_000_000_000)
			o.Snapshot.HeldLanes = route.Lane + "," + sibling.Lane
			o.routeBatch = &routeObservationBatch{Slot: 77, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
			before := hashConfirmedAccounts(accounts)
			result, err := (lanePricer{client: client, view: view, manifest: m}).priceInvestmentExit(context.Background(), o, route, accounts, 77, 1_000_000_000, 0)
			if err != nil {
				t.Fatal(err)
			}
			if result.RepaymentUSDCRaw < pyusdDebt+2_000_000 || result.CashAfterRepaymentRaw == 0 || result.QuotedUSDCRaw < investmentExitFloor(result.ReferenceUSDCRaw) || hashConfirmedAccounts(accounts) != before {
				t.Fatalf("mixed debt lost value or changed accounts: %+v", result)
			}
		})
	}
}

func TestInvestmentExitOrdinaryBorrowCompilesOnlyAfterRecheck(t *testing.T) {
	m, _, client, accounts := selectorDestinationFixture(t)
	m = enableInvestmentExit(m)
	o, d := executionFixture(t, m.vault)
	o.routeBatch.Accounts = accounts
	o.Snapshot.Target = LaneTarget{Rule: ruleEntry}
	o.Snapshot.WithdrawalDemandRaw = 0
	route, _ := m.route(MapleSyrupUSDCLaneID)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 5_000_000_000)
	o.Snapshot.net = fixtureSnapshotCertificate(o.Snapshot)
	d.Action, d.Reason = OpenRouteStep, leverageUpReason
	step, _, err := stepTestBuild(t, route.Lane, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	expensive := false
	investmentQuoteFixture(t, client, func(_ *http.Request, amount uint64) (uint64, uint64, error) {
		if expensive {
			return amount * 99 / 100, 42, nil
		}
		return amount, 42, nil
	})
	worker := &Worker{manifest: m, runtime: tickRuntime{
		prepareStep: func(_ context.Context, _ RouteManifest, _ Decision, current Observation) (Observation, StepExecutionEvidence, error) {
			return current, StepExecutionEvidence{Request: step}, nil
		},
		protectInvestment: (lanePricer{client: client, manifest: m}).protectInvestment,
	}}
	request, _, _, err := worker.prepare(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = compileRequest(m, request); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{63}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	got := request.(StepRequest)
	message, err := compileStepMessageForDelegate(m.vault, got, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(key, delegate, message, got.Open.RecentBlockhash, got.Open.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	built, err := signed.BuildResult(o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if err = built.validateForDelegate(delegate); err != nil {
		t.Fatal(err)
	}
	expensive = true
	_, _, _, err = worker.prepare(context.Background(), d, o)
	assertInvestmentHold(t, err, investmentExitExpensive)
}

func TestInvestmentExitUsesExactAllocationWhenPercentIsRounded(t *testing.T) {
	q := jupiter.Quote{InputMint: "a", OutputMint: "b", RoutePlan: []json.RawMessage{
		json.RawMessage(`{"percent":33,"bps":3333,"swapInfo":{"ammKey":"first","inputMint":"a","outputMint":"b"}}`),
		json.RawMessage(`{"percent":67,"bps":6667,"swapInfo":{"ammKey":"second","inputMint":"a","outputMint":"b"}}`),
	}}
	path := investmentQuotePath(q)
	if len(path) != 2 || path[0].BPS != 3333 || path[1].BPS != 6667 {
		t.Fatalf("lost exact split allocation: %+v", path)
	}
}
