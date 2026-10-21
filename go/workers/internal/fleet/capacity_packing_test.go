package fleet

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// packingReserve is a same-mint USDC reserve with explicit capacity evidence.
func packingReserve(address string, apy, supply, available int64, depositLimit uint64) ReserveState {
	return ReserveState{ReserveIdentity: ReserveIdentity{Address: address, Market: "market", Mint: USDCMint}, Slot: 500, LastUpdateSlot: 500,
		SupplyAPYBPS: apy, TotalSupplyRaw: supply, EconomicLifetimeMillis: 600_000, DataHash: "hash:" + address,
		AvailableLiquidityRaw: &available, DepositLimitRaw: &depositLimit}
}

func packingSnapshot(reserves ...ReserveState) MarketSnapshot {
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 500, ObservedAt: now, Hash: "packing", Reserves: map[string]ReserveState{}}
	for _, reserve := range reserves {
		snapshot.Reserves[reserve.Address] = reserve
	}
	return snapshot
}

func packingHolding(snapshot MarketSnapshot, vault int64, source string, amount int64, targets ...string) FleetVault {
	r := snapshot.Reserves[source]
	return FleetVault{Position: VaultPosition{VaultID: vault, PolicyID: vault, VaultPubkey: fmt.Sprintf("vault-%d", vault), PolicyAuthority: fmt.Sprintf("tenant-%d", vault),
		SourceReserve: source, Market: r.Market, Mint: r.Mint, AmountRaw: amount, SourceCollateralAmountRaw: amount,
		SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited, ObservedSlot: 499, ObservedAt: snapshot.ObservedAt}, AllowedTargets: targets}
}

// moves is each selected move as vault:source>target:amount, in wave order.
func moves(plan FleetPlan) []string {
	out := []string{}
	for _, o := range plan.Opportunities {
		d := o.Decision
		out = append(out, fmt.Sprintf("%d:%s>%s:%d", d.VaultID, d.SourceReserve, d.TargetReserve, d.AmountRaw))
	}
	return out
}

