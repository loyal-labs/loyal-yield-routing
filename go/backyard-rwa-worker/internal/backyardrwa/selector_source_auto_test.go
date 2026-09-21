package backyardrwa

// Doc-15 candidate source proof: one owned AUTO position drives the actual
// observeAutoSelectorSource entry end to end through the real funding
// producer, the real residue requote, and the real consumer walk. Only the
// transport is synthetic; every gate, validator and cost engine is the
// production code on the reviewed candidate manifest.

import (
	"context"
	"encoding/binary"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

// autoSourceFundedBatch mutates the default owned-position batch so the
// funding trace is solvent: the collateral custody holds 5e9 nine-dec tokens
// (7_500_000 micro-USDC at the 1.5 price — above the funding margin's need of
// about 7.14e6 against the ~7.0e6 PYUSD payoff), and the idle PYUSD custody is
// emptied so the source gate's DebtIdleRaw==0 contract holds and debt cash
// starts at zero.
func autoSourceFundedBatch(route RuntimeRoute) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 5_000_000_000)
		binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	}
}

func TestAutoSourceFullCandidateTrace(t *testing.T) {
	const slot = int64(58)
	manifest, route, accounts := autoObservationBatch(t, slot, nil)
	autoSourceFundedBatch(route)(accounts)
	observation, _, err := autoObservationForAccounts(manifest, slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observation.Snapshot.PilotActive = true
	observation.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("auto-source-full-trace"))
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	batch := append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...)
	rpc := autoPayoffRPC(t, slot, batch)
	// Token-coherent prices with Jupiter's actual slippage contract: every
	// edge offers the 50bps floor as its threshold, so the enforced funding
	// minimum sits inside the wire's slippage band. Collateral sells at 1.5
	// USDC per token across the 9-to-6 decimal shift (out = 3/2000 of raw);
	// PYUSD converts to USDC at parity.
	client := autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
		switch {
		case in == route.Kamino.CollateralMint && destination == route.Kamino.DebtMint,
			in == route.Kamino.CollateralMint && destination == bridgeUSDC:
			out := amount * 3 / 2000
			return out, out * 9950 / 10_000
		case in == route.Kamino.DebtMint && destination == bridgeUSDC:
			return amount, amount * 9950 / 10_000
		case destination == route.Kamino.CollateralMint:
			out := amount * 2000 / 3
			return out, out * 9950 / 10_000
		default:
			t.Fatalf("unexpected quote edge %s->%s", in, destination)
			return 0, 0
		}
	}, nil)
	q, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	if err != nil {
		t.Fatal(err)
	}
	if q.Lane != route.Lane || q.MinimumIdleRaw <= 0 || q.Recipe.CostRaw <= 0 || q.ExitBound == nil {
		t.Fatal("candidate source quote lost its identity or economics", q)
	}
	// The surviving funding leg must carry the enforced minimum, not the quote.
	if len(q.Recipe.Inputs) == 0 {
		t.Fatal("candidate source recipe lost its inputs", q)
	}
	var fundingMinimum uint64
	var sawFunding, sawRepay, sawWithdraw, sawConversion bool
	stage, restore, residue := uint64(0), uint64(0), uint64(0)
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case JupiterSwapRequest:
			switch r.Action {
			case SwapCollateralToDebtStep:
				// autoCandidateQuote sells collateral at 3/2000: 5e9 nine-dec
				// raw quote 7_500_000 six-dec PYUSD; the enforced minimum is
				// the 50bps floor 7_462_500, not the quote.
				if r.AmountRaw != 5_000_000_000 || r.MinimumOutputRaw != 7_462_500 {
					t.Fatal("funding leg lost its guaranteed minimum", r.AmountRaw, r.MinimumOutputRaw)
				}
				fundingMinimum, sawFunding = r.MinimumOutputRaw, true
			case SwapCollateralToStableStep:
				// The withdrawn 20e9 collateral tokens convert at 1.5 USDC
				// per token; the enforced minimum is the 50bps floor.
				if r.MinimumOutputRaw != 29_850_000 {
					t.Fatal("collateral conversion lost its minimum", r.MinimumOutputRaw)
				}
				sawConversion = true
			case SwapDebtToUSDCStep:
				residue = r.AmountRaw
			}
		case KaminoPrimeUSDCRequest:
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				t.Fatal(err)
			}
			switch leg {
			case kaminoLegRepay:
				sawRepay = true
			case kaminoLegWithdraw:
				sawWithdraw = true
			default:
				t.Fatal("unexpected kamino leg", leg)
			}
		case BridgeBuildRequest:
			switch r.Action {
			case StageSquadsToVoltr:
				stage = r.AmountRaw
			case VoltrRestoreIdle:
				restore = r.AmountRaw
			}
		}
	}
	if !sawFunding || !sawRepay || !sawWithdraw || !sawConversion {
		t.Fatal("funding/repay/withdraw/conversion trace incomplete", sawFunding, sawRepay, sawWithdraw, sawConversion)
	}
	if q.ExitBound.MaxDebtRaw < int64(autoFixtureDebtRaw)+1 || fundingMinimum <= uint64(q.ExitBound.MaxDebtRaw) {
		t.Fatal("payoff bound or funding minimum incoherent", q.ExitBound, fundingMinimum)
	}
	// The residue is requoted at the GUARANTEED remainder: funding minimum
	// minus the payoff upper — never the margin-inflated optimistic residue
	// (upper estimate minus observed debt) the producer's tail sold.
	guaranteed := fundingMinimum - uint64(q.ExitBound.MaxDebtRaw)
	optimistic := uint64(0)
	if upper, err := withdrawalUSDCExitEstimate(7_500_000); err == nil {
		optimistic = upper - autoFixtureDebtRaw
	}
	if residue != guaranteed || residue == optimistic {
		t.Fatal("residue was not requoted at the guaranteed remainder", residue, guaranteed, optimistic)
	}
	// Stage/restore are rebuilt from guaranteed balances: squads cash is the
	// 6 raw start plus the collateral->USDC minimum plus the residue minimum.
	if stage != 6+29_850_000+residue*9950/10_000 || restore != stage {
		t.Fatal("continuation templates were not rebuilt from guaranteed custody", stage, restore, residue)
	}
	if q.MinimumIdleRaw != uint64(observation.Snapshot.VoltrIdleRaw)+stage {
		t.Fatal("minimum idle did not count guaranteed cash only", q.MinimumIdleRaw, stage)
	}
	if q.ExitBound.MaxDebtRaw < int64(autoFixtureDebtRaw) || q.ExitBound.MaxCollateralRaw != observation.Snapshot.PositionCollateralRaw {
		t.Fatal("exit bound lost the payoff ceiling", q.ExitBound)
	}
}

