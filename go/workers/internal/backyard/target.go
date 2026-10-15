package backyard

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Backyard has one target and one step.
//
// target(chain, rates, inputs) is a pure function computed every tick and
// never stored. Its inputs besides the chain are the lane levels and the
// operator exits (target_inputs.go) and the selector's latest result (the
// rates' choice of a lane for the vault's cash, or of a held lane's tranche
// to rotate into another). For every candidate lane it
// says where that lane should be, and it names the one lane the next step
// moves. Its rules, in priority order:
//
//  1. hard LTV: a lane at or past the lower of its liquidation threshold -
//     10 points and its max LTV - 3 points goes back to its level's LTV;
//  2. term maturity: term debt under a day from maturity without room for the
//     keep-alive borrow (or past it) goes to debt 0; under three days with
//     room, the keep-alive borrow (the minimum up step) restarts the term;
//  3. the operator exit row: debt 0, and with equity_zero the collateral
//     leaves as USDC (one exit step: withdraw and swap);
//  4. custody in flight: collateral waiting for a held lane's next leg goes
//     first, and debt-mint cash no held lane owes converts toward Voltr;
//     withdrawal demand: Voltr idle must cover the queue, and the largest
//     lane frees the rest at its level;
//  5. the selector's allocation: an overflow lane takes the collateral its
//     source cannot lever (one atomic move); the vault's cash enters the
//     selector's current destination, a held lane included; a rotation
//     releases its tranche from the source as Squads cash, which that rule
//     then enters (the selector records no new result until it has); with
//     no destination named, a USDC-debt home lane's step excess joins its
//     position;
//  6. the level setting: each lane steps toward its stored level.
//
// An ordinary economic target never goes to debt 0: a stored level of 1 is
// held at 1.5, because borrowing room may not come back. Debt 0 comes only
// from hard LTV's emergency, term maturity, or the operator exit row.
//
// step (Decide) returns the next atomic transaction toward the working lane's
// target, or a hold whose reason is one of a small set: at_target, no_room,
// stale. A manual recovery hold stops the route.

// Target rules.
const (
	ruleHardLTV       = "hard_ltv"
	ruleTermUnknown   = "term_unknown"
	ruleTermMaturity  = "term_maturity"
	ruleTermKeepAlive = "term_keepalive"
	ruleOperatorExit  = "operator_exit"
	ruleCustody       = "custody"
	ruleWithdrawal    = "withdrawal"
	ruleOverflow      = "overflow"
	ruleLevel         = "level"
	ruleRotate        = "rotate"
	ruleEntry         = "entry"
	ruleHold          = "at_target"
)

// Decision reasons of the target's moves.
const (
	// A lane's debt goes to 0 (term maturity, operator exit), by down steps.
	debtZeroReason = "debt_zero"
	// A lane frees equity toward Voltr at its own level (withdrawal).
	releaseReason = "withdrawal_release"
	// A lane's collateral moves to its overflow lane in one transaction.
	overflowMoveReason = "overflow_move"
	// A rotation's source frees its tranche at its own level as Squads
	// cash: it repays the debt share, then withdraws the collateral share
	// as USDC.
	rotateRepayReason    = "rotate_repay"
	rotateWithdrawReason = "rotate_withdraw"
	// Lane custody and Squads cash leave toward Voltr.
	outboundCollateralReason = "outbound_collateral_to_usdc"
	outboundDebtReason       = "outbound_debt_residue"
	outboundStageReason      = "outbound_stage"
	exitWithdrawReason       = "exit_withdraw_collateral"
	// An entry or top-up's cash.
	entryAllocationReason = "eligible_voltr_idle"
	entrySwapReason       = "usdc_requires_collateral"
	entryDepositReason    = "collateral_ready"
	returnCashReason      = "entry_capacity_changed_return_cash"
)

// LaneTarget is where target() puts the working lane this tick, and the rule
// that put it there.
type LaneTarget struct {
	Rule string `json:"rule"`
	// Level is the leverage the lane sits at: 0 leaves the position as it is,
	// 1 repays all debt, a level above it is that level's LTV band.
	Level      float64 `json:"level,omitempty"`
	UpApproved bool    `json:"upApproved,omitempty"`
	KeepAlive  bool    `json:"keepAlive,omitempty"`
	// EquityZero withdraws the lane's collateral as USDC after its debt.
	EquityZero bool `json:"equityZero,omitempty"`
	// Outbound sends the lane's idle custody and Squads cash toward Voltr.
	Outbound bool `json:"outbound,omitempty"`
	// ReleaseRaw is the equity (bridge USDC value) the lane frees at its own
	// level toward Voltr.
	ReleaseRaw int64 `json:"releaseRaw,omitempty"`
	// RepayRaw (debt) then WithdrawRaw (collateral receipts) are a
	// rotation's release from the lane: the USDC stays in the Squads account
	// for the cash rule to enter.
	RepayRaw    int64 `json:"repayRaw,omitempty"`
	WithdrawRaw int64 `json:"withdrawRaw,omitempty"`
	// MoveRaw is the collateral value the lane moves to MoveTo, its overflow
	// lane, in one transaction.
	MoveRaw int64  `json:"moveRaw,omitempty"`
	MoveTo  string `json:"moveTo,omitempty"`
	// EntryRaw is Voltr idle allocated to the lane: an entry, beside its
	// position or not.
	EntryRaw int64 `json:"entryRaw,omitempty"`
}

