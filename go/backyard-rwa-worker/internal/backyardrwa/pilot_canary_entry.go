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

// readPilotCanaryEntryRequest is the preserved embedded wrapper: the request
// lane authority stays exactly the installed selectorEntryLane set.
func readPilotCanaryEntryRequest(now time.Time) (*pilotCanaryEntryRequest, error) {
	return readPilotCanaryEntryRequestWithLane(now, selectorEntryLane)
}

// readPilotCanaryEntryRequestOnManifest scopes the request lane authority to
// the explicit reviewed manifest: the installed selectorEntryLane members plus
// the candidate AUTO lane only while that manifest's plain binding resolves
// (doc 31: new-entry candidate authority is selectorEntryFundingLane(lane,
// false)). The read shape is byte-identical — same environment variable, same
// unknown-field and trailing-value rejection, same 512-byte bound, same
// invalid-request hold.
func readPilotCanaryEntryRequestOnManifest(now time.Time, manifest RouteManifest) (*pilotCanaryEntryRequest, error) {
	return readPilotCanaryEntryRequestWithLane(now, func(lane string) bool {
		return manifest.selectorEntryFundingLane(lane, false)
	})
}

func readPilotCanaryEntryRequestWithLane(now time.Time, laneAllowed func(string) bool) (*pilotCanaryEntryRequest, error) {
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
	if len(raw) > 512 || decoder.Decode(&trailing) != io.EOF || request.validateWithLane(now, laneAllowed) != nil {
		return nil, budgetHold("invalid_pilot_canary_request")
	}
	return &request, nil
}

// validateWithLane is the shared request check with the lane authority
// parameterized. Canary acceptance is entry authority too: a deferred lane
// must not become an operator-requested destination. Failing here ends this
// selector sample and retains the prior durable selector authority; it does
// not enable ordinary switching on this tick. ID shape, equity bounds and the
// expiry window are the exact installed checks for every caller.
func (r pilotCanaryEntryRequest) validateWithLane(now time.Time, laneAllowed func(string) bool) error {
	if len(r.ID) != 64 || !sha256Pattern.MatchString(r.ID) || !laneAllowed(r.Lane) || r.EquityRaw <= 0 || r.EquityRaw > PilotWorkingTrancheCapRaw || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(15*time.Minute)) {
		return budgetHold("invalid_pilot_canary_request")
	}
	return nil
}

func (r pilotCanaryEntryRequest) validate(now time.Time) error {
	return r.validateWithLane(now, selectorEntryLane)
}

// validateOnManifest resolves the request lane authority through the explicit
// reviewed manifest with the doc 31 new-entry candidate scope
// (selectorEntryFundingLane(lane, false)). An absent binding keeps the
// candidate lane refused; a malformed binding fails the candidate request,
// never the installed lanes.
func (r pilotCanaryEntryRequest) validateOnManifest(now time.Time, manifest RouteManifest) error {
	return r.validateWithLane(now, func(lane string) bool {
		return manifest.selectorEntryFundingLane(lane, false)
	})
}

// pilotCanaryReceiptCapacity bounds the number of retained canary receipts.
// Retained IDs are never pruned or reset, and consumed-ID checks stay before
// this capacity gate.
const pilotCanaryReceiptCapacity = 16

// selectPilotCanaryEntry is the preserved embedded wrapper: both lane
// authorities stay the installed embedded sets — selectorEntryLane for the
// operator request, selectorLane for the built entry.
func selectPilotCanaryEntry(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	return selectPilotCanaryEntryWithManifest(input, result, history, nil)
}

// selectPilotCanaryEntryOnManifest is the manifest-scoped forced-acceptance
// call for evaluateSelector: the same request identity, retained-history,
// capacity and expiry semantics, with the request and entry lane authorities
// resolved through the explicit reviewed manifest. An absent binding keeps
// the candidate lane refused exactly as the embedded wrapper does; a malformed
// binding fails the candidate request, never the installed lanes.
func selectPilotCanaryEntryOnManifest(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt, manifest RouteManifest) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	return selectPilotCanaryEntryWithManifest(input, result, history, &manifest)
}

func selectPilotCanaryEntryWithManifest(input SelectorInput, result SelectorResult, history map[string]pilotCanaryEntryReceipt, manifest *RouteManifest) (SelectorResult, *pilotCanaryEntryReceipt, error) {
	request := input.canaryRequest
	if request == nil {
		return result, nil, nil
	}
	requestValid := func() error { return request.validate(input.Now) }
	entryValid := func(e SelectorEntry) error { return e.validate() }
	if manifest != nil {
		requestValid = func() error { return request.validateOnManifest(input.Now, *manifest) }
		entryValid = manifest.validateSelectorEntry
	}
	if err := requestValid(); err != nil {
		return result, nil, err
	}
	// Always suppress economic switching while an acceptance request is installed.
	// Worker recovery, NAV, risk and withdrawal actions run independently first.
	result.Action, result.Reason, result.DestinationLane, result.SelectedQuote = "KEEP", "operator_canary_waiting", "", nil
	if prior, exists := history[request.ID]; exists {
		if prior.Request != *request {
			return result, nil, budgetHold("pilot_canary_request_id_reused")
		}
		if !canaryReacceptable(input, prior) {
			result.Reason = "operator_canary_already_consumed"
			return result, nil, nil
		}
	} else if len(history) >= pilotCanaryReceiptCapacity {
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
	if err := entryValid(entry); err != nil {
		return result, nil, err
	}
	result.Action, result.Reason, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "CANARY_ENTER", "operator_acceptance_not_economic_recommendation", request.Lane, request.EquityRaw, chosen
	return result, &pilotCanaryEntryReceipt{Request: *request, AcceptedAt: input.Now, QuoteEvidenceID: chosen.EvidenceID}, nil
}

// canaryReacceptable lets one unexpired request re-accept with a fresh quote
// when its own last entry lapsed before any allocation bound to it. A new lane
// needs an obligation initializer and then the allocation, and the entry
// lapses 30 s after its quote, so one attempt could never fund a new lane
// (live 2026-09-24: Maple and AUTO each consumed two receipts). Once an
// allocation binds, or the request's own <=15-minute expiry passes, the
// request stays consumed; every money step still needs a current quote. The
// caller already requires the route flat and the request valid.
func canaryReacceptable(input SelectorInput, prior pilotCanaryEntryReceipt) bool {
	e := input.canaryPriorEntry
	return e != nil && e.Lane == prior.Request.Lane && e.EquityRaw == prior.Request.EquityRaw &&
		e.Quote.EvidenceID == prior.QuoteEvidenceID && e.AllocationOperationID == "" &&
		(!input.Now.Before(e.ExpiresAt) || !e.Quote.currentAtSlot(input.Snapshot.Slot))
}