// autoSourceReleasedBatch is the common zero-idle case: no collateral and no
// debt sit in custody, so the payoff must be funded by a PILOT RELEASE of
// excess position collateral (release -> funding -> repay -> remaining
// withdrawal -> conversions -> stage/restore). The reserve's loan-to-value
// byte and the market's global allowed borrow value give the release risk
// model its reviewed room; every other byte stays the default owned batch.
func autoSourceReleasedBatch(route RuntimeRoute) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		autoSourceFundedBatch(route)(accounts)
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
		accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 60
		binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 1_000_000_000)
	}
}

// autoCollateralSellQuote prices every AUTO collateral sell at 1.5 USDC (or
// PYUSD at par) per token across the 9-to-6 decimal shift with the 50bps
// floor as the threshold; the buy edge mirrors it. override, when set,
// replaces the economics for one edge pair.
func autoCollateralSellQuote(t *testing.T, route RuntimeRoute, override func(in, destination string, amount uint64) (uint64, uint64, bool)) func(in, destination string, amount uint64) (uint64, uint64) {
	return func(in, destination string, amount uint64) (uint64, uint64) {
		if override != nil {
			if out, min, ok := override(in, destination, amount); ok {
				return out, min
			}
		}
		switch {
		case in == route.Kamino.CollateralMint && destination == route.Kamino.DebtMint,
			in == route.Kamino.CollateralMint && destination == bridgeUSDC:
			out := amount * 3 / 2000
			return out, out * 9950 / 10_000
		case in == route.Kamino.DebtMint && destination == bridgeUSDC:
			return amount, amount * 9950 / 10_000
		case destination == route.Kamino.CollateralMint:
			out := amount * 2000 / 3
			return out, out * 9950 / 10_000
		default:
			t.Fatalf("unexpected quote edge %s->%s", in, destination)
			return 0, 0
		}
	}
}