// hardLTVBPS is a lane's hard LTV, the brake (owner decisions 2026-10-14):
// the lower of its collateral reserve's liquidation threshold less 10
// points and its loan-to-value less stepWithdrawMarginBPS, where a down step
// still has withdraw room. The lane's cap sits under it.
func hardLTVBPS(liquidationThresholdBPS, maxLTVBPS int64) int64 {
	return min(liquidationThresholdBPS-1000, maxLTVBPS-stepWithdrawMarginBPS)
}

// laneEquityRaw is the lane position's own equity in bridge USDC.
func laneEquityRaw(s Snapshot) int64 {
	return s.PositionCollateralValueRaw - s.PositionDebtValueRaw
}

// laneInFlight reports collateral custody the lane's next leg takes: above
// dust, and at least the deposit KLend accepts (one collateral receipt) when
// that leg is a deposit. Less is vault cash, counted in NAV, that the lane's
// next step takes whole.
func laneInFlight(s Snapshot) bool {
	return s.CollateralIdleRaw > partialWithdrawalDustRaw && s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw
}

// debtCashOwed reports a held lane that steps and owes the mint of s's debt
// custody (the vault's one account of that mint): its next step takes that
// cash whole.
func debtCashOwed(lanes []targetLane, s Snapshot) bool {
	lane, ok := findEarnLane(s.RouteLane)
	for _, l := range lanes {
		other, otherOK := findEarnLane(l.s.RouteLane)
		if ok && otherOK && l.held && !other.exitOnly && l.s.PositionDebtRaw > 0 &&
			other.route.Kamino.DebtMint == lane.route.Kamino.DebtMint && other.route.DebtTokenProgram == lane.route.DebtTokenProgram {
			return true
		}
	}
	return false
}

// targetLane is one candidate lane's view and its stored inputs.
type targetLane struct {
	s     Snapshot
	held  bool
	level laneLevel
	exit  *laneExit
}

