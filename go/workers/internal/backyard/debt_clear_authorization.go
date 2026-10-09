package backyard

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// DebtClearConfirmation is an explicit privileged-operator attestation, not
// authentication of ConfirmedBy. Only the bounded operator command accepts it.
// SSH/database access remains the operator trust boundary.
type DebtClearConfirmation struct {
	RequestID                      string    `json:"requestId"`
	ConfirmedBy                    string    `json:"confirmedBy"`
	ConfirmationRecord             string    `json:"confirmationRecord"`
	AcknowledgeUnavailableReborrow bool      `json:"acknowledgeUnavailableReborrow"`
	ExpiresAt                      time.Time `json:"expiresAt"`
}

func (c DebtClearConfirmation) validate(now time.Time) error {
	if !sha256Pattern.MatchString(c.RequestID) || !sha256Pattern.MatchString(c.ConfirmationRecord) || strings.TrimSpace(c.ConfirmedBy) == "" || len(c.ConfirmedBy) > 256 || !c.AcknowledgeUnavailableReborrow || !c.ExpiresAt.After(now) || c.ExpiresAt.After(now.Add(15*time.Minute)) {
		return budgetHold("debt_clear_confirmation_required")
	}
	return nil
}

// Receipts are never pruned. A full ordinary history needs explicit maintenance;
// emergency receipts live in their immutable origin operation, not this limit.
const debtClearReceiptCapacity = 128

type debtClearAuthority struct {
	ID               string                 `json:"id"`
	RouteKey         string                 `json:"routeKey"`
	ManifestSHA256   string                 `json:"manifestSha256"`
	Vault            string                 `json:"vault"`
	Obligation       string                 `json:"obligation"`
	DebtMint         string                 `json:"debtMint"`
	DebtReserve      string                 `json:"debtReserve"`
	Origin           UnwindIntent           `json:"origin"`
	Confirmation     *DebtClearConfirmation `json:"confirmation,omitempty"`
	Emergency        *debtClearRiskProof    `json:"emergency,omitempty"`
	FirstOperationID string                 `json:"firstOperationId,omitempty"`
	UsedCostMicros   int64                  `json:"usedCostMicros"`
}

type debtClearRiskProof struct {
	OperationID     string    `json:"operationId"`
	ObservationID   string    `json:"observationId"`
	AccountsSHA256  string    `json:"accountsSha256"`
	ValuationSource string    `json:"valuationSource"`
	Slot            int64     `json:"slot"`
	ObservedAt      time.Time `json:"observedAt"`
	LTVBPS          int64     `json:"ltvBps"`
	HardLTVBPS      int64     `json:"hardLtvBps"`
}

type debtClearRouteState struct {
	Authority *debtClearAuthority           `json:"debtClearAuthority"`
	Receipts  map[string]debtClearAuthority `json:"debtClearReceipts"`
	Unwind    *UnwindIntent                 `json:"selectorUnwind"`
	Target    *LeverageTarget               `json:"leverageTarget"`
}

func newDebtClearAuthority(m RouteManifest, routeKey, id string, intent UnwindIntent) (debtClearAuthority, error) {
	route, err := runtimeRoute(intent.SourceLane)
	if err != nil || m.validateUnwindIntent(intent) != nil || !sha256Pattern.MatchString(m.SHA256) || intent.MaxDebtRaw <= 0 {
		return debtClearAuthority{}, budgetHold("debt_clear_scope_unavailable")
	}
	return debtClearAuthority{ID: id, RouteKey: routeKey, ManifestSHA256: m.SHA256, Vault: bridgeVault, Obligation: route.Kamino.Obligation, DebtMint: route.Kamino.DebtMint, DebtReserve: route.Kamino.DebtReserve, Origin: intent}, nil
}