// autoSourceReleaseFixture observes the released zero-idle batch through the
// real production observer and pre-derives the chain facts the assertions
// need: the six-step payoff upper and the reviewed AUTO pilot release bound.
func autoSourceReleaseFixture(t *testing.T, extra func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, Observation, []ConfirmedAccount, KaminoReleaseBound, uint64) {
	const slot = int64(58)
	manifest, route, accounts := autoObservationBatch(t, slot, nil)
	autoSourceReleasedBatch(route)(accounts)
	if extra != nil {
		extra(accounts)
	}
	observation, _, err := autoObservationForAccounts(manifest, slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observation.Snapshot.PilotActive = true
	observation.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("auto-source-release-trace"))
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	future, err := decodeKaminoPayoffWindow(accounts, route, slot, 6)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := manifest.decodeKaminoRepaymentReleaseForMode(accounts, route, slot, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	if bound.ReceiptRaw == 0 || bound.ReceiptRaw >= autoFixtureDepositReceiptRaw || bound.LiquidityRaw == 0 {
		t.Fatal("reviewed AUTO pilot release produced no safe allowance", bound)
	}
	return manifest, route, observation, accounts, bound, future.UpperDebtRaw
}

func TestAutoSourceReleaseFundedFullReturnTrace(t *testing.T) {
	manifest, route, observation, accounts, release, upper := autoSourceReleaseFixture(t, nil)
	rpc := autoPayoffRPC(t, 58, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
	q, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	if err != nil {
		t.Fatal(err)
	}
	// Walk the actual retained recipe through the SAME manifest and assert
	// every guaranteed transfer balance against the pre-derived chain facts.
	var sawRelease, sawRepay, sawFunding, sawWithdraw, sawConversion bool
	receipt, fundingMinimum, conversionMinimum, residue, stage, restore := uint64(0), uint64(0), uint64(0), uint64(0), uint64(0), uint64(0)
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case KaminoPrimeUSDCRequest:
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				t.Fatal(err)
			}
			switch leg {
			case kaminoLegWithdraw:
				if r.RepaymentRelease {
					// The pilot release of excess collateral that funds the
					// payoff.
					receipt, sawRelease = r.AmountRaw, true
					break
				}
				// The tail withdrawal of the remaining position after the
				// repay, a plain collateral exit.
				if r.AmountRaw != autoFixtureDepositReceiptRaw-receipt {
					t.Fatal("tail withdrawal did not exit the remaining position", r.AmountRaw, receipt)
				}
				sawWithdraw = true
			case kaminoLegRepay:
				if r.AmountRaw != upper {
					t.Fatal("repay did not retire the payoff upper", r.AmountRaw, upper)
				}
				sawRepay = true
			default:
				t.Fatal("unexpected kamino leg", leg)
			}
		case JupiterSwapRequest:
			switch r.Action {
			case SwapCollateralToDebtStep:
				// The funding swap sells exactly the released liquidity at the
				// enforced floor, and that floor covers the payoff upper.
				if r.AmountRaw != release.LiquidityRaw {
					t.Fatal("funding did not sell the released custody", r.AmountRaw, release.LiquidityRaw)
				}
				fundingMinimum, sawFunding = r.MinimumOutputRaw, true
			case SwapCollateralToStableStep:
				if r.AmountRaw != 2*(autoFixtureDepositReceiptRaw-receipt) {
					t.Fatal("conversion did not sell the remaining withdrawal", r.AmountRaw)
				}
				conversionMinimum, sawConversion = r.MinimumOutputRaw, true
			case SwapDebtToUSDCStep:
				residue = r.AmountRaw
			}
		case BridgeBuildRequest:
			switch r.Action {
			case StageSquadsToVoltr:
				stage = r.AmountRaw
			case VoltrRestoreIdle:
				restore = r.AmountRaw
			}
		}
	}
	if !sawRelease || !sawRepay || !sawFunding || !sawWithdraw || !sawConversion {
		t.Fatal("release/funding/repay/withdraw/conversion trace incomplete", sawRelease, sawRepay, sawFunding, sawWithdraw, sawConversion)
	}
	if receipt != release.ReceiptRaw || fundingMinimum != release.LiquidityRaw*3/2000*9950/10_000 || fundingMinimum <= upper {
		t.Fatal("release or funding minimum drifted from the reviewed bound", receipt, release, fundingMinimum, upper)
	}
	if conversionMinimum != 2*(autoFixtureDepositReceiptRaw-receipt)*3/2000*9950/10_000 {
		t.Fatal("conversion minimum drifted from the remaining withdrawal", conversionMinimum)
	}
	// The residue is requoted at the GUARANTEED funding remainder, never the
	// producer's margin-inflated estimate; stage/restore hold the guaranteed
	// squads balance (start 6 + conversion minimum + residue minimum).
	guaranteed := fundingMinimum - upper
	optimistic := uint64(0)
	if estimate, err := withdrawalUSDCExitEstimate(release.LiquidityRaw * 3 / 2000); err == nil {
		optimistic = estimate - autoFixtureDebtRaw
	}
	if residue != guaranteed || residue == optimistic || guaranteed == 0 {
		t.Fatal("residue was not requoted at the guaranteed remainder", residue, guaranteed, optimistic)
	}
	if stage != 6+conversionMinimum+residue*9950/10_000 || restore != stage {
		t.Fatal("continuation templates were not rebuilt from guaranteed custody", stage, restore, residue)
	}
	if q.MinimumIdleRaw != uint64(observation.Snapshot.VoltrIdleRaw)+stage {
		t.Fatal("minimum idle did not count guaranteed cash only", q.MinimumIdleRaw, stage)
	}
	if q.ExitBound == nil || q.ExitBound.MaxDebtRaw != int64(upper) || q.Recipe.CostRaw <= 0 || len(q.Recipe.Costs) != len(q.Recipe.Inputs) {
		t.Fatal("exit bound or message costs incomplete", q.ExitBound, q.Recipe.CostRaw, len(q.Recipe.Costs), len(q.Recipe.Inputs))
	}
}