func sameMoves(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The target's 2% frontier is 20e9. The wave fills it with whole holdings,
// best gain per dollar first, and the holding that no longer fits waits with
// target_capacity_exhausted. Once the admitted moves land (supply up, nothing
// in flight) the next cycle moves it.
func TestPackingFillsTargetRoomAndTheNextVaultWaitsForTheNextCycle(t *testing.T) {
	const supply = int64(1_000_000_000_000)
	unlimited := uint64(1 << 62)
	snapshot := packingSnapshot(
		packingReserve("source", 100, 1_000_000_000_000_000, 1_000_000_000_000_000, unlimited),
		packingReserve("target", 900, supply, supply, unlimited))
	vaults := []FleetVault{
		packingHolding(snapshot, 1, "source", 6_000_000_000, "target"),
		packingHolding(snapshot, 2, "source", 10_000_000_000, "target"),
		packingHolding(snapshot, 3, "source", 8_000_000_000, "target"),
	}
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moves(plan), []string{"2:source>target:10000000000", "3:source>target:8000000000"}; !sameMoves(got, want) {
		t.Fatalf("wave %v, want %v", got, want)
	}
	if plan.Rejections[1] != "target_capacity_exhausted" {
		t.Fatalf("the vault past the frontier was rejected %q, want target_capacity_exhausted", plan.Rejections[1])
	}
	// No whole holding is split to use the 2e9 of room left.
	var admitted int64
	for _, o := range plan.Opportunities {
		if o.Decision.AmountRaw != o.Decision.PrincipalUSDMicros || o.Decision.AmountRaw != vaults[o.Decision.VaultID-1].Position.AmountRaw {
			t.Fatalf("move is not the whole holding: %+v", o.Decision)
		}
		admitted += o.Decision.PrincipalUSDMicros
	}
	if admitted > supply/50 {
		t.Fatalf("admitted %d past the 2%% frontier %d", admitted, supply/50)
	}
	// Next cycle: the two moves landed and telemetry reflects them; vaults
	// with landed work are planned from their new reserve, not this source.
	target := snapshot.Reserves["target"]
	target.TotalSupplyRaw += admitted
	snapshot.Reserves["target"] = target
	next, err := PlanFleet(snapshot, vaults[:1])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moves(next), []string{"1:source>target:6000000000"}; !sameMoves(got, want) {
		t.Fatalf("next cycle %v, want %v (rejections %v)", got, want, next.Rejections)
	}
}

// The deposit limit binds before the 2% frontier: 15e9 of room takes the
// 10e9 holding, refuses the 8e9 one with target_deposit_limit, and still
// packs the 4e9 holding that fits what is left.
func TestPackingStopsAtTheTargetDepositLimit(t *testing.T) {
	const supply = int64(10_000_000_000_000)
	snapshot := packingSnapshot(
		packingReserve("source", 100, 1_000_000_000_000_000, 1_000_000_000_000_000, 1<<62),
		packingReserve("target", 900, supply, supply, uint64(supply)+15_000_000_000))
	vaults := []FleetVault{
		packingHolding(snapshot, 1, "source", 10_000_000_000, "target"),
		packingHolding(snapshot, 2, "source", 8_000_000_000, "target"),
		packingHolding(snapshot, 3, "source", 4_000_000_000, "target"),
	}
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moves(plan), []string{"1:source>target:10000000000", "3:source>target:4000000000"}; !sameMoves(got, want) {
		t.Fatalf("wave %v, want %v (rejections %v)", got, want, plan.Rejections)
	}
	if plan.Rejections[2] != "target_deposit_limit" {
		t.Fatalf("holding past the deposit limit rejected %q", plan.Rejections[2])
	}
	// A deposit limit at or below supply (0 blocks deposits) takes nothing.
	full := snapshot.Reserves["target"]
	limit := uint64(supply)
	full.DepositLimitRaw = &limit
	snapshot.Reserves["target"] = full
	if d := Plan(snapshot, vaults[2].Position, "source", "target"); d.Eligible || d.Reason != "target_deposit_limit" {
		t.Fatalf("full deposit limit planned: eligible=%v reason=%s", d.Eligible, d.Reason)
	}
}

// A source at 100% utilization pays nothing out: its holdings are
// source_liquidity_exhausted and nothing is published. A source with some
// liquidity pays the first holding and keeps the rest for the next cycle.
func TestSourceLiquidityBindsWithdrawals(t *testing.T) {
	unlimited := uint64(1 << 62)
	snapshot := packingSnapshot(
		packingReserve("source", 100, 1_000_000_000_000, 0, unlimited),
		packingReserve("target", 900, 10_000_000_000_000, 10_000_000_000_000, unlimited))
	vaults := []FleetVault{packingHolding(snapshot, 1, "source", 4_000_000_000, "target"), packingHolding(snapshot, 2, "source", 3_000_000_000, "target")}
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 0 || plan.Rejections[1] != "source_liquidity_exhausted" || plan.Rejections[2] != "source_liquidity_exhausted" {
		t.Fatalf("fully utilized source moved: %v rejections %v", moves(plan), plan.Rejections)
	}
	if len(plan.Movable) != 0 {
		t.Fatalf("capital that cannot leave counted as movable: %v", plan.Movable)
	}
	source := snapshot.Reserves["source"]
	available := int64(5_000_000_000)
	source.AvailableLiquidityRaw = &available
	snapshot.Reserves["source"] = source
	plan, err = PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moves(plan), []string{"1:source>target:4000000000"}; !sameMoves(got, want) || plan.Rejections[2] != "source_liquidity_exhausted" {
		t.Fatalf("wave %v want %v, rejections %v", got, want, plan.Rejections)
	}
	// Outflows already in flight from the source count against it too.
	vaults[0].CommittedOutflows = map[string]int64{"source": 2_000_000_000}
	vaults[1].CommittedOutflows = vaults[0].CommittedOutflows
	if plan, err = PlanFleet(snapshot, vaults); err != nil || !sameMoves(moves(plan), []string{"2:source>target:3000000000"}) {
		t.Fatalf("in-flight outflow ignored: %v %v %v", moves(plan), plan.Rejections, err)
	}
}

// Missing capacity evidence is its own reason, never zero or unlimited, and
// the epoch carries the deposit limit from the verified view into planning.
func TestUnknownCapacityEvidenceIsNotPlannable(t *testing.T) {
	unlimited := uint64(1 << 62)
	snapshot := packingSnapshot(
		packingReserve("source", 100, 1_000_000_000_000, 1_000_000_000_000, unlimited),
		packingReserve("target", 900, 10_000_000_000_000, 10_000_000_000_000, unlimited))
	position := packingHolding(snapshot, 1, "source", 4_000_000_000, "target").Position
	source, target := snapshot.Reserves["source"], snapshot.Reserves["target"]
	source.AvailableLiquidityRaw = nil
	snapshot.Reserves["source"] = source
	if d := Plan(snapshot, position, "source", "target"); d.Reason != "source_liquidity_unknown" {
		t.Fatalf("unknown source liquidity planned as %s", d.Reason)
	}
	snapshot.Reserves["source"] = packingReserve("source", 100, 1_000_000_000_000, 1_000_000_000_000, unlimited)
	target.DepositLimitRaw = nil
	snapshot.Reserves["target"] = target
	if d := Plan(snapshot, position, "source", "target"); d.Reason != "target_deposit_limit_unknown" {
		t.Fatalf("unknown deposit limit planned as %s", d.Reason)
	}
}

// replanStore is the narrow durable surface PrepareExecution touches before
// fresh preparation fails: claim, unsigned recovery and the two outcomes.
type replanStore struct {
	lease              RevalidationLease
	replanned, retried int
}

func (s *replanStore) ClaimRevalidation(context.Context, string, string, time.Duration, bool, bool, ...string) (*RevalidationLease, error) {
	lease := s.lease
	return &lease, nil
}
func (s *replanStore) CheckRevalidationLease(context.Context, RevalidationLease) error { return nil }
func (s *replanStore) ReleaseRevalidationNotYet(context.Context, RevalidationLease) error {
	return errors.New("not reached")
}
func (s *replanStore) LoadReusableLookupTables(context.Context, string, int64, int64, []string) ([]LookupTable, error) {
	return nil, errors.New("not reached")
}
func (s *replanStore) CommitRevalidation(context.Context, RevalidationLease, RevalidationCommit) error {
	return errors.New("not reached")
}
func (s *replanStore) EligibleFeePayerShards(context.Context, string, string, []string) ([]FeePayerShard, error) {
	return nil, nil
}
func (s *replanStore) CommitExecutionAdmission(context.Context, RevalidationLease, RevalidationCommit, *ExecutionAdmission) error {
	return errors.New("not reached")
}
func (s *replanStore) DeferRevalidation(context.Context, RevalidationLease, string) (bool, error) {
	s.retried++
	return true, nil
}
func (s *replanStore) ReplanRevalidation(context.Context, RevalidationLease) (bool, error) {
	s.replanned++
	return true, nil
}

type countingMetrics struct{ replans, simulations map[string]int }

func (m *countingMetrics) FleetCapacityReplan(reason string)  { m.replans[reason]++ }
func (m *countingMetrics) FleetSimulationFailed(stage string) { m.simulations[stage]++ }

// A move that no longer fits the chain at execution ends as capacity_replan,
// once, and frees its vault for the next cycle; it is never deferred into the
// 2-second retry loop. A failure that is not about capacity still retries.
func TestExecutionCapacityFailureReplansInsteadOfRetrying(t *testing.T) {
	const reserveConfig = 4856
	for _, tc := range []struct {
		name   string
		mutate func(source, target *chain.Account)
		reason string
	}{
		{"source fully utilized", func(source, _ *chain.Account) {
			// Everything supplied is borrowed: the collateral still redeems
			// the planned amount, but nothing can be paid out.
			binary.LittleEndian.PutUint64(source.Data[224:232], 0)
			putScaledInteger(source.Data[232:248], 2_000_000_000_000)
			// TrueApr: the plain curve at full utilization stays a priced APY.
			source.Data[reserveConfig+9] = 1
		}, "source_liquidity_exhausted"},
		{"target deposit limit reached", func(_, target *chain.Account) {
			binary.LittleEndian.PutUint64(target.Data[reserveConfig+160:reserveConfig+168], 2_000_000_000_000)
		}, "target_deposit_limit"},
		{"policy account unreadable", func(source, _ *chain.Account) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, lease := fixtureFreshRoute(t, 2_000_000_000_000, tc.mutate)
			lease.OpportunityID, lease.VaultID, lease.RouteKind, lease.DelegatedSigners = 7, 1, "same_mint", []string{r.signer}
			lease.ExpiresAt = time.Now().Add(time.Minute)
			if tc.reason == "" {
				// A lease naming a policy the chain does not hold fails the
				// read for a reason capacity cannot explain.
				lease.PolicyAccount = testIdentity(99)
			}
			store := &replanStore{lease: lease}
			metrics := &countingMetrics{replans: map[string]int{}, simulations: map[string]int{}}
			r.store, r.metrics, r.owner, r.leaseTTL = store, metrics, "owner", time.Minute
			admission, claimed, err := r.PrepareExecution(context.Background(), "localnet")
			if err != nil || !claimed || admission != nil {
				t.Fatalf("admission=%v claimed=%v err=%v", admission, claimed, err)
			}
			if tc.reason == "" {
				if store.retried != 1 || store.replanned != 0 || len(metrics.replans) != 0 {
					t.Fatalf("non-capacity failure: retried=%d replanned=%d replans=%v", store.retried, store.replanned, metrics.replans)
				}
				return
			}
			if store.replanned != 1 || store.retried != 0 || metrics.replans[tc.reason] != 1 {
				t.Fatalf("capacity failure: replanned=%d retried=%d replans=%v", store.replanned, store.retried, metrics.replans)
			}
		})
	}
}

