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
// restarting the same deployment never authorizes another attempt.
type pilotCanaryEntryRequest struct {
	ID        string    `json:"id"`
	Lane      string    `json:"lane"`
	EquityRaw int64     `json:"equityRaw"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type pilotCanaryEntryReceipt struct {
	Request         pilotCanaryEntryRequest `json:"request"`
	AcceptedAt      time.Time               `json:"acceptedAt"`
	QuoteEvidenceID string                  `json:"quoteEvidenceId"`
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
	if len(r.ID) != 64 || !sha256Pattern.MatchString(r.ID) || !earnActiveLane(r.Lane) || r.EquityRaw <= 0 || uint64(r.EquityRaw) > strategyTwoBridgeLegCapRaw || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(15*time.Minute)) {
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
	if prior, exists := history[request.ID]; exists && prior.Request != *request {
		return result, nil, budgetHold("pilot_canary_request_id_reused")
	} else if !exists && len(history) >= pilotCanaryReceiptCapacity {
		return result, nil, budgetHold("pilot_canary_history_full")
	}
	// The request enters a flat vault. It keeps its lane, with a fresh quote
	// each sample, until the cash it allocated reaches the lane: target()
	// reads the latest result for the vault's Squads cash. A new allocation
	// needs a quiet book.
	s := input.Snapshot
	if !s.Fresh || laneExposure(s) || s.WithdrawalDemandRaw != 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.VoltrStrategyIdleRaw != 0 {
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
		if candidate.Lane == request.Lane && candidate.CostsKnown && candidate.BlockedReason == "" {
			valid = true
		}
	}
	if !valid {
		return result, nil, nil
	}
	var chosen *MoveQuote
	for i := range input.Quotes {
		q := input.Quotes[i]
		if q.DestinationLane == request.Lane && q.SourceLane == s.RouteLane && q.EquityRaw == request.EquityRaw && q.ObservationID == s.ObservationID && q.currentAtSlot(s.Slot) && freshAt(input.Now, q.ObservedAt, input.Policy.QuoteMaxAge) {
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
