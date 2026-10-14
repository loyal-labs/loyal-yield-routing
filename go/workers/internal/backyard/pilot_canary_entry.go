package backyard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// An explicit, expiring acceptance request uses the real selector quote and
// ordinary execution admission. It is not an economic recommendation. The
// immutable request ID is consumed under the route lock before any allocation;
// restarting the same deployment never authorizes another attempt. With a
// sourceLane it rotates equityRaw of that held lane into lane.
type pilotCanaryEntryRequest struct {
	ID         string    `json:"id"`
	Lane       string    `json:"lane"`
	SourceLane string    `json:"sourceLane,omitempty"`
	EquityRaw  int64     `json:"equityRaw"`
	ExpiresAt  time.Time `json:"expiresAt"`
}
type pilotCanaryEntryReceipt struct {
	Request         pilotCanaryEntryRequest `json:"request"`
	AcceptedAt      time.Time               `json:"acceptedAt"`
	QuoteEvidenceID string                  `json:"quoteEvidenceId"`
	// SourceRelease is a rotation's release, sized from the source position
	// first accepted: target rule 5r releases from that position.
	SourceRelease *SourceRelease `json:"sourceRelease,omitempty"`
}

// readPilotCanaryEntryRequest reads the operator acceptance request: an active
// registry lane, a bounded equity and a short expiry.
func readPilotCanaryEntryRequest(now time.Time) (*pilotCanaryEntryRequest, error) {
	raw := os.Getenv("BACKYARD_RWA_PILOT_CANARY_ENTRY")
	if raw == "" {
		return nil, nil
	}
	var request pilotCanaryEntryRequest
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return nil, budgetHold("invalid_pilot_canary_request")
	}
	// Reject trailing values as well as overlong/unbounded configuration.
	var trailing any
	if len(raw) > 512 || decoder.Decode(&trailing) != io.EOF || request.validate(now) != nil {
		return nil, budgetHold("invalid_pilot_canary_request")
	}
	return &request, nil
}

// validate is the request check. Canary acceptance is entry authority too:
// an exit-only lane must not become an operator-requested destination.
// Failing here ends this selector sample and retains the prior durable
// selector authority; it does not enable ordinary switching on this tick.
func (r pilotCanaryEntryRequest) validate(now time.Time) error {
	if len(r.ID) != 64 || !sha256Pattern.MatchString(r.ID) || !earnActiveLane(r.Lane) || r.EquityRaw <= 0 || uint64(r.EquityRaw) > strategyTwoBridgeLegCapRaw || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(15*time.Minute)) ||
		r.SourceLane != "" && (!earnHeldLane(r.SourceLane) || !rotationPair(r.SourceLane, r.Lane)) {
		return budgetHold("invalid_pilot_canary_request")
	}
	return nil
}

// pilotCanaryReceiptCapacity bounds the number of retained canary receipts.
// Retained IDs are never pruned or reset, and consumed-ID checks stay before
// this capacity gate.
// Raised 16 -> 32 on 2026-09-25 after the Maple/AUTO rotation used all 16.
const pilotCanaryReceiptCapacity = 32

