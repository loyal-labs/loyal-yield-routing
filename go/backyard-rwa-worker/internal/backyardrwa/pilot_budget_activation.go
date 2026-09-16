package backyardrwa

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"
)

// Audit all existing runtime lanes, including retired canary siblings. A
// desired pilot allocation cannot hide prior collateral or another debt mint.
var pilotTransitionLanes = []string{RouteID, PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC", "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS"}

type pilotFlatEvidence struct {
	Genesis    string             `json:"genesis"`
	Commitment string             `json:"commitment"`
	Slot       int64              `json:"slot"`
	Accounts   []ConfirmedAccount `json:"accounts"`
}

func pilotFlatAddresses() ([]string, map[string]struct{}, error) {
	addresses := []string{bridgeVoltrVault, bridgeLPMint, bridgeStrategyReceipt, reportTicketPDA, bridgeIdleATA, bridgeStrategyATA, bridgeSquadsATA}
	optional := map[string]struct{}{}
	for _, lane := range pilotTransitionLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return nil, nil, err
		}
		for _, address := range []string{route.Kamino.Obligation, route.CollateralCustody, route.DebtCustody} {
			addresses = append(addresses, address)
			if address != bridgeSquadsATA {
				optional[address] = struct{}{}
			}
		}
	}
	return uniqueNonzero(addresses), optional, nil
}
func validatePilotFlatEvidence(e pilotFlatEvidence) error {
	if e.Genesis != mainnetGenesisHash || e.Commitment != "finalized" || e.Slot <= 0 {
		return budgetHold("pilot_transition_requires_finalized_flat_state")
	}
	addresses, _, err := pilotFlatAddresses()
	if err != nil {
		return err
	}
	if len(e.Accounts) != len(addresses) {
		return budgetHold("pilot_flat_account_set_mismatch")
	}
	for i, address := range addresses {
		if e.Accounts[i].Address != address {
			return budgetHold("pilot_flat_account_set_mismatch")
		}
	}
	receipt, err := decodeStrategyReceipt(accountAt(e.Accounts, bridgeStrategyReceipt))
	if err != nil {
		return err
	}
	if receipt.PositionValueRaw != 0 || receipt.CustodyTrackedRaw != 0 {
		return budgetHold("pilot_transition_strategy_not_flat")
	}
	ticket, err := decodeObservedReportTicket(accountAt(e.Accounts, reportTicketPDA))
	if err != nil {
		return err
	}
	if ticket.Armed {
		return budgetHold("pilot_transition_ticket_armed")
	}
	idle, err := decodePinnedUSDC(accountAt(e.Accounts, bridgeIdleATA), bridgeIdleAuthority)
	if err != nil {
		return err
	}
	book, err := decodeVoltrVaultBook(accountAt(e.Accounts, bridgeVoltrVault))
	if err != nil {
		return err
	}
	supply, err := decodeVoltrLPSupply(accountAt(e.Accounts, bridgeLPMint))
	if err != nil {
		return err
	}
	if book.TotalValueRaw != idle.Raw || idle.Raw > uint64(PilotDepositCapRaw) || supply != 0 || book.FeeAccumulatorManagerRaw != 0 || book.FeeAccumulatorAdminRaw != 0 || book.FeeAccumulatorProtocolRaw != 0 {
		return budgetHold("pilot_transition_vault_book_not_empty")
	}
	for _, custody := range []struct{ address, authority string }{{bridgeStrategyATA, bridgeStrategyAuth}, {bridgeSquadsATA, bridgeVault}} {
		token, err := decodePinnedUSDC(accountAt(e.Accounts, custody.address), custody.authority)
		if err != nil {
			return err
		}
		if token.Raw != 0 {
			return budgetHold("pilot_transition_cash_not_returned")
		}
	}
	owner, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return err
	}
	for _, lane := range pilotTransitionLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return err
		}
		obligation := accountAt(e.Accounts, route.Kamino.Obligation)
		if obligation.Lamports == 0 {
			if obligation.Owner != "" || len(obligation.Data) != 0 || obligation.Executable {
				return budgetHold("invalid_absent_pilot_obligation")
			}
		} else {
			if _, err := decodeKaminoObligation(obligation, route.Kamino); err != nil {
				return err
			}
			// Even malformed slots with an absent reserve key cannot hide raw debt.
			for i := 0; i < 8; i++ {
				if binary.LittleEndian.Uint64(obligation.Data[128+i*136:136+i*136]) != 0 {
					return budgetHold("pilot_transition_collateral_not_flat")
				}
			}
			for i := 0; i < 5; i++ {
				if !allZero(obligation.Data[1296+i*200 : 1312+i*200]) {
					return budgetHold("pilot_transition_debt_not_flat")
				}
			}
		}
		for _, custody := range []struct{ address, mint, program string }{{route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram}, {route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram}} {
			account := accountAt(e.Accounts, custody.address)
			if account.Lamports == 0 {
				if account.Owner != "" || len(account.Data) != 0 || account.Executable {
					return budgetHold("invalid_absent_pilot_custody")
				}
				continue
			}
			mint, err := decodeBase58PublicKey(custody.mint)
			if err != nil {
				return err
			}
			if account.Owner != custody.program || account.Executable {
				return budgetHold("pilot_transition_custody_identity")
			}
			token, err := DecodeTokenCustody(account.Owner, account.Data, mint, owner)
			if err != nil {
				return err
			}
			if token.Raw != 0 {
				return budgetHold("pilot_transition_custody_not_flat")
			}
		}
	}
	return nil
}
func observePilotFlatEvidence(ctx context.Context, rpc *RPCClient) (pilotFlatEvidence, error) {
	if rpc == nil {
		return pilotFlatEvidence{}, budgetHold("pilot_flat_rpc_unavailable")
	}
	genesis, err := rpc.GenesisHash(ctx)
	if err != nil || genesis != mainnetGenesisHash {
		return pilotFlatEvidence{}, budgetHold("pilot_transition_wrong_chain")
	}
	minimum, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		return pilotFlatEvidence{}, err
	}
	addresses, optional, err := pilotFlatAddresses()
	if err != nil {
		return pilotFlatEvidence{}, err
	}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, minimum, optional, "finalized")
	if err != nil {
		return pilotFlatEvidence{}, err
	}
	evidence := pilotFlatEvidence{genesis, "finalized", slot, accounts}
	return evidence, validatePilotFlatEvidence(evidence)
}

