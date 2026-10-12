package backyard

import (
	"math/big"
)

// Partial withdrawals (active registry lanes). A withdrawal shortfall S = demand -
// Voltr idle - withdrawal cash already in flight frees only E = S + buffer
// of equity and keeps the position at its level: one down step withdraws the
// collateral share C*E/Eq and converts and repays only its debt share (the
// LTV stays at the level), then the rest converts to USDC and stages to
// Voltr. A shortfall of >= 90% of equity is excluded by this partial planner;
// exclusion does not establish necessity or authority for a full exit. The
// decision is stateless: each leg is chosen from the observed custody, so a
// restart resumes the same chain.
const (
	partialWithdrawalBufferBPS     int64 = 100       // 1% of S ...
	partialWithdrawalMinimumBuffer int64 = 1_000_000 // ... at least $1
	partialWithdrawalFullExitBPS   int64 = 9_000     // E >= 90% equity -> hold for review
	// Collateral or cash below this is rounding residue, not a leg.
	partialWithdrawalDustRaw int64 = 1_000

	partialReleaseReason    = "withdrawal_partial_release"
	partialSwapToUSDCReason = "withdrawal_partial_swap_to_usdc"
	partialStageReason      = "withdrawal_partial_stage"
	// At most this many release rounds (each keeps LTV under the release
	// ceiling); a shortfall under 90% of equity at 1.5x needs <= 6. More
	// requires review, not automatic full exit.
	partialWithdrawalMaxRounds = 6
)

// partialWithdrawalTargetLTVBPS is the LTV the position keeps: the stored
// level's (1.5x = 33.33%); a leveraged position with no stored level keeps
// 1.5x. A 1x target with debt is a down move, handled by the full chain.
func partialWithdrawalTargetLTVBPS(s Snapshot) (int64, bool) {
	if s.PositionDebtRaw <= 0 {
		return 0, true
	}
	if s.LeverageTargetLevel == 1 {
		return 0, false
	}
	if s.PartialWithdrawalOperationID != "" {
		return s.PartialWithdrawalLTVBPS, true
	}
	// An overflow source releases to the overflow LTV (no repay).
	if overflowSourceHalf(s) {
		return overflowSourceLTVBPS, true
	}
	// Only capture with nothing in flight. Legacy in-flight positions keep
	// their old discrete target; new releases persist this actual ratio.
	if s.CollateralIdleRaw <= partialWithdrawalDustRaw && s.SquadsIdleRaw <= partialWithdrawalDustRaw {
		if s.PositionCollateralValueRaw <= s.PositionDebtValueRaw || s.PositionDebtValueRaw <= 0 {
			return 0, false
		}
		ratio := new(big.Int).Mul(big.NewInt(s.PositionDebtValueRaw), big.NewInt(10_000))
		ratio.Quo(ratio, big.NewInt(s.PositionCollateralValueRaw))
		if !ratio.IsInt64() || ratio.Int64() > leverageMaxLTVBPS {
			return 0, false
		}
		return ratio.Int64(), true
	}
	level := s.LeverageTargetLevel
	if level == 0 {
		level = 1.5
	}
	return leverageLevelLTVBPS(level), level >= 1.5
}

// partialWithdrawalStep returns the next partial-withdrawal leg, or ok=false
// when a partial path is not established. Callers hold withdrawal-only flows,
// after hard LTV, recovery and the covered/staged checks.
func partialWithdrawalStep(s Snapshot) (Action, string, int64, bool) {
	// An overflow source frees its E as collateral for the overflow lane.
	if overflowSourceHalf(s) {
		return overflowSourceStep(s)
	}
	if !earnActiveLane(s.RouteLane) || s.Unwind || s.CutoverDrain || (partialDemandRaw(s) <= 0 && !partialWithdrawalInFlight(s)) || !s.HasPosition ||
		s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 || s.PositionDebtRaw < 0 {
		return "", "", 0, false
	}
	// A reconciled stage has left Squads but is not pooled idle yet. Finish
	// only the exact journal-bound partial withdrawal, even if demand shrank.
	if s.VoltrStrategyIdleRaw > 0 {
		if s.PartialWithdrawalOperationID != "" && s.StagedAmountKnown && s.StagedAmountRaw == s.VoltrStrategyIdleRaw {
			return VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw, true
		}
		return "", "", 0, false
	}
	_, ok := partialWithdrawalTargetLTVBPS(s)
	if !ok {
		return "", "", 0, false
	}
	usdc := s.SquadsIdleRaw // withdrawal proceeds waiting to stage
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	// Debt residue beside the position is no withdrawal cash: the next down
	// step's repay sweeps it.
	inflight := usdc + s.CollateralIdleValueRaw
	shortfall := partialDemandRaw(s) - s.VoltrIdleRaw
	if equity <= 0 || (shortfall <= 0 && !partialWithdrawalInFlight(s)) {
		return "", "", 0, false
	}
	// Keep the existing near-total exclusion. The $50 remainder heuristic
	// is not a safety bound: safe release and positive residual checks below
	// and at admission still apply to small positions.
	total := equity + inflight
	if !partialWithdrawalInFlight(s) && (shortfall*10_000 >= total*partialWithdrawalFullExitBPS) {
		return "", "", 0, false
	}
	// Bounded rounds: a shortfall a capped release cannot free within
	// partialWithdrawalMaxRounds rounds remains unproven. Judged against
	// the whole vault position so the answer is stable mid-chain.
	if !partialWithdrawalInFlight(s) && !partialWithdrawalFitsRounds(s, shortfall, total) {
		return "", "", 0, false
	}
	if s.CollateralIdleRaw > partialWithdrawalDustRaw {
		return SwapCollateralToStableStep, partialSwapToUSDCReason, s.CollateralIdleRaw, true
	}
	if usdc > partialWithdrawalDustRaw {
		return StageSquadsToVoltr, partialStageReason, usdc, true
	}
	if shortfall <= 0 {
		return "", "", 0, false
	}
	// Nothing in flight: free E = S + buffer. The decision carries E (USDC
	// raw, stable); prepare converts it to receipts with
	// partialWithdrawalReleaseReceipts, capped at the safe release (a larger
	// E runs further rounds), in one down step beside debt or one withdrawal
	// without it.
	free := shortfall + max(shortfall*partialWithdrawalBufferBPS/10_000, partialWithdrawalMinimumBuffer)
	if partialWithdrawalReleaseReceipts(s, free) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, partialReleaseReason, free, true
}