func (a debtClearAuthority) validate(m RouteManifest, routeKey string, now time.Time) error {
	expected, err := newDebtClearAuthority(m, routeKey, a.ID, a.Origin)
	if err != nil || !sha256Pattern.MatchString(a.ID) || a.RouteKey != expected.RouteKey || a.ManifestSHA256 != expected.ManifestSHA256 || a.Vault != expected.Vault || a.Obligation != expected.Obligation || a.DebtMint != expected.DebtMint || a.DebtReserve != expected.DebtReserve || a.UsedCostMicros < 0 {
		return budgetHold("debt_clear_scope_changed")
	}
	if a.UsedCostMicros > a.Origin.CostBoundRaw {
		return budgetHold("debt_clear_cost_bound_exceeded")
	}
	if a.Emergency != nil {
		p := a.Emergency
		if a.Confirmation != nil || p.OperationID == "" || p.ObservationID != a.Origin.ObservationID || p.AccountsSHA256 != a.Origin.EvidenceID || !sha256Pattern.MatchString(p.AccountsSHA256) || p.Slot <= 0 || p.ObservedAt.IsZero() || p.HardLTVBPS <= TargetLTVBPS || p.HardLTVBPS > 6000 || p.LTVBPS < p.HardLTVBPS || a.Origin.Reason != "hard_ltv_reduction" {
			return budgetHold("debt_clear_emergency_proof_invalid")
		}
		return nil
	}
	if a.Confirmation == nil || a.ID != a.Confirmation.RequestID || a.Confirmation.validate(now) != nil {
		return budgetHold("debt_clear_confirmation_required")
	}
	return nil
}