// target computes the vault's target over the tick's per-lane views (one per
// held lane, plus the candidates an overflow or entry may move into, in
// registry order) and returns the index of the working lane and its target.
func target(views []Snapshot, in targetInputs) (int, LaneTarget) {
	if len(views) == 0 {
		return -1, LaneTarget{}
	}
	lanes := make([]targetLane, len(views))
	held := views[0].heldLanes()
	for i, s := range views {
		lanes[i] = targetLane{s: s, held: containsLane(held, s.RouteLane), level: in.Levels[s.RouteLane]}
		if exit, ok := in.Exits[s.RouteLane]; ok {
			lanes[i].exit = &exit
		}
	}
	vault := views[0]
	stored := func(l targetLane) LaneTarget {
		return LaneTarget{Rule: ruleHold, Level: heldLevel(l.s.RouteLane, l.level.Level), UpApproved: l.level.UpApproved && l.level.Level > 1}
	}
	// 1. Hard LTV: the lane furthest past it.
	worst, excess := -1, int64(-1)
	for i, l := range lanes {
		s := l.s
		if hard := hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS); l.held && s.HasPosition && s.PositionDebtRaw > 0 && s.LTVBPS >= hard && s.LTVBPS-hard > excess {
			worst, excess = i, s.LTVBPS-hard
		}
	}
	if worst >= 0 {
		t := stored(lanes[worst])
		t.Rule, t.Level = ruleHardLTV, heldLevel(lanes[worst].s.RouteLane, max(lanes[worst].level.Level, 1.5))
		return worst, t
	}
	shortfall := vault.WithdrawalDemandRaw - vault.VoltrIdleRaw
	outbound := shortfall > 0
	// 2. Term maturity. The keep-alive waits for an unfunded withdrawal until
	// the term's last day.
	for i, l := range lanes {
		s := l.s
		if !l.held || s.PositionDebtRaw <= 0 {
			continue
		}
		if termDebtUnknown(s) {
			return i, LaneTarget{Rule: ruleTermUnknown}
		}
		if termDeleverDue(s) {
			return i, LaneTarget{Rule: ruleTermMaturity, Level: 1}
		}
		if left, ok := termSecondsLeft(s); ok && left < termKeepAliveSeconds && termKeepAliveRoom(s) && (!outbound || left < termDeleverSeconds) {
			t := stored(l)
			t.Rule, t.KeepAlive = ruleTermKeepAlive, true
			return i, t
		}
	}
	// 3. Operator exits.
	for i, l := range lanes {
		s := l.s
		if l.exit == nil || !l.held {
			continue
		}
		if s.PositionDebtRaw > 0 || (l.exit.EquityZero && s.PositionCollateralRaw > partialWithdrawalDustRaw) {
			return i, LaneTarget{Rule: ruleOperatorExit, Level: 1, EquityZero: l.exit.EquityZero, Outbound: l.exit.EquityZero}
		}
	}
	// 4a. Custody in flight finishes first, in its direction: collateral a
	// held lane's next leg takes, and debt-mint cash above dust that a
	// withdrawal needs or no held lane owes, which leaves toward Voltr (cash
	// a held lane owes otherwise waits for its next step).
	for i, l := range lanes {
		inFlight := l.held && laneInFlight(l.s)
		loose := !inFlight && l.s.DebtIdleRaw > partialWithdrawalDustRaw && (outbound || !debtCashOwed(lanes, l.s))
		if !inFlight && !loose {
			continue
		}
		t := stored(l)
		t.Rule, t.Outbound = ruleCustody, outbound || loose || (l.exit != nil && l.exit.EquityZero)
		return i, t
	}
	// 4b. Withdrawal: Squads cash stages; the lane with the most equity frees
	// the rest, up to under 90% of its own equity (more is an exit), and the
	// next lane frees what is still missing on a later tick.
	if outbound {
		if gap := shortfall - vault.SquadsIdleRaw; gap > 0 {
			if i := releaseLane(lanes); i >= 0 {
				free := gap + max(gap*partialWithdrawalBufferBPS/10_000, partialWithdrawalMinimumBuffer)
				t := stored(lanes[i])
				t.Rule, t.Outbound, t.ReleaseRaw = ruleWithdrawal, true, min(free, laneReleaseLimitRaw(lanes[i].s))
				return i, t
			}
		}
		i := max(homeLane(lanes), 0)
		return i, LaneTarget{Rule: ruleWithdrawal, Outbound: true}
	}
	// 5r. Rotation: the selector's current rotation releases its tranche
	// from the source as Squads cash, from the position it priced to the
	// position it leaves, while its destination is viewed and not exiting;
	// rule 5 then enters that cash. It
	// finishes before an overflow move reads the source's level mid-release.
	for i, l := range lanes {
		if repay, withdraw := rotationReleaseStep(in.Entry, l.s); l.held && l.exit == nil && (repay > 0 || withdraw > 0) {
			if j, _ := entryLane(lanes, in); j < 0 {
				// The destination is unviewed or exiting: the source keeps it.
				break
			}
			t := stored(l)
			t.Rule, t.RepayRaw, t.WithdrawRaw = ruleRotate, repay, withdraw
			return i, t
		}
	}
	// 5a. Overflow: a source whose own reserve cannot lever its equity moves
	// collateral to its overflow lane, which levers it there.
	for i, l := range lanes {
		if lane, e := overflowMove(lanes, i, in); e > 0 {
			t := stored(l)
			t.Rule, t.MoveRaw, t.MoveTo = ruleOverflow, e, lane
			return i, t
		}
	}
	// 5. Cash: the vault's Squads cash, and the Voltr idle a current entry
	// quote allocates, enter the selector's current destination, a held lane
	// included. A flat vault's view is that lane's. The Squads cash is the
	// destination's while the selector names one, viewed or not; with none,
	// a USDC-debt home lane's step excess joins its position, and any other
	// cash waits for the selector.
	if i, raw := entryLane(lanes, in); i >= 0 && (vault.SquadsIdleRaw > 0 || raw > 0 || !anyHeld(lanes)) {
		t := stored(lanes[i])
		t.Rule, t.EntryRaw = ruleEntry, raw
		return i, t
	}
	if home := homeLane(lanes); home >= 0 && vault.SquadsIdleRaw > 0 && selectorDestination(in) == "" &&
		sharedUSDCDebt(lanes[home].s.RouteLane) && lanes[home].s.PositionDebtRaw > 0 {
		t := stored(lanes[home])
		t.Rule = ruleEntry
		return home, t
	}
	// 6. Levels: down to the stored level, or up toward it with approval and
	// room.
	for i, l := range lanes {
		if !l.held || l.exit != nil || l.level.Level == 0 {
			continue
		}
		t := stored(l)
		t.Rule = ruleLevel
		if levelDownDue(l.s, t) || (t.UpApproved && !l.s.BorrowUtilizationBlocked && leverageUpReceive(l.s, t) >= leverageMinimumBorrowRaw) {
			return i, t
		}
	}
	// Otherwise the vault holds, described by its home lane at its stored
	// level.
	if home := homeLane(lanes); home >= 0 {
		t := stored(lanes[home])
		t.Rule = ruleHold
		return home, t
	}
	return 0, LaneTarget{Rule: ruleHold}
}

// homeLane is the lane a held vault's hold describes: the held active lane
// with the most equity that is no overflow lane and is not exiting; -1
// without one.
func homeLane(lanes []targetLane) int {
	best, equity := -1, int64(-1)
	for i, l := range lanes {
		if !l.held || l.exit != nil || !selectorDestinationLane(l.s.RouteLane) || !l.s.HasPosition {
			continue
		}
		if e := laneEquityRaw(l.s); e > equity {
			best, equity = i, e
		}
	}
	return best
}

