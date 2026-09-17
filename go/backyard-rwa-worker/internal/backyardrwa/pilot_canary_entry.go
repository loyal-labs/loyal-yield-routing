package backyardrwa

import (
	"encoding/json"
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
func (r pilotCanaryEntryRequest) validate(now time.Time) error {
	if len(r.ID) != 64 || !sha256Pattern.MatchString(r.ID) || !selectorLane(r.Lane) || r.EquityRaw <= 0 || r.EquityRaw > PilotWorkingTrancheCapRaw || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(15*time.Minute)) {
		return budgetHold("invalid_pilot_canary_request")
	}
	return nil
}

func selectPilotCanaryEntry(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	request := input.canaryRequest
	if request == nil {
		return result, nil, nil
	}
	if err := request.validate(input.Now); err != nil {
		return result, nil, err
	}
	// Always suppress economic switching while an acceptance request is installed.
	// Worker recovery, NAV, risk and withdrawal actions run independently first.
	result.Action, result.Reason, result.DestinationLane, result.SelectedQuote = "KEEP", "operator_canary_waiting", "", nil
	if prior, exists := history[request.ID]; exists {
		if prior.Request != *request {
			return result, nil, budgetHold("pilot_canary_request_id_reused")
		}
		result.Reason = "operator_canary_already_consumed"
		return result, nil, nil
	}
	if len(history) >= 8 {
		return result, nil, budgetHold("pilot_canary_history_full")
	}
	s := input.Snapshot
	if !s.PilotActive || !s.Fresh || !unwindComplete(s) || s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.PostMutationNAVRequired || s.CapitalMutated || s.LastReportAgeSeconds >= 60 || s.VoltrIdleRaw < request.EquityRaw || Decide(s).Action == ReportNAV {
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
	entry := SelectorEntry{Lane: request.Lane, EquityRaw: request.EquityRaw, ObservationID: s.ObservationID, Quote: *chosen, AcceptedAt: input.Now, ExpiresAt: chosen.ObservedAt.Add(min(input.Policy.QuoteMaxAge, 30*time.Second))}
	if err := entry.validate(); err != nil {
		return result, nil, err
	}
	result.Action, result.Reason, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "CANARY_ENTER", "operator_acceptance_not_economic_recommendation", request.Lane, request.EquityRaw, chosen
	return result, &pilotCanaryEntryReceipt{Request: *request, AcceptedAt: input.Now, QuoteEvidenceID: chosen.EvidenceID}, nil
}
