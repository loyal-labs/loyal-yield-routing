package backyard

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestEconomicReductionNeedsNetBenefitAndPersistence(t *testing.T) {
	s := leverageSnapshot(2.5)
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.TotalVaultNAVRaw, s.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	armFeeAuthorityFixture(t, &s)
	p := productionRWAPolicy
	price := func(float64) (leverageReduction, error) {
		return leverageReduction{RepaidMicros: 500_000_000, CollateralMicros: 502_500_000, Cost: leverageStepCost{TotalMicros: 2_500_000}}, nil // 50 bps
	}
	market := leverageMarket(s.RouteLane, .10, math.Log1p(.13))
	decide := func() leverageDecision {
		d, ok := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, p, nil, price)
		if !ok {
			t.Fatal("settled economic decision absent")
		}
		return d
	}
	if got := decide(); got.Next != 2.5 || got.Reason != leverageDownNotWorthCost || got.GainRaw >= 0 {
		t.Fatalf("Oct 29-style 3-point dip paid 50 bps to exit: %+v", got)
	}
	// A severe sustained rate shock can pay for the sale; it still needs
	// consecutive observations, and outages or changed targets restart them.
	market.CurrentBorrowAPY = .80
	approved := decide()
	if approved.Next != 2 || approved.Reason != leverageDownApproved || approved.GainRaw <= 0 {
		t.Fatalf("profitable debt reduction blocked: %+v", approved)
	}
	sampler := &selectorSampler{}
	now := time.Unix(1_800_000_000, 0)
	for minute := 0; minute <= 30; minute += 5 {
		got := sampler.persistDown(approved, p, now.Add(time.Duration(minute)*time.Minute))
		if (got.Next < got.Current) != (minute == 30) {
			t.Fatalf("persistence at minute %d: %+v", minute, got)
		}
	}
	if got := sampler.persistDown(approved, p, now.Add(time.Hour)); got.Next != got.Current {
		t.Fatal("stale observation window survived", got)
	}
	price = func(float64) (leverageReduction, error) { return leverageReduction{}, errors.New("quote unavailable") }
	unknown := decide()
	if !unknown.outage() || unknown.Next != unknown.Current {
		t.Fatal("unpriced sale authorized", unknown)
	}
	sampler.persistDown(unknown, p, now.Add(time.Hour+time.Minute))
	if len(sampler.downWindows) != 0 {
		t.Fatal("outage retained persistence")
	}
	if got := (&selectorSampler{}).persistDown(approved, p, now.Add(2*time.Hour)); got.Next != got.Current {
		t.Fatal("restart inherited an unverified approval")
	}
	// Emergency and required withdrawal decisions do not call this gate.
	s.LTVBPS, s.LiquidationThresholdBPS, s.MaxLTVBPS = 6400, 7500, 6600
	if got := Decide(targeted(s)); got.Reason != hardLTVDeleverReason {
		t.Fatal("economic hold disabled LTV protection", got)
	}
	withdrawal := livePartialSnapshot()
	if got := Decide(targeted(withdrawal)); got.Reason != releaseReason {
		t.Fatal("economic hold delayed required withdrawal", got)
	}
}

func TestReductionPricesEntireRemainingMove(t *testing.T) {
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	o := observeLiveAtRest(t, c, liveInputs(true))
	rpc, view, client := liveReplayStack(t, o, 50)
	m := embeddedTestManifest(t)
	var lane Snapshot
	for _, l := range o.lanes {
		if l.RouteLane == autoAUTOPYUSD.Lane {
			lane = l
		}
	}
	price := productionLeverageDownPricer(context.Background(), rpc, view, client, m, o, lane)
	got, err := price(1.5)
	if err != nil {
		t.Fatal(err)
	}
	wantRepay := stepDebtToLTVRaw(lane, leverageLevelLTVBPS(1.5))
	if wantRepay <= 0 || got.RepaidMicros <= 0 || got.Cost.TotalMicros <= got.Cost.NetworkMicros || got.Cost.NetworkMicros <= 0 {
		t.Fatalf("reduction unpriced: %+v, repay=%d", got, wantRepay)
	}
	// Cost is on the whole sale (50 bps plus 50 bps allowed slippage), even when the current withdrawal
	// room means the executor must use more than one transaction.
	bps := float64(got.Cost.UnwindSwapCostMicros) / float64(got.RepaidMicros) * 10_000
	if bps < 100 || bps > 102 {
		t.Fatalf("wrong full-move cost: %.2f bps, %+v", bps, got)
	}
}