func selectPilotCanaryEntry(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	request := input.canaryRequest
	if request == nil {
		return result, nil, nil
	}
	if err := request.validate(input.Now); err != nil {
		return result, nil, err
	}
	// Always suppress economic entry while an acceptance request is installed.
	// Worker recovery, NAV, risk and withdrawal actions run independently first.
	result.Action, result.Reason, result.DestinationLane, result.SelectedQuote = "KEEP", "operator_canary_waiting", "", nil
	result.SourceLane, result.SourceRelease = input.Snapshot.RouteLane, nil
	if prior, exists := history[request.ID]; exists && prior.Request != *request {
		return result, nil, budgetHold("pilot_canary_request_id_reused")
	} else if !exists && len(history) >= pilotCanaryReceiptCapacity {
		return result, nil, budgetHold("pilot_canary_history_full")
	}
	s := input.Snapshot
	if !s.Fresh || s.WithdrawalDemandRaw != 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.VoltrStrategyIdleRaw != 0 {
		return result, nil, nil
	}
	if request.SourceLane != "" {
		return selectPilotCanaryRotation(input, result, history)
	}
	// The request enters a flat vault. It keeps its lane, with a fresh quote
	// each sample, until the cash it allocated reaches the lane: target()
	// reads the latest result for the vault's Squads cash. A new allocation
	// needs a quiet book. Capital anywhere in a lane, its entry's collateral
	// in flight included, spends the request.
	if s.HasPosition || s.PositionCollateralRaw > 0 || s.PositionDebtRaw > 0 || s.CollateralIdleRaw > 0 || s.PrimeIdleRaw > 0 || s.HeldLanes != "" {
		return result, nil, nil
	}
	// Squads cash is this request's allocation in flight; any other request
	// needs it empty.
	if _, accepted := history[request.ID]; s.SquadsIdleRaw > 0 && !accepted {
		return result, nil, nil
	}
	if s.SquadsIdleRaw == 0 && (s.PostMutationNAVRequired || s.CapitalMutated || s.LastReportAgeSeconds >= 60 || s.VoltrIdleRaw < request.EquityRaw || Decide(s).Action == ReportNAV) {
		return result, nil, nil
	}
	// Candidate validity includes current market data, capacity, bounded full
	// move costs and borrow terms. Only economic benefit/persistence is bypassed,
	// explicitly and exclusively for this one operator-funded acceptance attempt.
	valid := false
	for _, candidate := range result.Candidates {
		if candidate.SourceLane == "" && candidate.Lane == request.Lane && candidate.CostsKnown && candidate.BlockedReason == "" {
			valid = true
		}
	}
	if !valid {
		return result, nil, nil
	}
	var chosen *MoveQuote
	for i := range input.Quotes {
		q := input.Quotes[i]
		if q.DestinationLane == request.Lane && q.SourceLane == s.RouteLane && q.RotateFrom == "" && q.EquityRaw == request.EquityRaw && q.ObservationID == s.ObservationID && q.currentAtSlot(s.Slot) && freshAt(input.Now, q.ObservedAt, input.Policy.QuoteMaxAge) {
			if chosen != nil {
				return result, nil, budgetHold("duplicate_move_quote")
			}
			chosen = &q
		}
	}
	if chosen == nil {
		return result, nil, nil
	}
	if chosen.CostRaw < 0 || chosen.CostRaw >= chosen.EquityRaw || chosen.MinimumIdleRaw < uint64(chosen.EquityRaw) || !chosen.validBorrow() || !chosen.storedWindowValid() || !sha256Pattern.MatchString(chosen.EvidenceID) {
		return result, nil, fmt.Errorf("invalid_selector_entry")
	}
	result.Action, result.Reason, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "CANARY_ENTER", "operator_acceptance_not_economic_recommendation", request.Lane, request.EquityRaw, chosen
	return result, &pilotCanaryEntryReceipt{Request: *request, AcceptedAt: input.Now, QuoteEvidenceID: chosen.EvidenceID}, nil
}

// selectPilotCanaryRotation rotates the request's equity from its held
// source lane into its lane: CANARY_ROTATE, re-issued with a fresh quote each
// sample until the lane holds a position. target() releases the source
// from the position first accepted (SourceRelease), and enters the released
// cash.
func selectPilotCanaryRotation(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	request, s := input.canaryRequest, input.Snapshot
	held := s.heldLanes()
	if !containsLane(held, request.SourceLane) || containsLane(held, request.Lane) || Decide(s).Action == ReportNAV {
		return result, nil, nil
	}
	valid := false
	for _, candidate := range result.Candidates {
		if candidate.SourceLane == request.SourceLane && candidate.Lane == request.Lane && candidate.CostsKnown && candidate.BlockedReason == "" {
			valid = true
		}
	}
	var source *Snapshot
	for i := range input.Lanes {
		if input.Lanes[i].RouteLane == request.SourceLane {
			source = &input.Lanes[i]
		}
	}
	if !valid || source == nil {
		return result, nil, nil
	}
	var chosen *MoveQuote
	for i := range input.Quotes {
		q := input.Quotes[i]
		if q.RotateFrom == request.SourceLane && q.DestinationLane == request.Lane && q.EquityRaw == request.EquityRaw && q.ObservationID == s.ObservationID && q.currentAtSlot(s.Slot) && freshAt(input.Now, q.ObservedAt, input.Policy.QuoteMaxAge) {
			if chosen != nil {
				return result, nil, budgetHold("duplicate_move_quote")
			}
			chosen = &q
		}
	}
	if chosen == nil {
		return result, nil, nil
	}
	if chosen.CostRaw < 0 || chosen.CostRaw >= chosen.EquityRaw || chosen.MinimumIdleRaw < uint64(chosen.EquityRaw) || !chosen.validBorrow() || !chosen.storedWindowValid() || !sha256Pattern.MatchString(chosen.EvidenceID) {
		return result, nil, fmt.Errorf("invalid_selector_entry")
	}
	receipt := pilotCanaryEntryReceipt{Request: *request, AcceptedAt: input.Now, SourceRelease: newSourceRelease(*source, request.EquityRaw)}
	if prior, accepted := history[request.ID]; accepted {
		receipt = prior
	}
	receipt.QuoteEvidenceID = chosen.EvidenceID
	result.Action, result.Reason, result.SourceLane, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "CANARY_ROTATE", "operator_acceptance_not_economic_recommendation", request.SourceLane, request.Lane, request.EquityRaw, chosen
	result.SourceRelease = receipt.SourceRelease
	return result, &receipt, nil
}
