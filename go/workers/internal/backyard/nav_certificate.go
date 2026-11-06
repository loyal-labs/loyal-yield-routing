package backyard

import (
	"encoding/json"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"time"
)

type exitQuoteScope struct {
	vault, manifest, path string
	usdc                  kamino.ScaledFraction
}
type certifiedNAV struct {
	vault, manifest string
	snapshotBook    string
	slot            int64
	report          BridgeReport
	exitLiability   uint64
	quotes          []ExitCost
	validUntil      time.Time
}

func (c ExitCost) validAt(now time.Time, slot int64) bool {
	return c.scope != nil && c.Source == exitCostQuote && !now.Before(c.At) && now.Sub(c.At) <= exitQuoteInterval && c.Slot > 0 && c.Slot <= slot && slot-c.Slot <= min(observationLagSlots(), adaptorMaxReportAgeSlots)
}
func (e exitCosts) applicable(m RouteManifest, slot int64, now time.Time) exitCosts {
	out := exitCosts{}
	for lane, costs := range e {
		valid := func(q *ExitCost) *ExitCost {
			if q != nil && q.validAt(now, slot) && q.scopedTo(m, lane) {
				return q.cloned()
			}
			return nil
		}
		if c := (laneExitCosts{Quote: valid(costs.Quote), Pending: valid(costs.Pending)}); c.Quote != nil || c.Pending != nil {
			out[lane] = c
		}
	}
	return out
}
func certifyNAV(m RouteManifest, slot int64, nav NAVReportInput, exits []ExitCost, liability uint64) *certifiedNAV {
	now := time.Now()
	c := &certifiedNAV{vault: m.vault.squads, manifest: m.SHA256, slot: slot, exitLiability: liability, report: BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: uint64(nav.Raw), SnapshotDigest: nav.SnapshotDigest}, validUntil: now.Add(exitQuoteInterval)}
	for _, q := range exits {
		if !q.validAt(now, slot) {
			return nil
		}
		scope := *q.scope
		q.scope = &scope
		c.quotes = append(c.quotes, q)
		if until := q.At.Add(exitQuoteInterval); until.Before(c.validUntil) {
			c.validUntil = until
		}
	}
	return c
}
func (c *certifiedNAV) valid(now time.Time, slot int64) bool {
	if c == nil || c.vault == "" || !sha256Pattern.MatchString(c.manifest) || c.slot <= 0 || slot < c.slot || slot-c.slot > min(observationLagSlots(), adaptorMaxReportAgeSlots) || now.After(c.validUntil) {
		return false
	}
	for _, q := range c.quotes {
		if !q.validAt(now, slot) {
			return false
		}
	}
	return true
}
func (s Snapshot) netKnown(now time.Time) bool {
	return s.net.valid(now, s.Slot) && s.net.snapshotBook != "" && s.net.snapshotBook == snapshotNAVBook(s)
}
func (s Snapshot) netLogValue(now time.Time) any {
	if !s.netKnown(now) {
		return nil
	}
	return s.StrategyNAVRaw
}
func (r BridgeBuildRequest) validateNAV(v *Vault, now time.Time) error {
	if r.Action == bridgeStageSquadsToVoltr && !v.trustful() {
		return nil
	}
	if !r.net.valid(now, int64(r.Report.ObservedSlot)) || r.net.vault != v.squads || r.Report != r.net.report || r.requestDigest == "" || r.requestDigest != addRequestDigest(r) {
		return staleHold("certified_nav_unavailable")
	}
	return nil
}

func requiresNAVForAdd(d Decision) bool {
	switch d.Action {
	case VoltrAllocateToSquads, SwapStableToCollateralStep, SwapUSDCToDebtStep:
		return true
	case OpenRouteStep:
		return true
	case SwapDebtToCollateralStep:
		return d.Reason != outboundDebtReason && d.Reason != stepResidueReason
	}
	return false
}

func quotePath(m RouteManifest, lane string) string {
	r, err := m.route(lane)
	if err != nil {
		return ""
	}
	return r.Kamino.CollateralMint + ":" + fleet.USDCMint
}