// Inside the admission transaction the fresh reserve capacity is net of the
// moves admitted before this one and not yet landed.
func TestLockedCapacityCountsAdmittedMovesInFlight(t *testing.T) {
	var replan *CapacityReplan
	if err := reserveCapacityShortfall(5, 10, 5, 10, 5); err != nil {
		t.Fatalf("exact fit refused: %v", err)
	}
	if err := reserveCapacityShortfall(5, 10, 6, 100, 0); !errors.As(err, &replan) || replan.Reason != "source_liquidity_exhausted" {
		t.Fatalf("source in flight ignored: %v", err)
	}
	if err := reserveCapacityShortfall(5, 100, 0, 10, 6); !errors.As(err, &replan) || replan.Reason != "target_deposit_limit" {
		t.Fatalf("target in flight ignored: %v", err)
	}
	// The dilution-priced economics the 609 prod deferrals failed on are a
	// re-plan too, not a retry.
	plan := json.RawMessage(`{"observed_target_apy_bps":100,"source_apy_bps":99,"confidence_ppm":950000,"holding_horizon_seconds":2592000,"estimated_execution_cost_usd_micros":100000}`)
	_, err := ComputeAdmissionEconomics(RevalidationLease{PrincipalUSDMicros: 9_000_000_000, ExecutionPlan: plan}, plan, 1_000_000_000, 20_000_000)
	if !errors.As(err, &replan) || replan.Reason != "target_capacity_economics" {
		t.Fatalf("lost economics are not a re-plan: %v", err)
	}
}