// TestAutoSourceReleaseZeroResidueDropsConversion proves a funding minimum
// that exactly meets the payoff upper leaves a zero guaranteed remainder and
// drops the residue conversion instead of selling unproduced funding.
func TestAutoSourceReleaseZeroResidueDropsConversion(t *testing.T) {
	manifest, route, observation, accounts, _, upper := autoSourceReleaseFixture(t, nil)
	rpc := autoPayoffRPC(t, 58, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	// Quote the funding edge at the exact floor that meets the upper: the
	// enforced minimum then equals the upper and the guaranteed remainder is
	// exactly zero.
	exact := (upper*10_000 + 9_949) / 9_950
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, func(in, destination string, amount uint64) (uint64, uint64, bool) {
		if in == route.Kamino.CollateralMint && destination == route.Kamino.DebtMint {
			return exact, upper, true
		}
		return 0, 0, false
	}), nil)
	q, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := request.(JupiterSwapRequest); ok && r.Action == SwapDebtToUSDCStep {
			t.Fatal("zero-guarantee residue conversion survived", r.AmountRaw)
		}
	}
	stage, conversionMinimum := uint64(0), uint64(0)
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case BridgeBuildRequest:
			if r.Action == StageSquadsToVoltr {
				stage = r.AmountRaw
			}
		case JupiterSwapRequest:
			if r.Action == SwapCollateralToStableStep {
				conversionMinimum = r.MinimumOutputRaw
			}
		}
	}
	if stage != 6+conversionMinimum || q.MinimumIdleRaw != uint64(observation.Snapshot.VoltrIdleRaw)+stage {
		t.Fatal("zero-residue minimum idle drifted from guaranteed cash", q.MinimumIdleRaw, stage, conversionMinimum)
	}
}

