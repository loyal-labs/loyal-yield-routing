package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// A tranche moves part of the capital, E, from one held lane to another
// instead of all of it. The source half frees E with the debt-preserving
// partial withdrawal (the position keeps its level) and parks it in Voltr
// idle, reserved for the destination; the working lane then changes to the
// destination, which enters (flat) or tops up (held) from that idle cash and
// levers toward its own stored target. Only the working lane ever holds
// transient Squads, collateral or debt cash, and every custody is clear
// before the working lane changes.
type trancheIntent struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	EquityRaw   int64  `json:"equityRaw"`
	// TargetIdleRaw is the Voltr idle the source half frees to: the idle
	// observed when the tranche was chosen, plus E.
	TargetIdleRaw int64     `json:"targetIdleRaw"`
	Generation    int64     `json:"generation"`
	ObservationID string    `json:"observationId"`
	EvidenceID    string    `json:"evidenceId"`
	CreatedAt     time.Time `json:"createdAt"`
	// SourceFreed marks the handoff: the destination half is running.
	SourceFreed bool `json:"sourceFreed,omitempty"`
}

// maxHeldLanes bounds the held set: one reserve-refresh capture values every
// held lane, and its packet fits three lanes with the installed lookup table
// (AUTO+OnRe+Maple 1158 bytes; a fourth lane exceeds 1232).
const maxHeldLanes = 3

// trancheDestinationMaxAge abandons a destination half that never funded
// (a flat lane whose entry quote never came back): its idle cash returns to
// the source lane's top-up.
const trancheDestinationMaxAge = 2 * time.Hour

func (t trancheIntent) validate() error {
	if !earnActiveLane(t.Source) || !earnActiveLane(t.Destination) || t.Source == t.Destination || siblingLanes(t.Source, t.Destination) ||
		t.EquityRaw <= 0 || uint64(t.EquityRaw) > strategyTwoBridgeLegCapRaw || t.TargetIdleRaw < t.EquityRaw || t.Generation <= 0 ||
		t.ObservationID == "" || !sha256Pattern.MatchString(t.EvidenceID) || t.CreatedAt.IsZero() {
		return fmt.Errorf("invalid_tranche_intent")
	}
	return nil
}

func decodeTrancheIntent(raw []byte) (*trancheIntent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var t trancheIntent
	if json.Unmarshal(raw, &t) != nil || t.validate() != nil {
		return nil, budgetHold("invalid_durable_tranche")
	}
	return &t, nil
}

// siblingLanes reports two lanes that share a collateral reserve or a
// collateral custody (the Prime lanes share BUTND9 and the PRIME account).
// Siblings are never held together, and moving between them keeps the
// full-exit path: their custody balances cannot be told apart.
func siblingLanes(a, b string) bool {
	ra, errA := runtimeRoute(a)
	rb, errB := runtimeRoute(b)
	if errA != nil || errB != nil || a == b {
		return false
	}
	return ra.Kamino.CollateralReserve == rb.Kamino.CollateralReserve || ra.CollateralCustody == rb.CollateralCustody
}

// applyTranche stamps the tranche into the per-lane view: the source half's
// idle target on the source lane, the reserved E on the destination lane.
func applyTranche(s *Snapshot, t *trancheIntent) {
	s.TrancheTargetIdleRaw, s.TrancheDestinationRaw = 0, 0
	if t == nil {
		return
	}
	switch {
	case !t.SourceFreed && s.RouteLane == t.Source:
		s.TrancheTargetIdleRaw = t.TargetIdleRaw
	case t.SourceFreed && s.RouteLane == t.Destination:
		s.TrancheDestinationRaw = t.EquityRaw
	}
}

// trancheSourceHalf: this view is a tranche source still freeing E. A user
// withdrawal or an unwind runs through its own branch instead.
func trancheSourceHalf(s Snapshot) bool {
	return s.TrancheTargetIdleRaw > 0 && s.WithdrawalDemandRaw == 0 && !s.Unwind && !s.CutoverDrain
}

// trancheSourceHoldReason labels a source half with no next partial leg.
func trancheSourceHoldReason(s Snapshot) string {
	if s.VoltrIdleRaw >= s.TrancheTargetIdleRaw {
		return "tranche_source_freed"
	}
	return "tranche_source_partial_unproven"
}

// laneTransientClear reports a working lane whose custodies are clear: no
// Squads cash, no strategy custody, and no collateral or debt cash beyond the
// partial planner's dust (none at all in a custody another lane shares).
func laneTransientClear(s Snapshot, shared bool) bool {
	dust := partialWithdrawalDustRaw
	if shared {
		dust = 0
	}
	return s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0 && s.CollateralIdleRaw <= dust && s.DebtIdleRaw <= dust
}

