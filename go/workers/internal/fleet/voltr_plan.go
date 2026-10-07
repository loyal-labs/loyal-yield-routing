package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Ported from voltr_controller.rs and voltr_planning.rs: at most one manager
// leg per confirmed observation, withdrawal liquidity before allocation and
// yield optimization; the next leg is planned only after this one confirms.

const (
	voltrPriorityVersion        = "backyard-voltr-confirmed-one-leg-v1"
	voltrEstimatedCostLamports  = 100_000
	voltrCapacityIncrementShare = 50
)

type voltrPosition struct {
	strategy                  int
	value, redeemable, target uint64
	apy                       int64
}

type voltrLeg struct {
	class     string // withdrawal_restoration | idle_allocation | yield_optimization
	operation string // deposit | withdraw
	strategy  int
	amount    uint64
}

// errVoltrUnrestorable is the controller's UnrestorableShortfall.
var errVoltrUnrestorable = errors.New("voltr withdrawal shortfall has no redeemable source")

// nextVoltrLeg is next_voltr_leg after its RecoverExisting check.
func nextVoltrLeg(idle, required, demand, maximum uint64, positions []voltrPosition, optimizationDue bool) (*voltrLeg, error) {
	if shortfall := satSub(required, idle); shortfall > 0 {
		desired := min(shortfall, maximum)
		var source *voltrPosition
		less := func(a, b *voltrPosition) bool {
			ak, bk := a.redeemable < desired, b.redeemable < desired
			if ak != bk {
				return !ak
			}
			if a.apy != b.apy {
				return a.apy < b.apy
			}
			return a.strategy < b.strategy
		}
		for i := range positions {
			if positions[i].redeemable > 0 && (source == nil || less(&positions[i], source)) {
				source = &positions[i]
			}
		}
		if source == nil {
			return nil, errVoltrUnrestorable
		}
		return &voltrLeg{"withdrawal_restoration", "withdraw", source.strategy, min(desired, source.redeemable)}, nil
	}
	target := voltrTargetDeficit(positions)
	if investable := satSub(idle, required); investable > 0 && target != nil {
		if amount := min(investable, maximum, target.target-target.value); amount > 0 {
			return &voltrLeg{"idle_allocation", "deposit", target.strategy, amount}, nil
		}
	}
	if !optimizationDue || demand != 0 || target == nil {
		return nil, nil
	}
	var source *voltrPosition
	for i := range positions {
		p := &positions[i]
		if p.value > p.target && p.redeemable > 0 && (source == nil || p.apy < source.apy || p.apy == source.apy && p.strategy < source.strategy) {
			source = p
		}
	}
	if source == nil || target.apy <= source.apy {
		return nil, nil
	}
	if amount := min(source.value-source.target, source.redeemable, maximum); amount > 0 {
		return &voltrLeg{"yield_optimization", "withdraw", source.strategy, amount}, nil
	}
	return nil, nil
}

// voltrTargetDeficit is target_deficit: the largest deficit, then the higher
// APY, then the earlier strategy.
func voltrTargetDeficit(positions []voltrPosition) *voltrPosition {
	var best *voltrPosition
	for i := range positions {
		p := &positions[i]
		if p.target <= p.value {
			continue
		}
		if best == nil {
			best = p
			continue
		}
		d, bd := p.target-p.value, best.target-best.value
		if d > bd || d == bd && (p.apy > best.apy || p.apy == best.apy && p.strategy < best.strategy) {
			best = p
		}
	}
	return best
}

// assignVoltrTargets is assign_capacity_adjusted_targets: fill the highest
// APY eligible markets up to their capacity; capital capacity cannot place
// stays where it already sits.
func assignVoltrTargets(positions []voltrPosition, capacity []uint64, eligible []bool, total uint64) {
	order := make([]int, len(positions))
	for i := range order {
		order[i] = i
	}
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && positions[order[j]].apy > positions[order[j-1]].apy; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	remaining := total
	for _, i := range order {
		limit := uint64(0)
		if eligible[i] {
			limit = capacity[i]
		}
		positions[i].target = min(remaining, limit)
		remaining -= positions[i].target
	}
	for i := range positions {
		if remaining == 0 {
			break
		}
		preserved := min(remaining, satSub(positions[i].value, positions[i].target))
		positions[i].target += preserved
		remaining -= preserved
	}
}