// The capacity line names, per reserve that holds capital, what it holds and
// pays, the best same-mint destination and that destination's room after the
// committed and this wave's inflows. It is logged on change and hourly.
func TestCapacityReportNamesTheBestDestinationAndItsRoom(t *testing.T) {
	unlimited := uint64(1 << 62)
	snapshot := packingSnapshot(
		packingReserve("source", 100, 1_000_000_000_000_000, 1_000_000_000_000_000, unlimited),
		packingReserve("target", 900, 1_000_000_000_000, 1_000_000_000_000, uint64(1_000_000_000_000)+12_000_000_000))
	vaults := []FleetVault{packingHolding(snapshot, 1, "source", 10_000_000_000, "target"), packingHolding(snapshot, 2, "source", 5_000_000_000, "target")}
	vaults[0].CommittedInflows = map[string]int64{"target": 1_000_000_000}
	vaults[1].CommittedInflows = vaults[0].CommittedInflows
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	report := capacityReport(snapshot, vaults, vaults[0].CommittedInflows, plan)
	if len(report) != 1 {
		t.Fatalf("report %+v", report)
	}
	line := report[0]
	// Deposit room 12e9 binds below the 20e9 frontier. 1e9 is committed; the
	// 5e9 holding prices into a lower dilution band, earns more per dollar
	// and goes first; the 10e9 holding no longer fits the 6e9 left.
	if got := moves(plan); !sameMoves(got, []string{"2:source>target:5000000000"}) || plan.Rejections[1] != "target_deposit_limit" {
		t.Fatalf("wave %v rejections %v", got, plan.Rejections)
	}
	if line.Reserve != "source" || line.Holdings != 2 || line.HeldUSDMicros != 15_000_000_000 || line.MovableUSDMicros != 15_000_000_000 || line.APYBPS != 100 ||
		line.Best != "target" || line.BestAPYBPS != 900 || line.BestRoomUSDMicros == nil || *line.BestRoomUSDMicros != 6_000_000_000 {
		t.Fatalf("capacity line %+v room %v", line, line.BestRoomUSDMicros)
	}
	var log capacityLog
	start := time.Now()
	if !log.due(start, report) || log.due(start.Add(time.Minute), report) || !log.due(start.Add(capacityHeartbeat+time.Minute), report) {
		t.Fatal("capacity line is not change-only plus hourly")
	}
	report[0].Best = ""
	if !log.due(start.Add(capacityHeartbeat+2*time.Minute), report) {
		t.Fatal("a changed destination was not logged")
	}
}