func anyHeld(lanes []targetLane) bool {
	for _, l := range lanes {
		if l.held {
			return true
		}
	}
	return false
}

// releaseLane is the lane a withdrawal frees from: the held lane with the
// most equity, an overflow lane included.
func releaseLane(lanes []targetLane) int {
	best, equity := -1, int64(0)
	for i, l := range lanes {
		if l.held && l.s.HasPosition && earnHeldLane(l.s.RouteLane) {
			if e := laneEquityRaw(l.s); e > equity {
				best, equity = i, e
			}
		}
	}
	return best
}

// laneReleaseLimitRaw is the most equity a release frees from s: under 90%
// of its equity (90% or more is the operator's exit).
func laneReleaseLimitRaw(s Snapshot) int64 {
	return max((laneEquityRaw(s)*partialWithdrawalFullExitBPS-1)/10_000, 0)
}

// overflowMove is the overflow lane a held source i moves collateral to, and
// how much (bridge USDC value): the source is below its stored level with no
// borrow room of its own, its overflow lane's reserve has room and its stored
// level approves levering there, and neither lane is exiting or near its term.
// The source's stored level is its cap, whose LTV the move releases to, and the
// overflow lane levers what an earlier move brought before the next move:
// each move is sized by the room the overflow reserve still has.
func overflowMove(lanes []targetLane, i int, in targetInputs) (string, int64) {
	l := lanes[i]
	s := l.s
	lane, ok := overflowLaneOf(s.RouteLane)
	if !ok || !l.held || l.exit != nil || l.level.Level != laneMaxLevel(s.RouteLane) || !s.HasPosition {
		return "", 0
	}
	if _, exiting := in.Exits[lane]; exiting {
		return "", 0
	}
	dest := in.Levels[lane]
	for _, other := range lanes {
		if t := other.s; other.held && t.RouteLane == lane && (termDebtUnknown(t) || termDeleverDue(t) || leverageUpLevel(withLevel(t, dest.Level)) > 0) {
			return "", 0
		}
	}
	if leverageUpLevel(withLevel(s, l.level.Level)) == 0 || !overflowSourceBorrowClosed(s) || !s.OverflowRoomKnown || s.OverflowRoomRaw < leverageMinimumBorrowRaw ||
		dest.Level < 1.5 || !dest.UpApproved {
		return "", 0
	}
	e := overflowEquityRaw(s)
	if e <= 0 || overflowReleaseReceipts(s, e) <= 0 {
		return "", 0
	}
	return lane, e
}

// entryLane is where the vault's cash enters: the selector's latest
// destination among the candidates (an entry's, or a rotation's), and the
// Voltr idle an entry quote allocates while it is current at this batch and
// the idle beyond the queue covers it (0 otherwise; Squads cash already
// allocated or released still goes to that lane).
func entryLane(lanes []targetLane, in targetInputs) (int, int64) {
	r := in.Entry
	if r == nil || (r.Action != "ENTER" && r.Action != "CANARY_ENTER" && !selectorRotates(*r)) || r.SelectedQuote == nil || !earnActiveLane(r.DestinationLane) {
		return -1, 0
	}
	for i, l := range lanes {
		if l.s.RouteLane != r.DestinationLane || l.exit != nil {
			continue
		}
		s, q := l.s, r.SelectedQuote
		raw := int64(0)
		if !selectorRotates(*r) && q.DestinationLane == s.RouteLane && q.currentAtSlot(s.Slot) && q.EquityRaw > 0 && q.CostRaw >= 0 && q.CostRaw < q.EquityRaw && q.validBorrow() &&
			q.EquityRaw <= s.VoltrIdleRaw-s.WithdrawalDemandRaw {
			raw = q.EquityRaw
		}
		return i, raw
	}
	return -1, 0
}

// SourceRelease is a rotation's release from its source, sized when the
// selector decided it: the position it priced (receipts and debt raw) and the
// position it leaves, at the position's own level.
type SourceRelease struct {
	CollateralRaw       int64 `json:"collateralRaw"`
	DebtRaw             int64 `json:"debtRaw"`
	CollateralTargetRaw int64 `json:"collateralTargetRaw"`
	DebtTargetRaw       int64 `json:"debtTargetRaw"`
}