func TestReductionDoesNotBuyBackItsCashResidue(t *testing.T) {
	s := leverageSnapshot(2.5)
	s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = onreONycUSDC, onreONycUSDC, 2
	s.SquadsIdleRaw = 586_960_281 // Oct 29's intervening purchase
	if got := Decide(targeted(s)); got.Action != DeleverRouteStep || got.Reason != leverageDownPartialReason {
		t.Fatalf("bought more collateral during reduction: %+v", got)
	}
	armFeeAuthorityFixture(t, &s)
	s.ChainUnix = 1_800_000_000
	market := leverageMarket(s.RouteLane, .10, math.Log1p(.80))
	price := func(float64) (leverageReduction, error) {
		return leverageReduction{RepaidMicros: 500_000_000, CollateralMicros: 502_500_000, Cost: leverageStepCost{TotalMicros: 2_500_000}}, nil
	}
	// After a pause or restart the old permission can expire while cash remains.
	// Fresh profitable samples must be able to authorize the unfinished target.
	d, ok := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, productionRWAPolicy, nil, price)
	if !ok || d.Next != 2 || !d.Down.active(s.ChainUnix) {
		t.Fatalf("retained cash deadlocked reduction approval: %+v, eligible=%v", d, ok)
	}
	sampler := &selectorSampler{}
	for minute := 0; minute <= 30; minute += 5 {
		got := sampler.persistDown(d, productionRWAPolicy, time.Unix(s.ChainUnix, 0).Add(time.Duration(minute)*time.Minute))
		if (got.Down.MaxCostBPS > 0) != (minute == 30) {
			t.Fatalf("renewal did not require persistence: %+v", got)
		}
	}
	s.LeverageTargetLevel = 2.5
	if leverageSettled(s, SelectorResult{Action: SelectorKeep}) {
		t.Fatal("cash exception admitted a new borrowing decision")
	}
}

func TestReductionQuoteCeilingExpiryAndExactWire(t *testing.T) {
	step, _, err := stepTestBuild(t, onreONycUSDC, false, -1)
	if err != nil {
		t.Fatal(err)
	}
	minimum, err := stepConversionOutput(step.conversion())
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_800_000_000)
	evidence := StepExecutionEvidence{Request: step, ExpectedEffects: ExpectedEffects{Sale: &ExpectedSale{MarkValueRaw: minimum + 1}}}
	approval := downApproval{MaxCostBPS: 100_000_000, ExpiresUnix: now + 600}
	if err := guardDownStepCost(approval, now, evidence, 1); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*downApproval, *StepExecutionEvidence){
		func(a *downApproval, _ *StepExecutionEvidence) { *a = downApproval{} },
		func(a *downApproval, _ *StepExecutionEvidence) { a.ExpiresUnix = now },
		func(_ *downApproval, e *StepExecutionEvidence) { e.ExpectedEffects.Sale = nil },
		func(_ *downApproval, e *StepExecutionEvidence) {
			e.ExpectedEffects.Sale = &ExpectedSale{MarkValueRaw: minimum * 2}
		},
	} {
		a, e := approval, evidence
		mutate(&a, &e)
		if err := guardDownStepCost(a, now, e, 10); err == nil {
			t.Fatal("unapproved or overpriced sale admitted")
		}
	}
	if err := guardDownStepCost(approval, now, evidence, usdMicros(minimum)); err == nil {
		t.Fatal("network expense escaped budget")
	}
	// Exercise the real compiler and persisted-wire validation with an ephemeral
	// local key. It is not the live delegate and nothing is submitted.
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	message, err := compileStepMessageForDelegate(rwa, step, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(key, delegate, message, step.Open.RecentBlockhash, step.Open.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signed.BuildResult(77)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(laneLevel{Level: 1, Down: approval})
	var reloaded laneLevel
	if err != nil || json.Unmarshal(encoded, &reloaded) != nil || reloaded.Down != approval {
		t.Fatal("approval lost on restart")
	}
	if err := json.Unmarshal([]byte(`{"level":1,"down":{"maxCostBps":-1,"expiresUnix":1800000600}}`), &reloaded); err != nil || reloaded.Down != (downApproval{}) {
		t.Fatal("malformed optional approval should revoke permission without disabling safety", err, reloaded)
	}
	for _, rule := range []targetRule{ruleHardLTV, ruleTermMaturity, ruleOperatorExit, ruleWithdrawal} {
		s := Snapshot{Target: LaneTarget{Rule: rule}}
		if economicDownStep(s, Decision{Reason: debtZeroReason}) {
			t.Fatal("mandatory exit gated", rule)
		}
	}
	if !economicDownStep(Snapshot{Target: LaneTarget{Rule: ruleLevel}}, Decision{Reason: debtZeroReason}) {
		t.Fatal("legacy 1x target bypasses fresh approval")
	}
	if economicHeldLevel(Snapshot{Trustful: true, RouteLane: onreONycUSDC}, 1) != 1.5 {
		t.Fatal("changed Looping policy")
	}
}