// TestAutoSourceReleaseUnavailableResidueQuoteHolds proves a residue requote
// miss after the producer's tail quote is a fail-closed hold, never a stale
// residue amount silently kept.
func TestAutoSourceReleaseUnavailableResidueQuoteHolds(t *testing.T) {
	manifest, route, observation, accounts, _, _ := autoSourceReleaseFixture(t, nil)
	rpc := autoPayoffRPC(t, 58, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
	debts := 0
	inner := client.http.Transport
	client.http.Transport = countingFailTransport{inner: inner, route: route, hits: &debts, failAt: 2}
	_, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	if err == nil || !strings.Contains(err.Error(), "selector_source_residue_quote_unavailable") {
		t.Fatal("an unavailable residue requote was not a fail-closed hold", err)
	}
}

type countingFailTransport struct {
	inner  http.RoundTripper
	route  RuntimeRoute
	hits   *int
	failAt int
}

func (c countingFailTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/quote" && request.URL.Query().Get("inputMint") == c.route.Kamino.DebtMint {
		*c.hits++
		if *c.hits >= c.failAt {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: make(http.Header)}, nil
		}
	}
	return c.inner.RoundTrip(request)
}

// TestAutoSourceReleaseStaleChainTimeHolds proves a chain slot beyond the
// batch's Clock cannot price the payoff window or the exit on the actual
// source path.
func TestAutoSourceReleaseStaleChainTimeHolds(t *testing.T) {
	manifest, route, observation, accounts, _, _ := autoSourceReleaseFixture(t, nil)
	rpc := autoPayoffRPC(t, 58+64, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
	_, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	assertBudgetHold(t, err, "invalid_payoff_clock")
}

// TestAutoSourceReleaseAbovePegDebtTraces proves a PYUSD debt repriced above
// peg completes the same release-funded trace and keeps every guaranteed
// balance relationship intact through the repriced liability.
func TestAutoSourceReleaseAbovePegDebtTraces(t *testing.T) {
	abovePeg := func(accounts []ConfirmedAccount) {
		putScaledFraction(accountAt(accounts, autoAUTOPYUSD.Kamino.DebtReserve).Data[248:264],
			new(big.Int).Quo(new(big.Int).Mul(big.NewInt(102), new(big.Int).Lsh(big.NewInt(1), 60)), big.NewInt(100)))
	}
	manifest, route, observation, accounts, _, _ := autoSourceReleaseFixture(t, abovePeg)
	if observation.Snapshot.PositionDebtValueRaw != 7_140_000 {
		t.Fatal("above-peg batch lost its repriced liability", observation.Snapshot.PositionDebtValueRaw)
	}
	rpc := autoPayoffRPC(t, 58, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
	q, err := observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	if err != nil {
		t.Fatal(err)
	}
	residue, stage, conversionMinimum, fundingMinimum := uint64(0), uint64(0), uint64(0), uint64(0)
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case JupiterSwapRequest:
			switch r.Action {
			case SwapCollateralToDebtStep:
				fundingMinimum = r.MinimumOutputRaw
			case SwapCollateralToStableStep:
				conversionMinimum = r.MinimumOutputRaw
			case SwapDebtToUSDCStep:
				residue = r.AmountRaw
			}
		case BridgeBuildRequest:
			if r.Action == StageSquadsToVoltr {
				stage = r.AmountRaw
			}
		}
	}
	if fundingMinimum == 0 || residue != fundingMinimum-uint64(q.ExitBound.MaxDebtRaw) {
		t.Fatal("above-peg residue left the guaranteed remainder", fundingMinimum, residue, q.ExitBound)
	}
	if stage != 6+conversionMinimum+residue*9950/10_000 || q.MinimumIdleRaw != uint64(observation.Snapshot.VoltrIdleRaw)+stage {
		t.Fatal("above-peg guaranteed balances drifted", stage, residue, q.MinimumIdleRaw)
	}
}

// TestAutoSourcePreexistingIdleDebtIsRefused proves the shared source gate
// still refuses a batch with idle PYUSD already in the debt custody: a
// preexisting shared-debt balance is never folded into a candidate exit.
func TestAutoSourcePreexistingIdleDebtIsRefused(t *testing.T) {
	const slot = int64(58)
	manifest, route, accounts := autoObservationBatch(t, slot, nil)
	// Keep the default batch's non-zero debt custody; only arm the trace.
	observation, _, err := autoObservationForAccounts(manifest, slot, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observation.Snapshot.PilotActive = true
	observation.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("auto-source-shared-debt"))
	rpc := autoPayoffRPC(t, slot, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client := autoJupiterTransport(t, route, autoCollateralSellQuote(t, route, nil), nil)
	_, err = observeAutoSelectorSource(context.Background(), rpc, client, manifest, observation)
	assertBudgetHold(t, err, "selector_source_unavailable")
}
