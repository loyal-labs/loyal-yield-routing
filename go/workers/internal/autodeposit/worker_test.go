package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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

// scriptedExecutor records dispatch order and replays exit codes.
type scriptedExecutor struct {
	exits     []*int
	errs      []error
	order     []ExecutableTarget
	calls     int
	afterCall func(index int)
}

func exitCodePtr(code int) *int { return &code }

func (s *scriptedExecutor) Execute(ctx context.Context, target ExecutableTarget) (*int, error) {
	index := s.calls
	s.calls++
	s.order = append(s.order, target)
	if s.afterCall != nil {
		s.afterCall(index)
	}
	var exit *int
	if index < len(s.exits) {
		exit = s.exits[index]
	}
	var err error
	if index < len(s.errs) {
		err = s.errs[index]
	}
	return exit, err
}

// TestWorkerDispatchKeepsRecoveryFirstAndClassifiesExits pins the dispatch
// contract the scan's ordering guarantees depend on: the prioritized order is
// executed as given, exits map onto the legacy classification, and an executor
// that dies without an exit is a failure with an alert, not silent progress.
func TestWorkerDispatchKeepsRecoveryFirstAndClassifiesExits(t *testing.T) {
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
		exits: []*int{exitCodePtr(ExitCompleted), exitCodePtr(ExitRecoveryPending), nil},
		errs:  []error{nil, nil, errors.New("rpc connection reset")},
	}
	worker, err := NewWorker(WorkerDependencies{Store: &Store{}, Executor: executor})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	var outcome ExecutorOutcome
	alerts := worker.dispatch(context.Background(), []ExecutableTarget{recovery, fresh, {TargetID: 3, ScheduledSlotID: 33}}, &outcome)

	if executor.order[0] != recovery {
		t.Fatalf("dispatch order started with %v; recovery rows must execute first", executor.order[0])
	}
	if outcome.ExecutionsAttempted != 3 || outcome.ExecutionsCompleted != 1 || outcome.ExecutionsRecoveryPending != 1 || outcome.ExecutionsFailed != 1 {
		t.Fatalf("outcome tallies %+v do not match the three exits", outcome)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0].Summary, "rpc connection reset") {
		t.Fatalf("alerts %v must name the executor error that never reached an exit", alerts)
	}

	// Exit zero without a completion marker is process success only: it must
	// stay unclassified instead of being counted as finished work.
	zeroExecutor := &scriptedExecutor{exits: []*int{exitCodePtr(0)}}
	worker, err = NewWorker(WorkerDependencies{Store: &Store{}, Executor: zeroExecutor})
	if err != nil {
		t.Fatalf("rebuild worker: %v", err)
	}
	outcome = ExecutorOutcome{}
	alerts = worker.dispatch(context.Background(), []ExecutableTarget{fresh}, &outcome)
	if outcome.ExecutionsProcessSuccessUnclassifd != 1 || outcome.ExecutionsCompleted != 0 {
		t.Fatalf("unclassified exit zero tallied %+v", outcome)
	}
	if len(alerts) != 0 {
		t.Fatalf("unclassified exit zero must not page: %v", alerts)
	}
}