// VoltrOpportunity is one planned manager leg as a rebalance opportunity row.
type VoltrOpportunity struct {
	VaultID                                   int64
	Class                                     string
	SourceReserve                             *string
	TargetReserve                             string
	AmountRaw, SourceAPYBPS, TargetAPYBPS     int64
	EdgeBPS, AnnualGainUSDMicros              int64
	RequirementsFingerprint, RouteFingerprint string
	ServiceDeadline                           *time.Time
	ExpiresAt                                 time.Time
	Plan                                      json.RawMessage
}

// errVoltrMarketCoverage defers planning until the market epoch covers all
// four exact reserves with fresh evidence.
var errVoltrMarketCoverage = errors.New("voltr market epoch lacks an exact fresh four-market reserve")

// PlanVoltr is plan_backyard_voltr_opportunity. A nil opportunity is a noop;
// the caller has already ruled out recovery of an existing signed leg.
func PlanVoltr(r VoltrRoute, o VoltrObservation, epoch ImmutableMarketEpoch, vaultID int64, lastOptimization *time.Time, now time.Time) (*VoltrOpportunity, error) {
	positions := make([]voltrPosition, len(r.Strategies))
	capacity := make([]uint64, len(r.Strategies))
	eligible := make([]bool, len(r.Strategies))
	for i, s := range r.Strategies {
		reserve, ok := epoch.Reserve(s.Reserve)
		if !ok || reserve.Market == nil || *reserve.Market != s.LendingMarket || reserve.LiquidityMint != USDCMint || !reserve.EconomicExpiresAt.After(now) {
			return nil, errVoltrMarketCoverage
		}
		available, err := decimalFloor(reserve.AvailableAmountRaw)
		if err != nil {
			return nil, err
		}
		supply, err := decimalFloor(reserve.TotalSupplyAmountRaw)
		if err != nil {
			return nil, err
		}
		positions[i] = voltrPosition{strategy: i, value: o.PositionsRaw[i], redeemable: min(o.PositionsRaw[i], available), apy: reserve.SupplyAPYBPS}
		capacity[i] = satAdd(o.PositionsRaw[i], supply/voltrCapacityIncrementShare)
		eligible[i] = reserve.TargetEligible
	}
	assignVoltrTargets(positions, capacity, eligible, o.TotalValueRaw)
	required := voltrSafetyBufferRaw + o.PendingRaw
	due := lastOptimization == nil || now.Sub(*lastOptimization) >= voltrOptimizationInterval*time.Second
	leg, err := nextVoltrLeg(o.IdleRaw, required, o.PendingRaw, r.MaxOperationRaw, positions, due)
	if err != nil || leg == nil {
		return nil, err
	}
	s := r.Strategies[leg.strategy]
	if leg.amount > math.MaxInt64 {
		return nil, errors.New("voltr amount overflows")
	}
	out := &VoltrOpportunity{VaultID: vaultID, Class: leg.class, AmountRaw: int64(leg.amount), RouteFingerprint: r.BundleSHA256}
	switch leg.class {
	case "withdrawal_restoration":
		if o.EarliestRedeem == 0 || o.EarliestRedeem > math.MaxInt64 {
			return nil, errors.New("voltr restoration has no receipt deadline")
		}
		deadline := time.Unix(int64(o.EarliestRedeem), 0).UTC()
		out.ServiceDeadline = &deadline
	case "idle_allocation":
		if positions[leg.strategy].apy <= 0 {
			return nil, nil
		}
		out.TargetAPYBPS = positions[leg.strategy].apy
	default:
		out.SourceAPYBPS, out.TargetAPYBPS = positions[leg.strategy].apy, positions[leg.strategy].apy
		for _, p := range positions {
			if p.target > p.value && p.apy > out.TargetAPYBPS {
				out.TargetAPYBPS = p.apy
			}
		}
		if out.TargetAPYBPS <= out.SourceAPYBPS {
			return nil, nil
		}
	}
	if leg.class != "withdrawal_restoration" {
		out.EdgeBPS = out.TargetAPYBPS - out.SourceAPYBPS
		if out.EdgeBPS > 0 && out.AmountRaw > math.MaxInt64/out.EdgeBPS {
			return nil, errors.New("voltr economics overflow")
		}
		if out.AnnualGainUSDMicros = out.AmountRaw * out.EdgeBPS / 10_000; out.AnnualGainUSDMicros <= 0 {
			return nil, nil
		}
	}
	marketExpires, complete := epoch.MintExpiresAt(USDCMint)
	if !complete {
		return nil, errVoltrMarketCoverage
	}
	out.ExpiresAt = marketExpires
	if out.ServiceDeadline != nil && out.ServiceDeadline.After(now) && out.ServiceDeadline.Before(marketExpires) {
		out.ExpiresAt = *out.ServiceDeadline
	}
	if !out.ExpiresAt.After(now) {
		return nil, errVoltrMarketCoverage
	}
	sourceKind, targetKind := "voltr_idle", "voltr_strategy"
	if leg.operation == "withdraw" {
		sourceKind, targetKind = targetKind, sourceKind
		reserve := s.Reserve
		out.SourceReserve, out.TargetReserve = &reserve, "voltr_idle:"+r.Vault
	} else {
		out.TargetReserve = s.Reserve
	}
	if out.RequirementsFingerprint, err = r.RequirementsFingerprint(leg.strategy, leg.operation); err != nil {
		return nil, err
	}
	out.Plan, err = json.Marshal(map[string]any{
		"kind": VoltrKind, "route_kind": VoltrKind, "route_id": voltrRouteID, "route_spec_sha256": voltrRouteSpecSHA256,
		"route_bundle_sha256": r.BundleSHA256, "route_fingerprint": r.BundleSHA256, "requirements_fingerprint": out.RequirementsFingerprint,
		"manager": r.Manager, "guardian": r.Guardian, "vault": r.Vault, "strategy_id": s.ID, "operation": leg.operation,
		"source_kind": sourceKind, "target_kind": targetKind, "amount_raw": out.AmountRaw, "max_operation_amount_raw": r.MaxOperationRaw,
		"protected_context_slot": o.ContextSlot, "receipt_set_fingerprint": o.Receipts, "protected_state_sha256": o.State,
		"protected_address_set_sha256": o.Addresses, "intent_sha256": r.IntentSHA256(leg.strategy, leg.operation, leg.amount, o.ContextSlot, o.Receipts, o.State, o.Addresses),
		"pre_total_value_raw": o.TotalValueRaw, "pre_idle_raw": o.IdleRaw, "pre_position_raw": o.PositionsRaw[leg.strategy],
		"conflict_account_keys": []string{"voltr:vault:" + r.Vault, "kamino:reserve:" + s.Reserve},
	})
	return out, err
}