// partialDemandRaw is the Voltr idle a partial withdrawal frees to: the
// pending withdrawal queue, or a tranche source's idle target, whichever is
// larger. Both are freed by the same debt-preserving legs.
func partialDemandRaw(s Snapshot) int64 {
	return max(s.WithdrawalDemandRaw, s.TrancheTargetIdleRaw)
}

// partialWithdrawalReleaseReceipts converts the USDC target E into receipts
// at the snapshot's values: the collateral share C*E/Eq, capped so C - R >=
// D / (ceiling - 5 pts). The builder's safe-release bound is the hard cap.
func partialWithdrawalReleaseReceipts(s Snapshot, free int64) int64 {
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if free <= 0 || equity <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionCollateralRaw <= 0 {
		return 0
	}
	release := new(big.Int).Mul(big.NewInt(free), big.NewInt(s.PositionCollateralValueRaw))
	release.Quo(release, big.NewInt(equity))
	if !release.IsInt64() {
		return 0
	}
	releaseValue := release.Int64()
	if s.PositionDebtValueRaw > 0 {
		limit := s.PositionCollateralValueRaw - s.PositionDebtValueRaw*10_000/(leverageExitReleaseCeilingBPS-500)
		if limit <= 0 {
			return 0
		}
		releaseValue = min(releaseValue, limit)
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(releaseValue))
	receipts.Quo(receipts, big.NewInt(s.PositionCollateralValueRaw))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}

// partialWithdrawalFitsRounds: at the position's level (value units), each
// round releases up to C - D/(ceiling - 5 pts) and frees that times
// (1 - target LTV) of equity; the rounds shrink C and D proportionally. A
// debt-free position frees everything in one release.
func partialWithdrawalFitsRounds(s Snapshot, shortfall, total int64) bool {
	return partialWithdrawalFitsRoundsWithin(s, shortfall, total, partialWithdrawalMaxRounds)
}

// partialWithdrawalFitsRoundsWithin is partialWithdrawalFitsRounds with an
// explicit round budget (at most partialWithdrawalMaxRounds).
func partialWithdrawalFitsRoundsWithin(s Snapshot, shortfall, total int64, rounds int) bool {
	rounds = min(rounds, partialWithdrawalMaxRounds)
	target, ok := partialWithdrawalTargetLTVBPS(s)
	if !ok {
		return false
	}
	if target == 0 {
		return true
	}
	need := shortfall + max(shortfall*partialWithdrawalBufferBPS/10_000, partialWithdrawalMinimumBuffer)
	// Per unit of equity at the level: C = 1/(1-t), D = t/(1-t).
	c := 10_000.0 / float64(10_000-target)
	d := float64(target) / float64(10_000-target)
	perRound := (c - d*10_000/float64(leverageExitReleaseCeilingBPS-500)) * float64(10_000-target) / 10_000
	if perRound <= 0 {
		return false
	}
	remaining := float64(total)
	freed := 0.0
	for round := 0; round < rounds; round++ {
		step := remaining * perRound
		freed += step
		remaining -= step
		if freed >= float64(need) {
			return true
		}
	}
	return false
}

// partialWithdrawalWireAmount maps a stable decision amount to the exact
// wire amount at prepare time, from the prepared snapshot. ok=false: the
// decision's own amount is the wire amount.
func partialWithdrawalWireAmount(s Snapshot, d Decision) (int64, bool) {
	if d.Reason != partialReleaseReason {
		return 0, false
	}
	if overflowSourceHalf(s) {
		return overflowReleaseReceipts(s, d.AmountRaw), true
	}
	return partialWithdrawalReleaseReceipts(s, d.AmountRaw), true
}