// newSourceRelease sizes the release of equity e (bridge USDC value) from
// s: its share e/Eq of the collateral and of the debt, below the 90% of its
// equity that keeps it a lane. Nil when s cannot release it.
func newSourceRelease(s Snapshot, e int64) *SourceRelease {
	e = min(e, laneReleaseLimitRaw(s))
	receipts := partialWithdrawalReleaseReceipts(s, e)
	equity := laneEquityRaw(s)
	if receipts <= 0 || equity <= 0 {
		return nil
	}
	repay := new(big.Int).Mul(big.NewInt(s.PositionDebtRaw), big.NewInt(e))
	repay.Quo(repay, big.NewInt(equity))
	if !repay.IsInt64() || repay.Int64() > s.PositionDebtRaw {
		return nil
	}
	return &SourceRelease{CollateralRaw: s.PositionCollateralRaw, DebtRaw: s.PositionDebtRaw,
		CollateralTargetRaw: s.PositionCollateralRaw - receipts, DebtTargetRaw: s.PositionDebtRaw - repay.Int64()}
}

// rotationReleaseStep is the release r's rotation has left on s, its
// source: the debt still above its target (repaid first; a hundredth of the
// planned repay absorbs interest), then the collateral receipts above
// theirs. Nothing once the release is done, when r is not current within
// MaxSampleGap of s's chain time, or when s holds more collateral than the
// position r priced. The selector records no new result while one is left
// (RecordSelectorEvaluation), so a release finishes from the result that
// started it.
func rotationReleaseStep(r *SelectorResult, s Snapshot) (repay, withdraw int64) {
	if !rotationCurrent(r, s) || s.RouteLane != r.SourceLane || !s.HasPosition {
		return 0, 0
	}
	rel := r.SourceRelease
	if s.PositionCollateralRaw > rel.CollateralRaw {
		return 0, 0
	}
	if s.PositionDebtRaw-rel.DebtTargetRaw > partialWithdrawalDustRaw+(rel.DebtRaw-rel.DebtTargetRaw)/100 {
		return s.PositionDebtRaw - rel.DebtTargetRaw, 0
	}
	if s.PositionCollateralRaw-rel.CollateralTargetRaw > partialWithdrawalDustRaw {
		return 0, s.PositionCollateralRaw - rel.CollateralTargetRaw
	}
	return 0, 0
}

// rotationCurrent reports a rotation r sampled within MaxSampleGap of s's
// chain time.
func rotationCurrent(r *SelectorResult, s Snapshot) bool {
	if r == nil || !selectorRotates(*r) || r.SelectedQuote == nil || r.SourceRelease == nil || r.SampledAt.IsZero() || s.ChainUnix <= 0 {
		return false
	}
	gap := DefaultSelectorPolicy().MaxSampleGap
	age := time.Unix(s.ChainUnix, 0).Sub(r.SampledAt)
	return age >= -gap && age <= gap
}

// rotationLeft reports a current rotation r that has not finished on the
// vault: its release is left on its source, or Squads cash (the release's
// USDC) waits for the cash rule to enter it in r's destination. The selector
// records no new result while one is left (RecordSelectorEvaluation), so the
// cash enters the destination the release was for, and no next tranche is
// released while this one's cash is idle.
func rotationLeft(r *SelectorResult, vault Snapshot, lanes []Snapshot) bool {
	for _, l := range lanes {
		if repay, withdraw := rotationReleaseStep(r, l); repay > 0 || withdraw > 0 {
			return true
		}
	}
	return rotationCurrent(r, vault) && vault.SquadsIdleRaw > 0
}

// levelDownDue: a held level below the position's band, with debt.
func levelDownDue(s Snapshot, t LaneTarget) bool {
	band := leverageLevelLTVBPS(t.Level)
	return t.Level >= 1.5 && s.HasPosition && s.PositionDebtRaw > 0 && s.LTVBPS > band+leverageUpNearBPS && stepDebtToLTVRaw(s, band) > 0
}

// leverageUpReceive is the same-batch capacity-sized borrow toward the
// target's level, 0 without an approval.
func leverageUpReceive(s Snapshot, t LaneTarget) uint64 {
	if !t.UpApproved {
		return 0
	}
	probe := s
	probe.LeverageTargetLevel = t.Level
	level := leverageUpLevel(probe)
	if level == 0 {
		return 0
	}
	receive := leverageBorrowCeiling(s, level)
	if receive < leverageMinimumBorrowRaw {
		return 0
	}
	return receive
}