// The operator commit stores the full attested envelope, not an owner label on
// an automatically generated intent. Call only under the route lock and lease.
func (d *Database) commitDebtClearConfirmationTx(ctx context.Context, tx pgx.Tx, m RouteManifest, routeKey string, raw []byte, intent *UnwindIntent, c DebtClearConfirmation) error {
	c.ExpiresAt = c.ExpiresAt.UTC()
	if err := c.validate(time.Now().UTC()); err != nil {
		return err
	}
	var state debtClearRouteState
	if json.Unmarshal(raw, &state) != nil {
		return budgetHold("debt_clear_state_invalid")
	}
	if prior, exists := state.Receipts[c.RequestID]; exists {
		// Idempotent retries retain the original creation clock. A completed or
		// superseded receipt can never be made active again.
		retry := *intent
		retry.CreatedAt = prior.Origin.CreatedAt
		if state.Authority == nil || state.Authority.ID != c.RequestID || prior.Confirmation == nil || *prior.Confirmation != c || !sameUnwindIntent(prior.Origin, retry) {
			return budgetHold("debt_clear_confirmation_reused")
		}
		*intent = prior.Origin
		return nil
	}
	if len(state.Receipts) >= debtClearReceiptCapacity {
		return budgetHold("debt_clear_receipt_history_full")
	}
	authority, err := newDebtClearAuthority(m, routeKey, c.RequestID, *intent)
	if err != nil {
		return err
	}
	authority.Confirmation = &c
	if state.Receipts == nil {
		state.Receipts = make(map[string]debtClearAuthority)
	}
	state.Receipts[c.RequestID] = authority
	receipts, err := json.Marshal(state.Receipts)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(state,'{debtClearReceipts}',$2::jsonb,true),'{debtClearAuthority}',$3::jsonb,true) WHERE route_key=$1`, routeKey, string(receipts), string(encoded))
	return err
}

// Cost-only exit templates are deliberately ignored. This classifies the
// executable leg AND its intended continuation. Unknown debt-bearing exits
// fail closed; positive residual cash alone does not prove a partial flow.
func debtClearRequired(m RouteManifest, request any, effects ExpectedEffects, plan *phase3BridgeAdmission, state debtClearRouteState) (bool, error) {
	switch r := request.(type) {
	case KaminoInitializationRequest:
		return false, nil
	case BridgeBuildRequest:
		if r.Action == ReportNAV || r.Action == VoltrRestoreIdle {
			return false, nil
		}
	case KaminoPrimeUSDCRequest:
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return false, err
		}
		if leg == kaminoLegDeposit || leg == kaminoLegBorrow {
			if state.Authority != nil {
				return false, budgetHold("debt_clear_flow_forbids_new_position")
			}
			return false, nil
		}
	}
	if plan == nil {
		return false, budgetHold("debt_clear_classification_unavailable")
	}
	s, decision := plan.Snapshot, plan.Decision
	if s.PositionDebtRaw < 0 || !s.Fresh || s.RouteLane != decision.StrategyKey || s.Slot <= 0 {
		return false, budgetHold("debt_clear_classification_unavailable")
	}
	if s.PositionDebtRaw == 0 {
		// Paying the debt is not flow completion. The collateral release, swaps
		// and staging still spend the same scoped approval until reconciled flat.
		return state.Authority != nil || state.Unwind != nil && state.Unwind.MaxDebtRaw > 0, nil
	}
	// Entry swaps cannot inherit exit authority while a flow is outstanding.
	if decision.Action == SwapDebtToCollateralStep || decision.Action == SwapStableToCollateralStep || decision.Action == SwapUSDCToPrimeStep || decision.Action == VoltrAllocateToSquads && !s.CutoverDrain {
		if state.Authority != nil {
			return false, budgetHold("debt_clear_flow_forbids_new_position")
		}
		return false, nil
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok {
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return false, err
		}
		if leg == kaminoLegRepay {
			if effects.Repayment == nil || effects.Repayment.MaximumDebitRaw != r.AmountRaw {
				return false, budgetHold("debt_clear_classification_unavailable")
			}
			if r.AmountRaw >= uint64(s.PositionDebtRaw) || r.FullPayoff {
				return true, nil
			}
			if plan.RepaymentProjection == nil {
				return false, budgetHold("debt_clear_partial_proof_required")
			}
			if _, err := validatePartialRepaymentProjection(r, effects, s, *plan.RepaymentProjection); err != nil {
				return false, err
			}
		}
	}
	if state.Unwind != nil || s.Unwind || s.CutoverDrain || s.LeverageTargetLevel == 1 || state.Target != nil && state.Target.Lane == s.RouteLane && state.Target.Level == 1 {
		return true, nil
	}
	// Re-evaluate the actual partial planner branch, including its fallback
	// bounds, rather than trusting an operation reason copied into the journal.
	matches := func(a Action, reason string, amount int64, ok bool) bool {
		return ok && decision.Action == a && decision.Reason == reason && decision.AmountRaw == amount
	}
	if matches(partialWithdrawalStep(s)) || matches(leverageDownPartialStep(s)) {
		return false, nil
	}
	return true, nil
}

// Emergency origin is accepted only from the observer's private coherent
// account batch. Decode health, actual debt, collateral and LTV independently;
// neither a caller-supplied snapshot nor a hard_ltv reason grants authority.
func verifyDebtClearEmergency(m RouteManifest, o Observation, decision Decision, operationID string, now time.Time) (*debtClearRiskProof, error) {
	switch decision.Action {
	case ReportNAV, VoltrRestoreIdle, InitializeKaminoObligation, OpenRouteStep, OpenPrimeUSDCStep, SwapStableToCollateralStep, SwapUSDCToPrimeStep, SwapDebtToCollateralStep:
		return nil, nil
	}
	proof, err := verifyDebtClearRiskBatch(m, o, operationID, now)
	if err != nil || proof == nil {
		return proof, err
	}
	if !decisionsEqual(m.DecideOnManifest(o.Snapshot), decision) {
		return nil, budgetHold("debt_clear_emergency_action_invalid")
	}
	switch decision.Action {
	case DeleverRouteStep, DeleverPrimeUSDCStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapCollateralToStableStep, SwapPrimeToUSDCStep:
		return proof, nil
	default:
		return nil, budgetHold("debt_clear_emergency_action_invalid")
	}
}

func verifyDebtClearRiskBatch(m RouteManifest, o Observation, operationID string, now time.Time) (*debtClearRiskProof, error) {
	s, b := o.Snapshot, o.routeBatch
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	if !s.HasPosition || s.PositionDebtRaw <= 0 || hard <= TargetLTVBPS || s.LTVBPS < hard {
		return nil, nil
	}
	if b == nil || b.ManifestSHA256 != m.SHA256 || b.ObservationID != s.ObservationID || b.Slot != s.Slot || !s.Fresh || !freshAt(now, o.ObservedAt, 30*time.Second) {
		return nil, budgetHold("debt_clear_emergency_evidence_unavailable")
	}
	if o.ValuationSource != "confirmed" && o.ValuationSource != routeRefreshValuationSource || o.ValuationSlot != b.Slot || s.ValuationSource != o.ValuationSource || s.ValuationSlot != o.ValuationSlot {
		return nil, budgetHold("debt_clear_emergency_provenance_invalid")
	}
	for _, account := range b.Accounts {
		if o.ValuationSource == routeRefreshValuationSource {
			if account.ValuationSource != routeRefreshValuationSource || account.ValuationSlot != b.Slot {
				return nil, budgetHold("debt_clear_emergency_provenance_invalid")
			}
		} else if account.ValuationSource != "" && account.ValuationSource != "confirmed" {
			return nil, budgetHold("debt_clear_emergency_provenance_invalid")
		}
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return nil, err
	}
	if m.selectorObservation || s.PilotActive {
		selected, err := observedSelectorRouteForManifest(b.Accounts, s.RouteLane, m)
		if err != nil || selected.Lane != s.RouteLane {
			return nil, budgetHold("debt_clear_emergency_scope_changed")
		}
	}
	obligation, err := decodeKaminoObligation(accountAt(b.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return nil, err
	}
	collateral, err := decodeKaminoReserve(accountAt(b.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return nil, err
	}
	debt, err := decodeKaminoReserve(accountAt(b.Accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return nil, err
	}
	emergency, err := decodeKaminoMarketEmergency(accountAt(b.Accounts, route.Kamino.Market), route.Kamino)
	if err != nil {
		return nil, err
	}
	if err = validateKaminoReserveHealth(b.Slot, emergency, obligation, collateral, debt); err != nil {
		return nil, err
	}
	if err = validateKaminoOracleAge(clockUnixTimestamp(b.Accounts), collateral, debt); err != nil {
		return nil, err
	}
	rawDebt, err := obligation.debtAtReserveRate(debt)
	if err != nil {
		return nil, err
	}
	redeemable, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return nil, err
	}
	ltv, err := observedLTVBPS(KaminoPosition{DebtRaw: rawDebt, RedeemablePrimeRaw: redeemable, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF})
	verifiedHard := min(int64(collateral.liquidationThresholdPct)*100-1500, int64(6000))
	if err != nil || rawDebt != uint64(s.PositionDebtRaw) || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || ltv != s.LTVBPS || verifiedHard != hard || ltv < verifiedHard {
		return nil, budgetHold("debt_clear_emergency_evidence_changed")
	}
	return &debtClearRiskProof{OperationID: operationID, ObservationID: s.ObservationID, AccountsSHA256: hashConfirmedAccounts(b.Accounts), ValuationSource: o.ValuationSource, Slot: s.Slot, ObservedAt: o.ObservedAt, LTVBPS: ltv, HardLTVBPS: verifiedHard}, nil
}

// Existing phase3 callers hold the same route lease/row lock for all three
// fences. Only admission may bind a new flow; build/send never adopt authority.
func (d *Database) authorizeDebtClearTx(ctx context.Context, tx pgx.Tx, m RouteManifest, operationID string, request any, effects ExpectedEffects, plan *phase3BridgeAdmission, auth *phase3OperationAuthorization, risk *debtClearRiskProof, slot int64, admission bool) error {
	var routeKey string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT op.route_key,route.state FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE operation_id=$1`, operationID).Scan(&routeKey, &raw); err != nil {
		return err
	}
	var state debtClearRouteState
	if json.Unmarshal(raw, &state) != nil {
		return budgetHold("debt_clear_state_invalid")
	}
	required, err := debtClearRequired(m, request, effects, plan, state)
	required = required || auth.DebtClear != nil && !debtClearPassiveRequest(request)
	if err != nil {
		return err
	}
	if !required {
		// Reporting is always available, including after consent expiry. Account
		// for its admitted cost without granting permission to a later capital leg.
		if admission && auth.GoalID == "" && plan != nil && state.Authority != nil && plan.Snapshot.RouteLane == state.Authority.Origin.SourceLane {
			a := *state.Authority
			if plan.CurrentCost.TotalMicros <= 0 || a.UsedCostMicros > math.MaxInt64-plan.CurrentCost.TotalMicros {
				return budgetHold("debt_clear_cost_overflow")
			}
			a.UsedCostMicros += plan.CurrentCost.TotalMicros
			auth.DebtClear = &a
			encoded, err := json.Marshal(a)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{debtClearAuthority}',$2::jsonb,true) WHERE route_key=$1`, routeKey, string(encoded))
			return err
		}
		return nil
	}
	if plan == nil || slot < plan.Snapshot.Slot || slot > plan.ValidThroughSlot {
		return budgetHold("debt_clear_evidence_expired")
	}
	now := time.Now().UTC()
	a := state.Authority
	if admission && auth.DebtClear == nil && risk != nil {
		if risk.OperationID != operationID || risk.ObservationID != plan.Snapshot.ObservationID || !freshAt(now, risk.ObservedAt, 30*time.Second) || slot < risk.Slot || slot-risk.Slot > observationLagSlots() {
			return budgetHold("debt_clear_emergency_evidence_expired")
		}
		s := plan.Snapshot
		maxDebt := max(s.PositionDebtRaw, s.PayoffDebtRaw)
		if plan.Payoff != nil {
			if plan.Payoff.UpperDebtRaw > math.MaxInt64 {
				return budgetHold("debt_clear_scope_unavailable")
			}
			maxDebt = max(maxDebt, int64(plan.Payoff.UpperDebtRaw))
		}
		if plan.CurrentCost.TotalMicros > math.MaxInt64-plan.ExitAfterMicros {
			return budgetHold("debt_clear_cost_overflow")
		}
		origin := UnwindIntent{SourceLane: s.RouteLane, Reason: "hard_ltv_reduction", ObservationID: s.ObservationID, MaxCollateralRaw: s.PositionCollateralRaw, MaxDebtRaw: maxDebt, CostBoundRaw: plan.CurrentCost.TotalMicros + plan.ExitAfterMicros, BudgetScope: Phase3GoalID, BudgetFamily: phase3BudgetFamilyForLane(s.RouteLane), EvidenceID: risk.AccountsSHA256, CreatedAt: now}
		authority, err := newDebtClearAuthority(m, routeKey, sha256Bytes([]byte("emergency:"+operationID)), origin)
		if err != nil {
			return err
		}
		authority.Emergency = risk
		a = &authority
		state.Unwind = &origin
		encoded, err := json.Marshal(origin)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(jsonb_set(state,'{selectorUnwind}',$2::jsonb,true),'{selectorEntryPaused}','true'::jsonb,true),'{selectorEntry}','null'::jsonb,true) WHERE route_key=$1`, routeKey, string(encoded)); err != nil {
			return err
		}
	}
	if a == nil {
		return budgetHold("debt_clear_confirmation_required")
	}
	if err = a.validate(m, routeKey, now); err != nil {
		return err
	}
	if state.Unwind == nil || !sameUnwindIntent(*state.Unwind, a.Origin) || plan.Snapshot.RouteLane != a.Origin.SourceLane || plan.Snapshot.PositionDebtRaw > a.Origin.MaxDebtRaw || plan.Snapshot.PayoffDebtRaw > a.Origin.MaxDebtRaw || plan.Snapshot.PositionCollateralRaw > a.Origin.MaxCollateralRaw {
		return budgetHold("debt_clear_scope_changed")
	}
	if plan.Payoff != nil && plan.Payoff.UpperDebtRaw > uint64(a.Origin.MaxDebtRaw) {
		return budgetHold("debt_clear_scope_changed")
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok {
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return err
		}
		if leg == kaminoLegRepay && r.AmountRaw > uint64(a.Origin.MaxDebtRaw) {
			return budgetHold("debt_clear_scope_changed")
		}
	}
	if a.Emergency != nil {
		if operationID == a.Emergency.OperationID {
			if risk == nil || risk.OperationID != operationID || !freshAt(now, risk.ObservedAt, 30*time.Second) || slot < risk.Slot || slot-risk.Slot > observationLagSlots() || risk.LTVBPS < risk.HardLTVBPS {
				return budgetHold("debt_clear_emergency_evidence_expired")
			}
		} else {
			var originRaw []byte
			if err = tx.QueryRow(ctx, `SELECT expected_effects->'phase3'->'debtClear' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND status='reconciled' AND confirmation_status='finalized' AND reconciled_effects IS NOT NULL`, a.Emergency.OperationID, routeKey).Scan(&originRaw); err != nil {
				return budgetHold("debt_clear_emergency_origin_not_reconciled")
			}
			var origin debtClearAuthority
			if json.Unmarshal(originRaw, &origin) != nil || origin.ID != a.ID || origin.Emergency == nil || *origin.Emergency != *a.Emergency || !sameUnwindIntent(origin.Origin, a.Origin) {
				return budgetHold("debt_clear_emergency_origin_changed")
			}
		}
	} else {
		receipt, exists := state.Receipts[a.ID]
		if !exists || receipt.Confirmation == nil || a.Confirmation == nil || *receipt.Confirmation != *a.Confirmation || !sameUnwindIntent(receipt.Origin, a.Origin) {
			return budgetHold("debt_clear_confirmation_reused")
		}
	}
	if auth.DebtClear != nil {
		if auth.DebtClear.ID != a.ID || auth.DebtClear.FirstOperationID != a.FirstOperationID || !sameUnwindIntent(auth.DebtClear.Origin, a.Origin) {
			return budgetHold("debt_clear_operation_scope_changed")
		}
	} else if !admission {
		return budgetHold("debt_clear_operation_not_authorized")
	} else {
		if a.FirstOperationID == "" {
			a.FirstOperationID = operationID
		}
		if a.UsedCostMicros > a.Origin.CostBoundRaw || plan.CurrentCost.TotalMicros <= 0 || plan.CurrentCost.TotalMicros > a.Origin.CostBoundRaw-a.UsedCostMicros || plan.ExitAfterMicros > a.Origin.CostBoundRaw-a.UsedCostMicros-plan.CurrentCost.TotalMicros {
			return budgetHold("debt_clear_cost_bound_exceeded")
		}
		a.UsedCostMicros += plan.CurrentCost.TotalMicros
		auth.DebtClear = a
		encoded, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{debtClearAuthority}',$2::jsonb,true) WHERE route_key=$1`, routeKey, string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