// VoltrPlanningState is voltr_vault_planning_state: a nonterminal signed
// Voltr leg defers planning to its recovery; the last normal optimization
// paces the next one.
func (s *Store) VoltrPlanningState(ctx context.Context, vaultID int64) (bool, *time.Time, error) {
	var pending bool
	var last *time.Time
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id
  WHERE o.vault_id=$1 AND o.execution_plan->>'kind'='voltr_kamino' AND s.submission_state NOT IN ('reconciled','expired','failed')),
 (SELECT max(o.created_at) FROM loyal_yield.rebalance_opportunities o WHERE o.vault_id=$1 AND o.execution_plan->>'kind'='voltr_kamino'
  AND o.operation_class='yield_optimization' AND o.opportunity_state NOT IN ('stale','superseded','failed','cancelled'))`, vaultID).Scan(&pending, &last)
	return pending, last, err
}

// PublishVoltr is upsert_rebalance_opportunity for one Voltr leg: each new
// observation supersedes the vault's unclaimed opportunity; a live lease on
// another opportunity defers it.
func (s *Store) PublishVoltr(ctx context.Context, cluster string, epochID int64, v VoltrOpportunity) (bool, error) {
	key := voltrOpportunityKey(cluster, epochID, v)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var active, ready bool
	if err := tx.QueryRow(ctx, `SELECT active FROM loyal_yield.managed_vaults WHERE id=$1 FOR UPDATE`, v.VaultID).Scan(&active); err != nil || !active {
		return false, fmt.Errorf("voltr vault %d is missing or inactive: %v", v.VaultID, err)
	}
	if err := tx.QueryRow(ctx, `SELECT cluster=$2 AND expires_at>=$3 AND expires_at>=clock_timestamp()+interval '60 seconds' AND $3>=clock_timestamp()+interval '60 seconds' FROM loyal_yield.optimizer_epochs WHERE id=$1 FOR SHARE`, epochID, cluster, v.ExpiresAt).Scan(&ready); err != nil || !ready {
		return false, err
	}
	var exists, leased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities WHERE rediscovery_key=$3),
  EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND vault_id=$2 AND rediscovery_key<>$3 AND opportunity_state='leased' AND lease_expires_at>now())`, cluster, v.VaultID, key).Scan(&exists, &leased); err != nil || exists || leased {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='superseded',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,terminal_reason='newer_opportunity_published',updated_at=now()
  WHERE cluster=$1 AND vault_id=$2 AND rediscovery_key<>$3 AND opportunity_state IN ('waiting_alt','revalidate','ready','leased') AND (opportunity_state<>'leased' OR lease_expires_at<=now())`, cluster, v.VaultID, key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.rebalance_opportunities
(cluster,idempotency_key,rediscovery_key,attempt_generation,vault_id,optimizer_epoch_id,route_fingerprint,requirements_fingerprint,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,principal_usd_micros,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,annual_yield_gain_usd_micros,expected_net_gain_usd_micros,economic_priority,priority_version,operation_class,service_deadline_at,opportunity_state,execution_plan,available_at,expires_at)
VALUES($1,$2,$2,1,$3,$4,$5,$6,$7,$8,$9,$9,$9,$10,$10,$11,$12,$13,$14,$15,$15,$15,$16,$17,$18,'revalidate',$19,clock_timestamp(),$20)`,
		cluster, key, v.VaultID, epochID, v.RouteFingerprint, v.RequirementsFingerprint, v.SourceReserve, v.TargetReserve, USDCMint, v.AmountRaw,
		v.SourceAPYBPS, v.TargetAPYBPS, v.EdgeBPS, int64(voltrEstimatedCostLamports), v.AnnualGainUSDMicros, voltrPriorityVersion, v.Class, v.ServiceDeadline, v.Plan, v.ExpiresAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// voltrOpportunityKey follows rebalance_opportunity_idempotency_key's shape:
// length-prefixed immutable evidence, so a re-observation with the same plan
// is the same opportunity.
func voltrOpportunityKey(cluster string, epochID int64, v VoltrOpportunity) string {
	h := sha256.New()
	for _, part := range []string{"loyal-rebalance-opportunity-v1", cluster, strconv.FormatInt(v.VaultID, 10), "idle", strconv.FormatInt(epochID, 10), string(v.Plan), v.ExpiresAt.UTC().Format(time.RFC3339Nano)} {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func decimalFloor(value string) (uint64, error) {
	integer, _, _ := strings.Cut(value, ".")
	n, err := strconv.ParseUint(integer, 10, 64)
	if err != nil {
		return 0, errVoltrMarketCoverage
	}
	return n, nil
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func satAdd(a, b uint64) uint64 {
	if a+b < a {
		return math.MaxUint64
	}
	return a + b
}
