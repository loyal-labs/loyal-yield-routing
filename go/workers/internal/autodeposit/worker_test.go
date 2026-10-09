package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// TestDepositPlanJSONMatchesLegacyExecutor pins the frozen plan's wire shape:
// the TypeScript recovery path parses these plans, so raw custody values must
// travel as strings and the destination identity must survive a round trip.
func TestDepositPlanJSONMatchesLegacyExecutor(t *testing.T) {
	reserve := "ReserveKamino1111111111111111111111111111111"
	plan := DepositPlan{
		Version:       1,
		AmountRaw:     4_000_000,
		Reserve:       reserve,
		Market:        "MarketKamino111111111111111111111111111111111",
		LiquidityMint: USDCMint,
		Target: DepositPlanTarget{
			ID:                 7,
			ManagedVaultID:     3,
			Settings:           "settings-pda",
			VaultIndex:         2,
			Wallet:             "wallet-pubkey",
			WalletUsdcAta:      "wallet-usdc-ata",
			WalletTokenAta:     "wallet-token-ata",
			VaultPubkey:        "vault-pubkey",
			VaultUsdcAta:       "vault-usdc-ata",
			VaultTokenAta:      "vault-token-ata",
			TokenMint:          USDCMint,
			RoutePolicyAccount: "policy-pda",
			RoutePolicySeed:    42,
		},
	}
	serialized, err := MarshalDepositPlan(plan)
	if err != nil {
		t.Fatalf("marshal deposit plan: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(serialized, &generic); err != nil {
		t.Fatalf("re-read plan json: %v", err)
	}
	if generic["amountRaw"] != "4000000" {
		t.Fatalf("amountRaw must serialize as a string, got %v", generic["amountRaw"])
	}
	target := generic["target"].(map[string]any)
	if target["id"] != "7" || target["managedVaultId"] != "3" || target["routePolicySeed"] != "42" {
		t.Fatalf("plan ids must serialize as strings, got target %v", target)
	}
	if _, isNumber := target["vaultIndex"].(float64); !isNumber {
		t.Fatalf("vaultIndex must stay a JSON number like the legacy writer, got %T", target["vaultIndex"])
	}

	parsed, err := UnmarshalDepositPlan(serialized)
	if err != nil {
		t.Fatalf("round-trip plan: %v", err)
	}
	if parsed != plan {
		t.Fatalf("round-trip changed the plan: %+v != %+v", parsed, plan)
	}

	if _, err := UnmarshalDepositPlan([]byte(`{"version":2}`)); err == nil {
		t.Fatal("an unknown plan version must be refused, not re-executed")
	}
	missingDestination := plan
	missingDestination.Target.Wallet = ""
	if _, err := UnmarshalDepositPlan(mustMarshal(t, missingDestination)); err == nil {
		t.Fatal("a plan without its frozen destination must be refused")
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// scriptedExecutor records dispatch order and replays family outcomes.
type scriptedExecutor struct {
	results   []ExecutorResult
	errs      []error
	order     []ExecutableTarget
	calls     int
	afterCall func(index int)
}

func (s *scriptedExecutor) Execute(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	index := s.calls
	s.calls++
	s.order = append(s.order, target)
	if s.afterCall != nil {
		s.afterCall(index)
	}
	var result ExecutorResult
	if index < len(s.results) {
		result = s.results[index]
	}
	var err error
	if index < len(s.errs) {
		err = s.errs[index]
	}
	return result, err
}

// TestWorkerDispatchKeepsRecoveryFirstAndClassifiesOutcomes pins the dispatch
// contract the scan's ordering guarantees depend on: the prioritized order is
// executed as given, and an executor that fails without an outcome alerts.
func TestWorkerDispatchKeepsRecoveryFirstAndClassifiesOutcomes(t *testing.T) {
	deps := WorkerDependencies{
		Store:    &Store{},
		Executor: nil,
	}
	if _, err := NewWorker(deps); err == nil {
		t.Fatal("a worker without an executor must be refused")
	}
	if _, err := NewWorker(WorkerDependencies{}); err == nil {
		t.Fatal("a worker without a store must be refused")
	}

	recovery := ExecutableTarget{TargetID: 1, ScheduledSlotID: 11, ClaimToken: "claim-a"}
	fresh := ExecutableTarget{TargetID: 2, ScheduledSlotID: 22}
	executor := &scriptedExecutor{
		results: []ExecutorResult{ResultCompleted, ResultRecoveryPending, ResultUnknown},
		errs:    []error{nil, nil, errors.New("rpc connection reset")},
	}
	worker, err := NewWorker(WorkerDependencies{Store: &Store{}, Executor: executor, Facts: testFacts(), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	var outcome ExecutorOutcome
	alerts := worker.dispatch(context.Background(), []ExecutableTarget{recovery, fresh, {TargetID: 3, ScheduledSlotID: 33}}, &outcome)

	if executor.order[0] != recovery {
		t.Fatalf("dispatch order started with %v; recovery rows must execute first", executor.order[0])
	}
	if outcome.ExecutionsAttempted != 3 || outcome.ExecutionsCompleted != 1 || outcome.ExecutionsRecoveryPending != 1 || outcome.ExecutionsFailed != 1 {
		t.Fatalf("outcome tallies %+v do not match the three outcomes", outcome)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0].Summary, "rpc connection reset") {
		t.Fatalf("alerts %v must name the executor error that returned no outcome", alerts)
	}

	// An unknown result cannot prove finished work or healthy execution.
	zeroExecutor := &scriptedExecutor{results: []ExecutorResult{ResultUnknown}}
	worker, err = NewWorker(WorkerDependencies{Store: &Store{}, Executor: zeroExecutor, Facts: testFacts(), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatalf("rebuild worker: %v", err)
	}
	outcome = ExecutorOutcome{}
	alerts = worker.dispatch(context.Background(), []ExecutableTarget{fresh}, &outcome)
	if outcome.ExecutionsUnknown != 1 || outcome.ExecutionsCompleted != 0 {
		t.Fatalf("unknown outcome tallied %+v", outcome)
	}
	if len(alerts) != 1 || alerts[0].Code != "autodeposit_executor_failed" {
		t.Fatalf("unknown outcome must fail closed: %v", alerts)
	}
}

func TestWorkerDispatchRetainsDecisionsAndRecoveryErrors(t *testing.T) {
	executor := &scriptedExecutor{
		results: []ExecutorResult{ResultDeferred, ResultRecoveryPending, ResultNotActionable, ResultNoop, ResultDependencyUnavailable, "future_outcome"},
		errs:    []error{errors.New("allowance unknown"), errors.New("ownership lost")},
	}
	worker, err := NewWorker(WorkerDependencies{Store: &Store{}, Executor: executor, Facts: testFacts(), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	var outcome ExecutorOutcome
	alerts := worker.dispatch(context.Background(), []ExecutableTarget{{TargetID: 1}, {TargetID: 2}, {TargetID: 3}, {TargetID: 4}, {TargetID: 5}, {TargetID: 6}}, &outcome)
	if outcome.ExecutionsAttempted != 6 || outcome.ExecutionsDeferred != 1 || outcome.ExecutionsRecoveryPending != 1 || outcome.ExecutionsNotActionable != 1 || outcome.ExecutionsNoop != 1 || outcome.ExecutionsFailed != 2 || outcome.ExecutionsCompleted != 0 || worker.executionErrors != 2 {
		t.Fatalf("outcome %+v, errors %d", outcome, worker.executionErrors)
	}
	if len(alerts) != 2 || alerts[0].Code != "autodeposit_dependency_unavailable" || !alerts[0].SelfRecovering || alerts[1].Code != "autodeposit_executor_failed" {
		t.Fatalf("alerts %+v", alerts)
	}
}

// Root cause: the Go port collapsed every Autodeposit alert and tick failure
// into one generic log line with no fact, so a failing family showed only as
// stale progress. Each now counts loyal_family_failed_total under the Rust
// trigger's OperationalError code for the same meaning.
func TestWorkerCountsStableFailureCodes(t *testing.T) {
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	executor := &scriptedExecutor{results: []ExecutorResult{ResultClaimTransitionFailed, ResultKaminoTopUpFailed, ResultDeferred, ResultClaimTransitionFailed}}
	worker, err := NewWorker(WorkerDependencies{Store: &Store{}, Executor: executor, Facts: facts, FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	var outcome ExecutorOutcome
	worker.dispatch(context.Background(), []ExecutableTarget{{TargetID: 1}, {TargetID: 2}, {TargetID: 3}, {TargetID: 4}}, &outcome)
	if _, err := worker.Tick(context.Background()); err == nil {
		t.Fatal("a pass without a database succeeded")
	}
	for code, want := range map[string]float64{"autodeposit_claim_transition_failed": 2, "kamino_top_up_failed": 1, "autodeposit_projection_failed": 1, "yield_persistence_failed": 0} {
		if got := failedCount(t, registry, code); got != want {
			t.Errorf("failed{code=%s} = %v, want %v", code, got, want)
		}
	}
	if total := failedCount(t, registry, ""); total != 0 {
		t.Errorf("a failure was counted without a code: %v", total)
	}
}

// fundedPayer is a fee payer that can always pay.
type fundedPayer struct{}

func (fundedPayer) FeePayerLamports(context.Context) (string, uint64, error) {
	return "itest-fee-payer", 1_000_000_000, nil
}

// failedCount reads loyal_family_failed_total{family="autodeposit",code}.
func failedCount(t *testing.T, registry *prometheus.Registry, code string) float64 {
	t.Helper()
	return metricValue(t, registry, "loyal_family_failed_total", map[string]string{"family": "autodeposit", "code": code})
}

// metricValue reads one counter or gauge series; absent reads as 0.
func metricValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	series:
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if want, named := labels[label.GetName()]; named && want != label.GetValue() {
					continue series
				}
			}
			if metric.GetGauge() != nil {
				return metric.GetGauge().GetValue()
			}
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}

// LoyalFamilyProgressStale must mean something: a pass marks progress only
// when nothing it was due to do is left undone. A noop (a capped allowance)
// or a drained vault is nothing to do; a deferral, a held claim or an
// exhausted payer is undone work.
func TestOnlySettledPassesMarkProgress(t *testing.T) {
	for _, c := range []struct {
		results []ExecutorResult
		payer   bool
		settled bool
	}{
		{nil, false, true},
		{[]ExecutorResult{ResultCompleted, ResultNoop, ResultNotActionable}, false, true},
		{[]ExecutorResult{ResultCompleted, ResultDeferred}, false, false},
		{[]ExecutorResult{ResultRecoveryPending}, false, false},
		{nil, true, false},
	} {
		report := TickReport{FeePayerExhausted: c.payer}
		for _, result := range c.results {
			report.Outcome.ExecutionsAttempted++
			report.Outcome.RecordExecutorResult(result)
		}
		if report.settled() != c.settled {
			t.Errorf("results %v payer exhausted %v: settled %v", c.results, c.payer, report.settled())
		}
	}
}