func (d *Database) observeDebtClearOriginRisk(ctx context.Context, rpc *chain.Client, m RouteManifest, operationID string) (*debtClearRiskProof, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(expected_effects->'phase3','null'::jsonb) FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&raw); err != nil {
		return nil, err
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(raw, &auth) != nil {
		return nil, budgetHold("debt_clear_state_invalid")
	}
	a := auth.DebtClear
	if a == nil || a.Emergency == nil || a.Emergency.OperationID != operationID {
		return nil, nil
	}
	m.selectorObservation, m.observationLane = true, a.Origin.SourceLane
	o, err := ObserveConfirmedRouteSnapshot(ctx, rpc, m)
	if err != nil {
		return nil, budgetHold("debt_clear_emergency_evidence_unavailable")
	}
	o.Snapshot.PilotActive = auth.BridgeAdmission != nil && auth.BridgeAdmission.Snapshot.PilotActive
	if o.Snapshot.RouteLane != a.Origin.SourceLane {
		return nil, budgetHold("debt_clear_emergency_scope_changed")
	}
	proof, err := verifyDebtClearRiskBatch(m, o, operationID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if proof == nil {
		return nil, budgetHold("debt_clear_emergency_risk_no_longer_present")
	}
	return proof, nil
}

// Denial only, never send authority. Run after immutable wire/expiry validation
// and before valuation, so price outages cannot prevent expired-absent cleanup.
func (d *Database) checkSignedDebtClearConsent(ctx context.Context, m RouteManifest, operation PersistedOperation, auth phase3OperationAuthorization, request any, effects ExpectedEffects) error {
	switch r := request.(type) {
	case KaminoInitializationRequest:
		return nil
	case BridgeBuildRequest:
		if r.Action == ReportNAV || r.Action == VoltrRestoreIdle {
			return nil
		}
	}
	var raw []byte
	var routeKey string
	if err := d.pool.QueryRow(ctx, `SELECT op.route_key,route.state FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE op.operation_id=$1`, operation.ID).Scan(&routeKey, &raw); err != nil {
		return err
	}
	var state debtClearRouteState
	if json.Unmarshal(raw, &state) != nil {
		return budgetHold("debt_clear_state_invalid")
	}
	required, err := debtClearRequired(m, request, effects, auth.BridgeAdmission, state)
	required = required || auth.DebtClear != nil && !debtClearPassiveRequest(request)
	if err != nil || !required {
		return err
	}
	a := state.Authority
	if auth.DebtClear == nil || a == nil || auth.DebtClear.ID != a.ID || auth.DebtClear.FirstOperationID != a.FirstOperationID || !sameUnwindIntent(auth.DebtClear.Origin, a.Origin) || state.Unwind == nil || !sameUnwindIntent(*state.Unwind, a.Origin) {
		return budgetHold("debt_clear_operation_not_authorized")
	}
	return a.validate(m, routeKey, time.Now().UTC())
}

func debtClearPassiveRequest(request any) bool {
	switch r := request.(type) {
	case KaminoInitializationRequest:
		return true
	case BridgeBuildRequest:
		return r.Action == ReportNAV || r.Action == VoltrRestoreIdle
	default:
		return false
	}
}