type pilotBudgetActivation struct {
	Authority      pilotBudgetAuthority `json:"authority"`
	PreviousBudget json.RawMessage      `json:"previousBudget"`
	FlatEvidence   pilotFlatEvidence    `json:"flatEvidence"`
}

// Explicit bookkeeping operation under the existing route lease. Never called
// by Tick/admission and never loads a signer or enables deposits. Fresh chain
// evidence is acquired while the route row is locked; no caller supplies it.
func (d *Database) activatePilotBudget(ctx context.Context, rpc *RPCClient) (pilotBudgetActivation, error) {
	var result pilotBudgetActivation
	lease, err := d.currentLease()
	if err != nil || lease.RouteKey != productionRouteKey {
		return result, ErrRouteLeaseLost
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, lease.RouteKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return result, err
	}
	var state map[string]json.RawMessage
	var generation int64
	if json.Unmarshal(raw, &state) != nil || state == nil || version <= 0 || version == math.MaxInt64 || json.Unmarshal(state["generation"], &generation) != nil || generation != version {
		return result, budgetHold("invalid_pilot_transition_route_state")
	}
	priorJSON, hasBudget := state["phase3"]
	markerJSON, hasMarker := state["phase3Initialization"]
	var prior Phase3Budget
	if hasBudget != hasMarker {
		return result, budgetHold("incomplete_phase3_initialization")
	}
	if hasBudget {
		var marker phase3Initialization
		if json.Unmarshal(priorJSON, &prior) != nil || prior.validate() != nil || json.Unmarshal(markerJSON, &marker) != nil || marker.GoalID != Phase3GoalID || marker.Generation < 2 || marker.Generation > version || marker.CreatedAt.IsZero() {
			return result, budgetHold("invalid_pilot_transition_prior_budget")
		}
		if prior.Pilot != nil {
			return validatePersistedPilotActivation(prior, state["pilotBudgetActivation"], version)
		}
	} else {
		var orphaned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND expected_effects ? 'phase3')`, lease.RouteKey).Scan(&orphaned); err != nil {
			return result, err
		}
		if orphaned {
			return result, budgetHold("historical_authorization_without_budget")
		}
		prior = Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{"OnRe": {}, "AUTO": {}, "Ethena": {}}, Reservations: map[string]BudgetReservation{}}
	}
	if _, exists := state["pilotBudgetActivation"]; exists {
		return result, budgetHold("orphaned_pilot_activation_marker")
	}
	if value, exists := state["phase3SetupIntent"]; exists && string(value) != "null" {
		return result, budgetHold("pilot_transition_setup_intent_pending")
	}
	if value, exists := state["selectorUnwind"]; exists && string(value) != "null" {
		return result, budgetHold("pilot_transition_unwind_pending")
	}
	var active, recovery, latched bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling'))`, lease.RouteKey).Scan(&active); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, lease.RouteKey).Scan(&recovery); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL)`, lease.RouteKey).Scan(&latched); err != nil {
		return result, err
	}
	if !latched {
		if err = tx.QueryRow(ctx, `SELECT EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, lease.RouteKey).Scan(&latched); err != nil {
			return result, err
		}
	}
	if active || recovery || latched {
		return result, budgetHold("unresolved_work_prevents_pilot_transition")
	}
	// Scope migration is mandatory when old accounting evidence still exists.
	var staleAccounting bool
	if err = tx.QueryRow(ctx, ActiveStrategyJournalCTE+`SELECT EXISTS(SELECT 1 FROM strategy_journal WHERE route_key=$1 AND confirmed_slot<=444157954 AND engine_version='backyard_rwa_v1')`, lease.RouteKey).Scan(&staleAccounting); err != nil {
		return result, err
	}
	if staleAccounting {
		return result, budgetHold("pilot_transition_requires_audited_journal_association")
	}
	flat, err := observePilotFlatEvidence(ctx, rpc)
	if err != nil {
		return result, err
	}
	encodedFlat, err := json.Marshal(flat)
	if err != nil {
		return result, err
	}
	previous, err := json.Marshal(prior)
	if err != nil {
		return result, err
	}
	if !hasBudget {
		previous = []byte("null")
	}
	authority := pilotBudgetAuthority{Schema: pilotBudgetAuthoritySchema, AuthorityID: pilotBudgetAuthorityID, PreviousBudgetWasAbsent: !hasBudget, PreviousBudgetSHA256: sha256Bytes(previous), Generation: version + 1, FinalizedSlot: flat.Slot, FlatEvidenceSHA256: sha256Bytes(encodedFlat)}
	next, err := activatePilotBudget(prior, authority)
	if err != nil {
		return result, err
	}
	budgetJSON, err := json.Marshal(next)
	if err != nil {
		return result, err
	}
	result = pilotBudgetActivation{authority, previous, flat}
	activationJSON, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if !hasBudget {
		markerJSON, err = json.Marshal(phase3Initialization{GoalID: Phase3GoalID, Generation: version + 1, CreatedAt: time.Now().UTC()})
		if err != nil {
			return result, err
		}
	}
	updated, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(jsonb_set(jsonb_set(state,'{phase3}',$4::jsonb,true),'{phase3Initialization}',$5::jsonb,true),'{pilotBudgetActivation}',$6::jsonb,true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, lease.RouteKey, lease.Owner, lease.FencingToken, string(budgetJSON), string(markerJSON), string(activationJSON))
	if err != nil {
		return result, err
	}
	if updated.RowsAffected() != 1 {
		return result, ErrRouteLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func canonicalPriorBudgetDigest(raw json.RawMessage) string {
	if string(raw) == "null" {
		return sha256Bytes([]byte("null"))
	}
	var prior Phase3Budget
	if json.Unmarshal(raw, &prior) != nil || prior.validate() != nil {
		return ""
	}
	encoded, err := json.Marshal(prior)
	if err != nil {
		return ""
	}
	return sha256Bytes(encoded)
}

// InspectPilotBudgetFlatState is read-only and has no database or signer path.
// Retain successfully read public accounts even when a readiness check fails.
func InspectPilotBudgetFlatState(ctx context.Context, rpcURL string, out io.Writer) error {
	rpc, err := NewRPCClient(rpcURL)
	if err != nil {
		return budgetHold("pilot_flat_rpc_unavailable")
	}
	evidence, err := observePilotFlatEvidence(ctx, rpc)
	var hold *BudgetHold
	if err != nil && !errors.As(err, &hold) {
		hold = &BudgetHold{Reason: "pilot_flat_observation_failed"}
	}
	if encodeErr := json.NewEncoder(out).Encode(struct {
		ProofLevel string            `json:"proofLevel"`
		Admissible bool              `json:"admissible"`
		Hold       *BudgetHold       `json:"hold,omitempty"`
		Evidence   pilotFlatEvidence `json:"evidence"`
	}{"FINALIZED_ACCOUNT_READ", err == nil, hold, evidence}); encodeErr != nil {
		return encodeErr
	}
	if hold != nil {
		return hold
	}
	return nil
}

// Revalidate the immutable authority and inherited counters at every durable
// pilot budget read, including retries after PostgreSQL jsonb key reordering.
func validatePersistedPilotActivation(current Phase3Budget, raw json.RawMessage, version int64) (pilotBudgetActivation, error) {
	var activation pilotBudgetActivation
	fail := func() (pilotBudgetActivation, error) {
		return activation, budgetHold("incoherent_pilot_activation_evidence")
	}
	if current.Pilot == nil || current.validate() != nil || json.Unmarshal(raw, &activation) != nil || activation.Authority != *current.Pilot || current.Pilot.Generation > version || current.Pilot.FinalizedSlot != activation.FlatEvidence.Slot {
		return fail()
	}
	encoded, err := json.Marshal(activation.FlatEvidence)
	if err != nil || validatePilotFlatEvidence(activation.FlatEvidence) != nil || sha256Bytes(encoded) != current.Pilot.FlatEvidenceSHA256 || canonicalPriorBudgetDigest(activation.PreviousBudget) != current.Pilot.PreviousBudgetSHA256 {
		return fail()
	}
	absent := string(activation.PreviousBudget) == "null"
	if absent != current.Pilot.PreviousBudgetWasAbsent {
		return fail()
	}
	prior := Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{"OnRe": {}, "AUTO": {}, "Ethena": {}}, Reservations: map[string]BudgetReservation{}}
	if !absent && json.Unmarshal(activation.PreviousBudget, &prior) != nil {
		return fail()
	}
	baseline, err := activatePilotBudget(prior, activation.Authority)
	if err != nil {
		return fail()
	}
	for family, old := range baseline.Families {
		row, exists := current.Families[family]
		if !exists || row.SpentMicros < old.SpentMicros || row.ExecutionCostSpentMicros < old.ExecutionCostSpentMicros {
			return fail()
		}
	}
	return activation, nil
}