// targetSummary names every candidate lane's stored inputs for the decision
// evidence: lane=level[+up][!exit].
func targetSummary(views []Snapshot, in targetInputs) string {
	parts := make([]string, 0, len(views))
	for _, s := range views {
		l := in.Levels[s.RouteLane]
		part := fmt.Sprintf("%s=%g", s.RouteLane, l.Level)
		if l.UpApproved {
			part += "+up"
		}
		if exit, ok := in.Exits[s.RouteLane]; ok {
			part += "!exit"
			if exit.EquityZero {
				part += "-all"
			}
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Decide is step(chain, target): the next atomic transaction toward the
// working lane's target (s.Target), or a hold.
func Decide(s Snapshot) Decision {
	return decideSnapshot(s, initializationSnapshotReady)
}

func decideSnapshot(s Snapshot, initializationReady func(Snapshot) bool) Decision {
	if s.Trustful {
		// The receipt is the fact: it must be the strategy's.
		if s.MonitorsArmed && s.Nonterminal == "" && s.StrategyReceiptIntegrityFault {
			return monitorHold(s, "strategy_receipt_integrity")
		}
	} else {
		if hold, blocked := custodyDiscipline(s); blocked {
			return hold
		}
		if hold, blocked := bridgeMonitorHold(s); blocked {
			return hold
		}
	}
	d := func(action Action, reason string, amount int64) Decision {
		return Decision{Action: action, Reason: reason, AmountRaw: amount, StrategyKey: s.RouteLane,
			IdempotencyKey: fmt.Sprintf("%s:%s:%s:%d:%s", s.ObservationID, s.RouteLane, action, amount, reason)}
	}
	if s.Nonterminal != "" {
		reason := "resume_nonterminal_operation"
		if s.HasAmbiguousSubmission {
			reason = "recover_ambiguous_submission"
		}
		return d(RecoverTransaction, reason, 0)
	}
	if !earnHeldLane(s.RouteLane) {
		return Decision{Action: HoldManualRecovery, Reason: "unsupported_runtime_lane", StrategyKey: s.RouteLane,
			IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "unsupported_runtime_lane")}
	}
	if s.ManualReason != "" || s.RouteKind != RouteKind || !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.LiquidationThresholdBPS > 10_000 || s.MaxLTVBPS > 10_000 {
		return d(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
	}
	for _, value := range []int64{s.WithdrawalDemandRaw, s.SquadsIdleRaw, s.CollateralIdleRaw, s.CollateralIdleValueRaw, s.MinimumCollateralDepositRaw, s.DebtIdleRaw, s.PayoffDebtRaw,
		s.VoltrStrategyIdleRaw, s.VoltrIdleRaw, s.PositionCollateralRaw, s.PositionDebtRaw, s.PositionCollateralValueRaw, s.PositionDebtValueRaw,
		s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.LTVBPS, s.LiquidationThresholdBPS, s.MaxLTVBPS, s.LastReportAgeSeconds, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw} {
		if value < 0 {
			return d(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
		}
	}
	if s.HasPosition != (s.PositionCollateralRaw > 0 || s.PositionDebtRaw > 0) {
		return d(HoldManualRecovery, "position_presence_mismatch", 0)
	}
	hard := hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS)
	if s.HasPosition && hard <= laneCeilingLTVBPS(s.RouteLane) {
		return d(HoldManualRecovery, "invalid_hard_ltv", 0)
	}
	t := s.Target
	// Risk first: one down step back to the level's LTV.
	// (LTV is 0 without debt.)
	if s.HasPosition && s.LTVBPS >= hard {
		return d(DeleverRouteStep, hardLTVDeleverReason, stepDownMarker)
	}
	if termDebtUnknown(s) {
		return d(Hold, holdStale, 0)
	}
	// A journal-explained stage finishes before anything reads Voltr idle.
	// (A Trustful withdrawal is one transaction: it stages nothing.)
	if !s.Trustful && s.VoltrStrategyIdleRaw > 0 {
		return d(VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw)
	}
	// Strategy custody is empty here (restored above), so a report observes
	// it empty.
	report := func(reason string) Decision { return d(ReportNAV, reason, 0) }
	if !s.Trustful && s.PostMutationNAVRequired && !withdrawalIdleUnderfunded(s) {
		return report("post_mutation_nav_due")
	}
	// Debt 0: term maturity or the operator exit.
	if t.Level == 1 && s.PositionDebtRaw > 0 {
		return d(DeleverRouteStep, debtZeroReason, stepDownMarker)
	}
	// Voltr idle already pays the queue: keep the report current for claims.
	if s.WithdrawalDemandRaw > 0 && s.WithdrawalDemandRaw <= s.VoltrIdleRaw {
		if s.Trustful && trustfulReportDue(s) || !s.Trustful && (s.CapitalMutated || s.LastReportAgeSeconds >= 60) {
			return report("withdrawal_covered_nav_due")
		}
		return d(Hold, holdAtTarget, 0)
	}
	if t.Outbound {
		if next, ok := outboundStep(s, t, d); ok {
			return next
		}
	}
	// A rotation's release, at the source's own level.
	if t.RepayRaw > 0 {
		return d(DeleverRouteStep, rotateRepayReason, t.RepayRaw)
	}
	if t.WithdrawRaw > 0 {
		return d(DeleverRouteStep, rotateWithdrawReason, t.WithdrawRaw)
	}
	if s.Trustful {
		if trustfulReportDue(s) {
			return report("nav_due")
		}
	} else {
		if hold, drifted := unexplainedNAVDriftHold(s); drifted {
			return hold
		}
		if capitalMutationReports(s) || (scheduledNAVReportDue(s) && !(t.Rule == ruleEntry && t.EntryRaw > 0)) {
			return report("nav_due")
		}
	}
	if levelDownDue(s, t) {
		return d(DeleverRouteStep, leverageDownPartialReason, stepDownMarker)
	}
	if t.MoveRaw > 0 {
		if !s.OverflowObligationPresent {
			// The overflow lane's obligation comes first; it moves no money.
			return Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: t.MoveTo,
				IdempotencyKey: fmt.Sprintf("%s:initialize:%s", s.ObservationID, t.MoveTo)}
		}
		return d(DeleverRouteStep, overflowMoveReason, t.MoveRaw)
	}
	if hold, absent := obligationPrerequisiteHold(s, initializationReady); absent {
		return hold
	}
	return inboundStep(s, t, hard, d)
}

// outboundStep moves the working lane's custody and the vault's Squads cash
// toward Voltr, then frees what the target releases; false when nothing is
// left to move.
func outboundStep(s Snapshot, t LaneTarget, d func(Action, string, int64) Decision) (Decision, bool) {
	if s.CollateralIdleRaw > partialWithdrawalDustRaw {
		if !laneSwap(s.RouteLane, SwapCollateralToStableStep) {
			return d(Hold, holdNoRoom, 0), true
		}
		return d(SwapCollateralToStableStep, outboundCollateralReason, s.CollateralIdleRaw), true
	}
	if s.DebtIdleRaw > partialWithdrawalDustRaw {
		if debtToUSDCConvertible(s.RouteLane) {
			return d(SwapDebtToUSDCStep, outboundDebtReason, s.DebtIdleRaw), true
		}
		// Hastra debt never redeems to USDC: it stakes to collateral first.
		return d(SwapDebtToCollateralStep, outboundDebtReason, s.DebtIdleRaw), true
	}
	if s.SquadsIdleRaw > 0 {
		return d(StageSquadsToVoltr, outboundStageReason, s.SquadsIdleRaw), true
	}
	if t.EquityZero && s.PositionDebtRaw == 0 && s.PositionCollateralRaw > partialWithdrawalDustRaw {
		// One step withdraws and converts it to USDC: none of it waits in a
		// custody other lanes share.
		return d(DeleverRouteStep, exitWithdrawReason, stepDownMarker), true
	}
	if t.ReleaseRaw > 0 {
		// A release never empties a lane: 90% of its equity or more is the
		// operator's exit.
		equity := laneEquityRaw(s)
		if equity <= 0 || t.ReleaseRaw*10_000 >= equity*partialWithdrawalFullExitBPS || partialWithdrawalReleaseReceipts(s, t.ReleaseRaw) <= 0 {
			return d(Hold, holdNoRoom, 0), true
		}
		return d(DeleverRouteStep, releaseReason, t.ReleaseRaw), true
	}
	if t.Rule == ruleWithdrawal && s.WithdrawalDemandRaw > s.VoltrIdleRaw {
		// Nothing the target may touch is left to free.
		if s.StrategyNAVRaw == 0 {
			return d(HoldManualRecovery, "withdrawal_conservation_shortfall", s.WithdrawalDemandRaw-s.VoltrIdleRaw), true
		}
		return d(Hold, holdNoRoom, 0), true
	}
	return Decision{}, false
}

// inboundStep moves cash and custody into the working lane, then levers it
// toward its level.
func inboundStep(s Snapshot, t LaneTarget, hard int64, d func(Action, string, int64) Decision) Decision {
	// A flat lane's Squads cash is an entry's allocation. It returns to
	// Voltr when the lane cannot take all of it; without a current entry it
	// waits for the selector (a withdrawal stages it).
	if !s.HasPosition && !laneInFlight(s) && s.SquadsIdleRaw > 0 {
		if min(s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw) < s.SquadsIdleRaw || s.LiquidationThresholdBPS <= 0 || hard <= laneCeilingLTVBPS(s.RouteLane) {
			return d(StageSquadsToVoltr, returnCashReason, s.SquadsIdleRaw)
		}
		if t.Rule != ruleEntry {
			return d(Hold, holdStale, 0)
		}
	}
	if hard <= laneCeilingLTVBPS(s.RouteLane) && (s.HasPosition || s.CollateralIdleRaw > 0 || s.SquadsIdleRaw > 0) {
		return d(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	// Squads cash the target gives this lane (rule 5) converts to its
	// collateral; beside a USDC debt, through the debt-to-collateral leg.
	if s.SquadsIdleRaw > 0 && (t.Rule == ruleEntry || t.Rule == ruleCustody) {
		if sharedUSDCDebt(s.RouteLane) && s.PositionDebtRaw > 0 {
			return d(SwapDebtToCollateralStep, stepResidueReason, s.SquadsIdleRaw)
		}
		if !s.HasPosition {
			return d(SwapStableToCollateralStep, entrySwapReason, s.SquadsIdleRaw)
		}
		return d(SwapStableToCollateralStep, topupSwapReason, s.SquadsIdleRaw)
	}
	// Collateral a deposit takes deposits beside the position.
	if laneInFlight(s) {
		if s.MinimumCollateralDepositRaw <= 0 {
			return d(Hold, holdStale, 0)
		}
		if s.HasPosition {
			return d(OpenRouteStep, topupDepositReason, s.CollateralIdleRaw)
		}
		return d(OpenRouteStep, entryDepositReason, s.CollateralIdleRaw)
	}
	if t.KeepAlive {
		return d(OpenRouteStep, termKeepAliveReason, int64(termKeepAliveBorrowRaw))
	}
	if receive := leverageUpReceive(s, t); receive >= leverageMinimumBorrowRaw && !s.BorrowUtilizationBlocked && s.HasPosition && s.PositionCollateralRaw > 0 && earnActiveLane(s.RouteLane) {
		return d(OpenRouteStep, leverageUpReason, int64(receive))
	}
	if t.Rule == ruleEntry && s.SquadsIdleRaw == 0 && !laneInFlight(s) {
		if t.EntryRaw <= 0 {
			return d(Hold, holdStale, 0)
		}
		// Beside a position the deposit's room is the reserve's.
		room, reason := min(s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw), entryAllocationReason
		if s.HasPosition {
			room, reason = s.TopupDepositRoomRaw, topupAllocationReason
		}
		if room <= 0 || t.EntryRaw > min(s.VoltrIdleRaw, room) {
			return d(Hold, holdNoRoom, 0)
		}
		return d(VoltrAllocateToSquads, reason, t.EntryRaw)
	}
	// Squads cash no current selector destination takes waits for one.
	if t.Rule == ruleHold && s.HasPosition && s.SquadsIdleRaw > 0 {
		return d(Hold, holdStale, 0)
	}
	// The target wants a move the chain has no room for.
	if t.UpApproved && leverageUpLevel(withLevel(s, t.Level)) > 0 {
		return d(Hold, holdNoRoom, 0)
	}
	return d(Hold, holdAtTarget, 0)
}

func withLevel(s Snapshot, level float64) Snapshot {
	s.LeverageTargetLevel = level
	return s
}

// custodyDiscipline fails closed when the strategy custody balance is not
// exactly the amount this worker last staged into Voltr for the route. Voltr
// sweeps the entire custody on restore and books that balance into its own
// total value, so an unexplained balance is manual recovery and never an
// amount the planner may infer from a custody read.
func custodyDiscipline(s Snapshot) (Decision, bool) {
	if s.Nonterminal != "" || s.VoltrStrategyIdleRaw <= 0 {
		return Decision{}, false
	}
	if !s.StagedAmountKnown || s.StagedAmountRaw != s.VoltrStrategyIdleRaw {
		return Decision{Action: HoldManualRecovery, Reason: "custody_mismatch", AmountRaw: 0,
			IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, "custody_mismatch", s.VoltrStrategyIdleRaw),
			StrategyKey:    s.RouteLane}, true
	}
	return Decision{}, false
}

// obligationAbsentHoldReason marks an observed prerequisite: the lane's
// Kamino obligation account does not exist, so Kamino refuses the deposit
// the next leg would fund. It clears by itself once the account exists.
const obligationAbsentHoldReason = "obligation_absent"

// obligationPrerequisiteHold initializes a lane's absent obligation before a
// flat vault's entry deposits into it; any other deposit into an absent
// obligation holds (Kamino would refuse it).
func obligationPrerequisiteHold(s Snapshot, initializationReady func(Snapshot) bool) (Decision, bool) {
	if !s.ObligationPresenceKnown || s.ObligationPresent || (s.Target.Rule != ruleEntry && s.Target.Rule != ruleCustody && !laneInFlight(s)) {
		return Decision{}, false
	}
	if initializationReady(s) {
		return Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: s.RouteLane,
			IdempotencyKey: fmt.Sprintf("%s:initialize:%s", s.ObservationID, s.RouteLane)}, true
	}
	return Decision{Action: Hold, Reason: holdStale, StrategyKey: s.RouteLane,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, obligationAbsentHoldReason, 0)}, true
}

// overflowReleaseValueRaw is the most collateral value an overflow source
// moves without repaying: down to its cap's LTV (debt / (collateral - R)),
// and below the 90% of its equity that keeps it a lane.
func overflowReleaseValueRaw(s Snapshot) int64 {
	if s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 {
		return 0
	}
	limit := s.PositionCollateralValueRaw
	if s.PositionDebtValueRaw > 0 {
		ceiling := laneCeilingLTVBPS(s.RouteLane)
		if ceiling <= 0 {
			return 0
		}
		keep := new(big.Int).Mul(big.NewInt(s.PositionDebtValueRaw), big.NewInt(10_000))
		keep.Add(keep, big.NewInt(ceiling-1)).Quo(keep, big.NewInt(ceiling))
		if !keep.IsInt64() || keep.Int64() >= s.PositionCollateralValueRaw {
			return 0
		}
		limit = s.PositionCollateralValueRaw - keep.Int64()
	}
	equity := s.PositionCollateralValueRaw + s.CollateralIdleValueRaw - s.PositionDebtValueRaw
	return max(min(limit, (equity*partialWithdrawalFullExitBPS-1)/10_000), 0)
}