// laneSharesCustody reports whether lane a shares a collateral or debt
// custody with lane b (the Squads USDC ATA is bridge cash, not lane custody).
func laneSharesCustody(a, b string) bool {
	ra, errA := runtimeRoute(a)
	rb, errB := runtimeRoute(b)
	if errA != nil || errB != nil {
		return true
	}
	if ra.CollateralCustody == rb.CollateralCustody {
		return true
	}
	return ra.Kamino.DebtMint != bridgeUSDC && ra.DebtCustody == rb.DebtCustody
}

// trancheSettledView is a coherent, quiet per-lane view: nothing in flight,
// no recovery, no pending report.
func trancheSettledView(s Snapshot) bool {
	return s.Fresh && s.ManualReason == "" && s.Nonterminal == "" && !s.HasAmbiguousSubmission && !s.PostMutationNAVRequired && !s.CrossLaneHardLTV
}

type trancheStep string

const (
	trancheStay     trancheStep = ""
	trancheHandoff  trancheStep = "handoff"
	trancheComplete trancheStep = "complete"
	trancheAbandon  trancheStep = "abandon"
)

// trancheTransition is the durable step a quiet tick takes: hand the working
// lane to the destination once the source has freed E and cleared its
// custodies; complete once the destination is funded and its leverage is
// settled; abandon a source that cannot free anything, or a destination
// that never funded.
func trancheTransition(t trancheIntent, s Snapshot, now time.Time) trancheStep {
	if !trancheSettledView(s) {
		return trancheStay
	}
	if !t.SourceFreed {
		if s.RouteLane != t.Source || s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain {
			return trancheStay
		}
		if _, _, _, next := partialWithdrawalStep(s); next {
			return trancheStay
		}
		if !laneTransientClear(s, laneSharesCustody(t.Source, t.Destination)) {
			return trancheStay
		}
		if s.VoltrIdleRaw < topupMinimumRaw {
			return trancheAbandon
		}
		return trancheHandoff
	}
	if s.RouteLane != t.Destination {
		return trancheStay
	}
	if !s.HasPosition {
		if now.Sub(t.CreatedAt) > trancheDestinationMaxAge && !hasWorkingCapital(s) {
			return trancheAbandon
		}
		return trancheStay
	}
	if selectorTrancheInProgress(s) || !laneTransientClear(s, true) || s.LeverageTargetLevel == 0 {
		return trancheStay
	}
	// The destination is funded and its loop done; it stays working until
	// the top-up has nothing left to move and its own target is settled.
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	if action, _, _, ok := leverageBorrowStep(s, hard); ok && action != Hold {
		return trancheStay
	}
	if _, topup := topupStep(s, hard, func(a Action, r string, n int64) Decision { return Decision{Action: a, Reason: r, AmountRaw: n} }); topup {
		return trancheStay
	}
	return trancheComplete
}

// LoadTranche reads the durable tranche for construction refreshes.
func (d *Database) LoadTranche(ctx context.Context, routeKey string) (*trancheIntent, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'tranche' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	return decodeTrancheIntent(raw)
}

// AdvanceTranche applies one tranche transition under the route lock, the
// lease fence and the no-nonterminal fence. It moves no money: the working
// lane changes only after the source's custodies are clear.
func (d *Database) AdvanceTranche(ctx context.Context, routeKey string, s Snapshot, now time.Time) (bool, error) {
	lease, err := d.currentLease()
	if err != nil {
		return false, err
	}
	if lease.RouteKey != routeKey {
		return false, fmt.Errorf("tranche_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrRouteLeaseLost
		}
		return false, err
	}
	var state struct {
		Tranche json.RawMessage `json:"tranche"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return false, budgetHold("invalid_tranche_route_state")
	}
	t, err := decodeTrancheIntent(state.Tranche)
	if err != nil || t == nil {
		return false, err
	}
	step := trancheTransition(*t, s, now)
	if step == trancheStay {
		return false, nil
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`, routeKey).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	tranche, working := "null", s.RouteLane
	switch step {
	case trancheHandoff:
		next := *t
		next.SourceFreed = true
		encoded, err := json.Marshal(next)
		if err != nil {
			return false, err
		}
		tranche, working = string(encoded), t.Destination
	case trancheAbandon:
		working = t.Source
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(jsonb_set(state,'{tranche}',$5::jsonb,true),'{workingLane}',to_jsonb($6::text),true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp()
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`,
		routeKey, lease.Owner, lease.FencingToken, version, tranche, working)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("tranche_write_lost_route_fence")
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: tranche %s source=%s destination=%s equity=%d working=%s\n", step, t.Source, t.Destination, t.EquityRaw, working)
	return true, nil
}
