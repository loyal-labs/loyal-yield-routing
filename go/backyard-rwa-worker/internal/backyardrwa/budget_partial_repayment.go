package backyardrwa

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Partial repayment has the same recovery budget as any other exit. Its
// simulated poststate prices a complete remaining exit, never settled NAV.
func observePhase3PartialRepaymentAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := o.Snapshot, e.Request
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || rpc == nil || client == nil || !s.PilotActive || !selectorLane(s.RouteLane) || !s.Fresh || s.Slot <= 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteKind != RouteKind || s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw <= 1 || d.Action != DeleverRouteStep || r.Action != d.Action || d.Reason != "hard_ltv_partial_repay" || !decisionsEqual(Decide(s), d) || d.AmountRaw <= 0 || uint64(d.AmountRaw) != r.AmountRaw || d.AmountRaw >= s.PositionDebtRaw || debtCashRaw(s) < d.AmountRaw || leg != kaminoLegRepay || r.FullPayoff || r.RepaymentRelease {
		return phase3BridgeAdmission{}, budgetHold("partial_repayment_admission_unavailable")
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	before, accounts, err := observeKaminoPayoffWindow(ctx, rpc, route, max(s.Slot, current.ObservationSlot), 1)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if before.ObservedDebtRaw != uint64(s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("partial_repayment_prestate_changed")
	}
	for _, effect := range e.ExpectedEffects.Accounts {
		a := accountAt(accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || c.Raw != effect.BeforeRaw {
			return phase3BridgeAdmission{}, budgetHold("partial_repayment_prestate_changed")
		}
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	addresses := depositProjectionAddresses(route)
	addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply)
	projection, err := rpc.simulatePhase3EntryProjection(ctx, message, addresses, before.ObservedSlot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if _, err = validatePartialRepaymentProjection(r, e.ExpectedEffects, s, projection); err != nil {
		return phase3BridgeAdmission{}, err
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err == nil {
		plan.RepaymentProjection = &projection
	}
	return plan, err
}

// Called within the existing locked admission transaction. writePhase3BudgetTx
// advances the generation for this combined mutation. A risk reduction always
// continues to idle; residual cash must not be classified as borrowed capital.
func (d *Database) persistPartialRepaymentUnwindTx(ctx context.Context, tx pgx.Tx, plan phase3BridgeAdmission, budget Phase3Budget, intentSHA string) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT state->'selectorUnwind' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, lease.RouteKey).Scan(&raw); err != nil {
		return err
	}
	if len(raw) > 0 && string(raw) != "null" {
		var old UnwindIntent
		if json.Unmarshal(raw, &old) != nil || old.validate() != nil || old.SourceLane != plan.Snapshot.RouteLane || old.BudgetScope != budget.GoalID {
			return budgetHold("partial_repayment_unwind_conflict")
		}
	} else {
		s := plan.Snapshot
		family := phase3BudgetFamilyForLane(s.RouteLane)
		i := UnwindIntent{SourceLane: s.RouteLane, Reason: "hard_ltv_reduction", ObservationID: s.ObservationID, MaxCollateralRaw: s.PositionCollateralRaw, MaxDebtRaw: max(s.PositionDebtRaw, s.PayoffDebtRaw), CostBoundRaw: budget.Families[family].ExitMicros, BudgetScope: budget.GoalID, BudgetFamily: family, EvidenceID: intentSHA, CreatedAt: time.Now().UTC()}
		if err = i.validate(); err != nil {
			return err
		}
		raw, err = json.Marshal(i)
		if err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(jsonb_set(state,'{selectorUnwind}',$4::jsonb,true),'{selectorEntryPaused}','true'::jsonb,true),'{selectorEntry}','null'::jsonb,true) WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, lease.RouteKey, lease.Owner, lease.FencingToken, string(raw))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRouteLeaseLost
	}
	return nil
}

func validatePartialRepaymentProjection(r KaminoPrimeUSDCRequest, e ExpectedEffects, s Snapshot, p phase3KaminoProjection) (KaminoPayoffBound, error) {
	message, err := CompileKaminoMessage(r)
	_, leg, legErr := kaminoPrimeUSDCInstruction(r)
	if err != nil || legErr != nil || leg != kaminoLegRepay || r.Action != DeleverRouteStep || r.FullPayoff || r.RepaymentRelease || !s.PilotActive || !selectorLane(r.RouteLane) || r.RouteLane != s.RouteLane || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 || p.Slot < s.Slot || p.Slot-s.Slot > budgetMaxObservationLagSlots || r.AmountRaw == 0 || s.PositionDebtRaw <= 0 || r.AmountRaw >= uint64(s.PositionDebtRaw) || e.Repayment == nil || e.Repayment.MinimumDebitRaw != r.AmountRaw || e.Repayment.MaximumDebitRaw != r.AmountRaw {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_identity_mismatch")
	}
	if _, err = MeasureExecutableDebit(r, e); err != nil {
		return KaminoPayoffBound{}, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, err
	}
	if debtCashRaw(s) < 0 || len(e.Accounts) != 2 || e.Accounts[0].BeforeRaw != uint64(debtCashRaw(s)) {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_cash_mismatch")
	}
	for _, effect := range e.Accounts {
		a := accountAt(p.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || c.Raw != effect.AfterRaw {
			return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_custody_mismatch")
		}
	}
	o, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || s.PositionCollateralRaw <= 0 || o.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || o.debtRaw == 0 || o.debtRaw >= uint64(s.PositionDebtRaw) || o.debtRaw < uint64(s.PositionDebtRaw)-r.AmountRaw {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_position_mismatch")
	}
	a := accountAt(p.Accounts, route.CollateralCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
	if err != nil || a.Executable || a.Lamports == 0 || s.CollateralIdleRaw < 0 || c.Raw != uint64(s.CollateralIdleRaw) {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_collateral_changed")
	}
	return decodeKaminoPayoffWindow(p.Accounts, route, p.Slot, 3)
}