type addAdmission struct {
	net           *certifiedNAV
	requestDigest string
}

func addRequestDigest(request any) string {
	b, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	return sha256Bytes(b)
}

func authorizeAddRequest(request any, d Decision, s Snapshot) (any, error) {
	required := requiresNAVForAdd(d)
	if required && !s.netKnown(time.Now()) {
		return nil, staleHold(exitUnpriced)
	}
	a := &addAdmission{net: s.net, requestDigest: addRequestDigest(request)}
	switch r := request.(type) {
	case KaminoLegRequest:
		if required {
			r.admission = a
		}
		r.riskOnly = !required && (d.Reason == termKeepAliveReason || d.Action == SwapDebtToCollateralStep || d.Action == SwapUSDCToDebtStep)
		return r, nil
	case JupiterSwapRequest:
		if required {
			r.admission = a
		}
		r.riskOnly = !required && (d.Reason == termKeepAliveReason || d.Action == SwapDebtToCollateralStep || d.Action == SwapUSDCToDebtStep)
		return r, nil
	case HastraConversionRequest:
		if required {
			r.admission = a
		}
		r.riskOnly = !required && (d.Reason == termKeepAliveReason || d.Action == SwapDebtToCollateralStep || d.Action == SwapUSDCToDebtStep)
		return r, nil
	case StepRequest:
		if required {
			r.admission = a
		}
		r.riskOnly = !required && (d.Reason == termKeepAliveReason || d.Action == SwapDebtToCollateralStep || d.Action == SwapUSDCToDebtStep)
		return r, nil
	}
	return request, nil
}

func requestAddAdmission(request any) (*addAdmission, bool) {
	switch r := request.(type) {
	case KaminoLegRequest:
		return r.admission, r.admission != nil || r.Action == OpenRouteStep && !r.riskOnly
	case JupiterSwapRequest:
		return r.admission, r.admission != nil || !r.riskOnly && (r.Action == SwapStableToCollateralStep || r.Action == SwapUSDCToDebtStep || r.Action == SwapDebtToCollateralStep)
	case HastraConversionRequest:
		return r.admission, r.admission != nil || !r.riskOnly && (r.Action == SwapDebtToCollateralStep || r.Action == SwapUSDCToDebtStep)
	case StepRequest:
		return r.admission, r.admission != nil || r.Open.Action == OpenRouteStep && !r.riskOnly
	}
	return nil, false
}

func validateAddAdmission(m RouteManifest, request any, a *addAdmission, now time.Time, slot int64) error {
	if a == nil || !a.net.valid(now, slot) || a.net.vault != m.vault.squads || a.net.manifest != m.SHA256 || a.requestDigest == "" || a.requestDigest != addRequestDigest(request) {
		return staleHold("certified_add_nav_unavailable")
	}
	return nil
}

func snapshotNAVBook(s Snapshot) string {
	return sha256Bytes([]byte(fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d", s.RouteLane, s.HeldLanes, s.Slot, s.PositionCollateralRaw, s.PositionDebtRaw, s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.CollateralIdleRaw, s.CollateralIdleValueRaw, s.DebtIdleRaw, s.SquadsIdleRaw, s.VoltrIdleRaw, s.VoltrStrategyIdleRaw, s.StrategyNAVRaw, s.TotalVaultNAVRaw, s.PriorReportedNAVRaw, s.LTVBPS, s.ExitLiabilityRaw, s.ExitCostBPS)))
}
func bindSnapshotNAV(s *Snapshot) {
	if s.net != nil {
		c := *s.net
		c.snapshotBook = snapshotNAVBook(*s)
		s.net = &c
	}
}

func bindBridgeRequest(r BridgeBuildRequest) BridgeBuildRequest {
	r.requestDigest = addRequestDigest(r)
	return r
}

func (c ExitCost) scopedTo(m RouteManifest, lane string) bool {
	return c.scope != nil && c.Lane == lane && c.scope.vault == m.vault.squads && c.scope.manifest == m.SHA256 && c.scope.path == quotePath(m, lane)
}
