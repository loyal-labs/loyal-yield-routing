package fleetexec

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type CrossMintPhase string

const (
	CrossMintSourceReserve      CrossMintPhase = "source_reserve"
	CrossMintSourceIdle         CrossMintPhase = "source_idle"
	CrossMintTargetIdle         CrossMintPhase = "target_idle"
	CrossMintTargetReserve      CrossMintPhase = "target_reserve"
	CrossMintClosedByUser       CrossMintPhase = "closed_by_user"
	CrossMintManualIntervention CrossMintPhase = "manual_intervention"
)

// These aliases preserve the retained Rust receipt JSON contract. A token
// delta and a complete account balance share a layout, but not a meaning.
type CrossMintTokenAmount = crossMintTokenAnchor
type CrossMintPositionAnchor = crossMintPositionAnchor
type CrossMintBalanceAnchors = crossMintAnchors

type CrossMintExpectedEffect struct {
	Debit                  *CrossMintTokenAmount `json:"debit"`
	CreditMint             *string               `json:"creditMint"`
	CreditTokenAccount     *string               `json:"creditTokenAccount"`
	MinimumCreditAmountRaw *int64                `json:"minimumCreditAmountRaw"`
}
type CrossMintEffect struct {
	Debit  *CrossMintTokenAmount `json:"debit"`
	Credit *CrossMintTokenAmount `json:"credit"`
}

type CrossMintMovement struct {
	DecisionID, OpportunityID, OptimizerEpochID, VaultID      int64
	Cluster, VaultPubkey                                      string
	SourceSnapshotID                                          *int64
	SourceReserve, IntendedTargetReserve, ActiveTargetReserve string
	SourceMint, TargetMint                                    string
	PlannedAmountRaw                                          int64
	ExecutionPlan, PreflightCertification                     json.RawMessage
	CustodyMint, CustodyAccount                               string
	CustodyAmountRaw                                          int64
	CustodyObservedBalanceRaw, CustodyReconciledSlot          *int64
	CustodyVersion                                            int64
	Phase                                                     CrossMintPhase
	TerminalOutcome                                           *string
}
type CrossMintContinuationLease struct {
	Movement                        CrossMintMovement
	Owner                           string
	FencingToken, ControlGeneration int64
	ExpiresAt                       time.Time
}
type CrossMintGates struct {
	StartNewMovements, ContinueOrRecoverExisting bool
	Generation                                   int64
}

type CrossMintLegRequest struct {
	Movement                                    CrossMintMovement
	Leg, Purpose                                string
	Generation, RemainingFeeLamports            int64
	ContinuationOwner                           string
	ContinuationFencingToken, ControlGeneration int64
	ExpiresAt                                   time.Time
}
type CrossMintPreparedLeg struct {
	Preparation                     fleet.RoutePreparation
	LastValidBlockHeight            int64
	PolicyAccount                   string
	ExpectedEffect                  CrossMintExpectedEffect
	BalanceAnchors                  CrossMintBalanceAnchors
	ConflictKeys                    []string
	SelectedALTs                    []fleet.ExecutionALT
	ExternalALTs                    []CrossMintExternalALT
	AltSelectionFingerprint         string
	WaitingALT                      bool
	MissingAddresses                []string
	SharedAddresses, VaultAddresses []CrossMintALTAddress
}

type CrossMintALTAddress struct {
	Address, SemanticClass, AccountRole string
	Ordinal                             int32
	Writable                            bool
}

// The composition root adapts the concrete signer-free fleet factory here;
// fleet does not import its signing consumer. Errors never authorize a send.
type CrossMintLegFactory interface {
	PrepareCrossMintLeg(context.Context, CrossMintLegRequest) (CrossMintPreparedLeg, error)
}

var ErrCrossMintSwapUnavailable = errors.New("cross-mint swap is currently ineligible; source recovery required")

type CrossMintController struct {
	store          *Store
	factory        CrossMintLegFactory
	signer         DelegateSigner
	cluster, owner string
	ttl            time.Duration
	swapEnabled    bool
	accounts       finalizedAccountReader
	history        finalizedHistoryReader
	adapter        *RPCAdapter
	marketEvidence fleet.MarketEpochSource
}

// The controller only signs and journals. Broadcast, finalized receipt proof,
// and ambiguity recovery remain the existing executor's responsibility.
func NewCrossMintController(store *Store, factory CrossMintLegFactory, signer DelegateSigner, adapter *RPCAdapter, cluster, owner string, ttl time.Duration, swapEnabled bool) (*CrossMintController, error) {
	if store == nil || store.pool == nil || factory == nil || adapter == nil || len(signer.FeePayer) != ed25519.PrivateKeySize || cluster == "" || owner == "" || ttl < 10*time.Second || ttl > 300*time.Second || ttl%time.Second != 0 {
		return nil, errors.New("cross-mint controller requires store, factory, identities and 10-300 second lease")
	}
	return &CrossMintController{store: store, factory: factory, signer: signer, cluster: cluster, owner: owner, ttl: ttl, swapEnabled: swapEnabled, accounts: fleet.NewRPCClient(adapter.url), history: adapter, adapter: adapter}, nil
}

// ContinueOne is deliberately separate from Worker.Tick until the composed
// receipt/history owner proves all phases. No new opportunity is admitted here.
func (c *CrossMintController) ContinueOne(ctx context.Context) (int64, bool, error) {
	lease, err := c.store.ClaimCrossMintContinuation(ctx, c.cluster, c.owner, c.ttl)
	if err != nil || lease == nil {
		return 0, false, err
	}
	workCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	gates, err := c.store.CrossMintGates(workCtx, c.cluster)
	if err != nil {
		return 0, true, err
	}
	request, err := nextCrossMintRequest(lease.Movement, c.swapEnabled)
	if err != nil {
		return 0, true, err
	}
	if !gates.ContinueOrRecoverExisting || gates.Generation != lease.ControlGeneration {
		return 0, true, ErrStaleOwner
	}
	initialAllowed := gates.StartNewMovements && c.swapEnabled
	if request.Leg == LegWithdraw && initialAllowed {
		initialAllowed, err = c.store.crossMintInitialAuthority(workCtx, *lease)
		if err != nil {
			return 0, true, err
		}
	}
	if request.Leg == LegWithdraw && !initialAllowed {
		var slot int64
		if err = c.adapter.call(workCtx, &slot, "getSlot", map[string]any{"commitment": "finalized"}); err != nil {
			return 0, true, err
		}
		if !c.swapEnabled {
			err = c.store.cancelUntouchedCrossMint(workCtx, *lease, slot, crossMintRolloutDisabled)
		} else {
			err = c.store.CancelUntouchedCrossMint(workCtx, *lease, slot)
		}
		return 0, true, err
	}
	if err = c.verifyIdleCustody(workCtx, lease.Movement); err != nil {
		return 0, true, err
	}
	var targetEpoch fleet.ImmutableMarketEpoch
	if lease.Movement.Phase == CrossMintTargetIdle {
		targetEpoch, err = c.loadFallbackEpoch(workCtx, lease.Movement)
		if err != nil {
			return 0, true, err
		}
		if !crossMintActiveTargetEligible(targetEpoch, lease.Movement) {
			return 0, true, c.rebindFallback(workCtx, *lease, targetEpoch)
		}
	}
	request.ContinuationOwner, request.ContinuationFencingToken, request.ControlGeneration, request.ExpiresAt = lease.Owner, lease.FencingToken, lease.ControlGeneration, lease.ExpiresAt
	request.Generation, request.RemainingFeeLamports, err = c.store.CrossMintLegBudget(workCtx, *lease, request.Leg)
	if err != nil {
		return 0, true, err
	}
	prepared, err := c.factory.PrepareCrossMintLeg(workCtx, request)
	if errors.Is(err, ErrCrossMintTargetUnavailable) && lease.Movement.Phase == CrossMintTargetIdle && workCtx.Err() == nil {
		return 0, true, c.rebindFallback(workCtx, *lease, targetEpoch)
	}
	if errors.Is(err, ErrCrossMintSwapUnavailable) && request.Leg == LegSwap && workCtx.Err() == nil {
		request.Leg, request.Purpose = LegDeposit, PurposeRecoverSource
		request.Generation, request.RemainingFeeLamports, err = c.store.CrossMintLegBudget(workCtx, *lease, request.Leg)
		if err == nil {
			prepared, err = c.factory.PrepareCrossMintLeg(workCtx, request)
		}
	}
	if err != nil {
		return 0, true, err
	}
	if prepared.WaitingALT {
		_, err = c.store.QueueCrossMintALT(workCtx, *lease, request, prepared)
		return 0, true, err
	}
	if err = validateCrossMintPrepared(request, prepared); err != nil {
		return 0, true, err
	}
	if err = c.verifyIdleCustody(workCtx, lease.Movement); err != nil {
		return 0, true, err
	}
	wire, err := c.signer.SignPreparedRoute(prepared.Preparation.Transaction, prepared.LastValidBlockHeight)
	if err != nil {
		return 0, true, err
	}
	id, err := c.store.AppendCrossMintLeg(workCtx, *lease, request, prepared, wire)
	return id, true, err
}

// A coherent amount is necessary but insufficient: external spending followed
// by replenishment can restore the balance. Retain D's finalized address
// history protocol and require a recognized signature at the custody anchor.
func (c *CrossMintController) verifyIdleCustody(ctx context.Context, m CrossMintMovement) error {
	if m.Phase == CrossMintSourceReserve {
		return nil
	}
	if m.Phase != CrossMintSourceIdle && m.Phase != CrossMintTargetIdle || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0 || m.CustodyObservedBalanceRaw == nil || m.CustodyAmountRaw <= 0 || *m.CustodyObservedBalanceRaw < m.CustodyAmountRaw || c.accounts == nil || c.history == nil {
		return errors.New("idle custody lacks finalized historical anchor or proof owner")
	}
	recognized, err := c.store.crossMintRecognizedSignatures(ctx, m.DecisionID, *m.CustodyReconciledSlot)
	if err != nil {
		return err
	}
	return verifyCrossMintIdleCustody(ctx, m, c.accounts, c.history, recognized)
}

// This proof is shared by the two signing checks; recognized signatures must
// come from this movement's reconciled durable journal, never an RPC allowlist.
func verifyCrossMintIdleCustody(ctx context.Context, m CrossMintMovement, reader finalizedAccountReader, history finalizedHistoryReader, recognized map[string]bool) error {
	if m.Phase != CrossMintSourceIdle && m.Phase != CrossMintTargetIdle || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0 || m.CustodyObservedBalanceRaw == nil || m.CustodyAmountRaw <= 0 || *m.CustodyObservedBalanceRaw < m.CustodyAmountRaw || reader == nil || history == nil {
		return errors.New("idle custody lacks finalized historical anchor or proof owner")
	}
	slot, accounts, err := reader.FinalizedAccounts(ctx, []string{m.CustodyAccount}, *m.CustodyReconciledSlot)
	if err != nil {
		return err
	}
	if slot < *m.CustodyReconciledSlot || len(accounts) != 1 || accounts[0].Address != m.CustodyAccount {
		return errors.New("finalized custody account readback is incomplete or stale")
	}
	amount, err := custodyTokenAmount(accounts[0], m.CustodyMint, m.VaultPubkey)
	if err != nil {
		return err
	}
	if amount != *m.CustodyObservedBalanceRaw || amount < m.CustodyAmountRaw {
		return errors.New("finalized custody aggregate changed or cannot cover attributed spend")
	}
	_, err = verifyCustodyHistory(ctx, history, m.CustodyAccount, *m.CustodyReconciledSlot, slot, recognized, true)
	return err
}

func nextCrossMintRequest(m CrossMintMovement, swapEnabled bool) (CrossMintLegRequest, error) {
	r := CrossMintLegRequest{Movement: m, Purpose: PurposeOptimizeYield}
	if m.TerminalOutcome != nil {
		return r, ErrNotClaimable
	}
	switch m.Phase {
	case CrossMintSourceReserve:
		r.Leg = LegWithdraw
	case CrossMintSourceIdle:
		if swapEnabled {
			r.Leg = LegSwap
		} else {
			r.Leg, r.Purpose = LegDeposit, PurposeRecoverSource
		}
	case CrossMintTargetIdle:
		r.Leg = LegDeposit
		if m.ActiveTargetReserve != m.IntendedTargetReserve {
			r.Purpose = PurposeFallbackTarget
		}
	default:
		return r, ErrNotClaimable
	}
	return r, nil
}

func validateCrossMintPrepared(r CrossMintLegRequest, p CrossMintPreparedLeg) error {
	t, s := p.Preparation.Transaction, p.Preparation.Simulation
	if r.Generation <= 0 || r.RemainingFeeLamports <= 0 || t.FeeLamports == 0 || t.FeeLamports > math.MaxInt64 || t.FeeLamports > uint64(r.RemainingFeeLamports) || p.LastValidBlockHeight <= 0 || p.PolicyAccount == "" || p.Preparation.RequirementsFingerprint == "" || p.AltSelectionFingerprint == "" || len(p.ConflictKeys) < 2 || !s.Succeeded || s.Error != "" || s.Slot <= 0 || s.UnitsConsumed > t.ComputeLimit || s.WireSHA256 != t.WireSHA256 || !json.Valid(p.Preparation.ExecutionPlan) {
		return errors.New("cross-mint prepared leg lacks exact fee, simulation, policy, conflicts or ALT evidence")
	}
	if r.Movement.CustodyReconciledSlot != nil && s.Slot < *r.Movement.CustodyReconciledSlot {
		return errors.New("leg simulation precedes finalized custody")
	}
	if !sameJSON(r.Movement.ExecutionPlan, p.Preparation.ExecutionPlan) {
		return errors.New("cross-mint prepared leg changed immutable execution plan")
	}
	return validateCrossMintLeg(r, p.ExpectedEffect, p.BalanceAnchors)
}

func validateCrossMintLeg(r CrossMintLegRequest, e CrossMintExpectedEffect, a CrossMintBalanceAnchors) error {
	m := r.Movement
	allowed := m.Phase == CrossMintSourceReserve && r.Leg == LegWithdraw && r.Purpose == PurposeOptimizeYield || m.Phase == CrossMintSourceIdle && (r.Leg == LegSwap && r.Purpose == PurposeOptimizeYield || r.Leg == LegDeposit && r.Purpose == PurposeRecoverSource) || m.Phase == CrossMintTargetIdle && r.Leg == LegDeposit && (r.Purpose == PurposeOptimizeYield || r.Purpose == PurposeFallbackTarget)
	if !allowed || m.TerminalOutcome != nil {
		return ErrNotClaimable
	}
	if err := validateCrossMintAnchors(a); err != nil {
		return err
	}
	if (e.Debit == nil) != (a.Debit == nil) || (e.CreditMint == nil) != (a.Credit == nil) || (e.CreditMint == nil) != (e.CreditTokenAccount == nil) || (e.CreditMint == nil) != (e.MinimumCreditAmountRaw == nil) {
		return errors.New("signed effect and anchor account sets differ")
	}
	if e.Debit != nil && (!validCrossMintDelta(e.Debit) || a.Debit.Mint != e.Debit.Mint || a.Debit.TokenAccount != e.Debit.TokenAccount || a.Debit.AmountRaw < e.Debit.AmountRaw) {
		return errors.New("debit anchor does not identify and fund signed debit")
	}
	if e.CreditMint != nil && (*e.CreditMint == "" || *e.CreditTokenAccount == "" || *e.MinimumCreditAmountRaw <= 0 || a.Credit.Mint != *e.CreditMint || a.Credit.TokenAccount != *e.CreditTokenAccount) {
		return errors.New("credit expectation does not match its account anchor")
	}
	if r.Leg == LegWithdraw {
		if e.Debit != nil || e.CreditMint == nil || *e.CreditMint != m.SourceMint || a.Position == nil || a.Position.Reserve != m.SourceReserve || !a.Position.ObligationExists || a.Position.CollateralRaw <= 1 {
			return errors.New("withdrawal lacks source position and source credit contract")
		}
	} else {
		if e.Debit == nil || e.Debit.Mint != m.CustodyMint || e.Debit.TokenAccount != m.CustodyAccount || e.Debit.AmountRaw != m.CustodyAmountRaw || m.CustodyObservedBalanceRaw == nil || a.Debit.AmountRaw != *m.CustodyObservedBalanceRaw || m.CustodyReconciledSlot == nil {
			return errors.New("leg does not bind full attributed idle custody and historical aggregate")
		}
		if r.Leg == LegSwap && (e.CreditMint == nil || *e.CreditMint != m.TargetMint || a.Position != nil) {
			return errors.New("swap must credit target mint without a position anchor")
		}
		if r.Leg == LegDeposit {
			reserve := m.ActiveTargetReserve
			if r.Purpose == PurposeRecoverSource {
				reserve = m.SourceReserve
				if m.CustodyMint != m.SourceMint {
					return errors.New("source recovery cannot deposit target custody")
				}
			}
			if e.CreditMint != nil || a.Position == nil || a.Position.Reserve != reserve {
				return errors.New("deposit lacks destination position contract")
			}
		}
	}
	return nil
}
func validCrossMintDelta(a *CrossMintTokenAmount) bool {
	return a != nil && a.Mint != "" && a.TokenAccount != "" && a.AmountRaw > 0
}
func validateCrossMintAnchors(a CrossMintBalanceAnchors) error {
	for _, t := range []*CrossMintTokenAmount{a.Debit, a.Credit} {
		if t != nil && (t.Mint == "" || t.TokenAccount == "" || t.AmountRaw < 0) {
			return errors.New("invalid token balance anchor")
		}
	}
	if a.Debit != nil && a.Credit != nil && a.Debit.TokenAccount == a.Credit.TokenAccount {
		return errors.New("duplicate token account anchor")
	}
	if p := a.Position; p != nil && (p.Reserve == "" || p.Market == "" || p.Obligation == "" || p.CollateralRaw < 0 || !p.ObligationExists && p.CollateralRaw != 0 || p.MinimumDepositAmountRaw != nil && *p.MinimumDepositAmountRaw <= 0) {
		return errors.New("invalid finalized position anchor")
	}
	return nil
}

type crossMintNextCustody struct {
	mint, account   string
	amount          int64
	observed        *int64
	outcome, reason *string
	evidence        json.RawMessage
	terminalSlot    *int64
}

// crossMintReceiptTransition is source parity with movement.rs. It receives
// exact finalized transaction deltas AND finalized position/account readbacks.
func crossMintReceiptTransition(m CrossMintMovement, leg, purpose string, expected CrossMintExpectedEffect, pre CrossMintBalanceAnchors, actual CrossMintEffect, post CrossMintBalanceAnchors, slot int64) (crossMintNextCustody, error) {
	var next crossMintNextCustody
	if slot <= 0 || m.CustodyReconciledSlot != nil && slot < *m.CustodyReconciledSlot {
		return next, errors.New("receipt precedes custody or lacks finalized slot")
	}
	if err := validateCrossMintLeg(CrossMintLegRequest{Movement: m, Leg: leg, Purpose: purpose}, expected, pre); err != nil {
		return next, err
	}
	if err := validateCrossMintAnchors(post); err != nil {
		return next, err
	}
	if (expected.Debit == nil) != (actual.Debit == nil) || (expected.CreditMint == nil) != (actual.Credit == nil) || (actual.Debit == nil) != (post.Debit == nil) || (actual.Credit == nil) != (post.Credit == nil) || (pre.Position == nil) != (post.Position == nil) {
		return next, errors.New("finalized effect/anchor account sets differ")
	}
	if d := actual.Debit; d != nil {
		if !validCrossMintDelta(d) || d.Mint != expected.Debit.Mint || d.TokenAccount != expected.Debit.TokenAccount || d.AmountRaw > expected.Debit.AmountRaw || leg != LegDeposit && d.AmountRaw != expected.Debit.AmountRaw || post.Debit.Mint != d.Mint || post.Debit.TokenAccount != d.TokenAccount || pre.Debit.AmountRaw < d.AmountRaw || pre.Debit.AmountRaw-d.AmountRaw != post.Debit.AmountRaw {
			return next, errors.New("finalized debit differs from signed custody or exact balance delta")
		}
	}
	if c := actual.Credit; c != nil {
		if !validCrossMintDelta(c) || c.Mint != *expected.CreditMint || c.TokenAccount != *expected.CreditTokenAccount || c.AmountRaw < *expected.MinimumCreditAmountRaw || post.Credit.Mint != c.Mint || post.Credit.TokenAccount != c.TokenAccount || pre.Credit.AmountRaw > math.MaxInt64-c.AmountRaw || pre.Credit.AmountRaw+c.AmountRaw != post.Credit.AmountRaw {
			return next, errors.New("finalized credit differs from minimum or exact balance delta")
		}
	}
	if pre.Position != nil {
		before, after := pre.Position, post.Position
		if before.Reserve != after.Reserve || before.Market != after.Market || before.Obligation != after.Obligation {
			return next, errors.New("finalized Kamino position identity changed")
		}
		if leg == LegWithdraw {
			if before.CollateralRaw <= after.CollateralRaw || after.CollateralRaw != 0 && (!after.ObligationExists || after.CollateralRaw != 1) {
				return next, errors.New("withdrawal did not remove source position except recovery anchor")
			}
		} else if !after.ObligationExists || after.CollateralRaw <= before.CollateralRaw {
			return next, errors.New("deposit did not increase finalized collateral")
		}
	}
	if leg == LegWithdraw || leg == LegSwap {
		next.mint, next.account, next.amount = actual.Credit.Mint, actual.Credit.TokenAccount, actual.Credit.AmountRaw
		next.observed = &post.Credit.AmountRaw
		return next, nil
	}
	next.mint, next.amount = m.CustodyMint, m.CustodyAmountRaw-actual.Debit.AmountRaw
	reserve, outcome := m.ActiveTargetReserve, "completed_target"
	if purpose == PurposeRecoverSource {
		reserve, outcome = m.SourceReserve, "recovered_source"
	}
	next.outcome = &outcome
	if next.amount == 0 {
		next.account = reserve
		return next, nil
	}
	if post.Position == nil || post.Position.MinimumDepositAmountRaw == nil || next.amount >= *post.Position.MinimumDepositAmountRaw {
		return next, errors.New("partial deposit lacks unmintable finalized dust proof")
	}
	next.account, next.observed = m.CustodyAccount, &post.Debit.AmountRaw
	reason := "kamino_unmintable_rounding_dust"
	next.reason, next.terminalSlot = &reason, &slot
	next.evidence, _ = json.Marshal(map[string]any{"kind": reason, "mint": m.CustodyMint, "tokenAccount": m.CustodyAccount, "requestedAmountRaw": m.CustodyAmountRaw, "depositedAmountRaw": actual.Debit.AmountRaw, "residualAmountRaw": next.amount, "minimumDepositAmountRaw": *post.Position.MinimumDepositAmountRaw, "finalizedPostBalanceRaw": post.Debit.AmountRaw, "reserve": reserve})
	return next, nil
}

func crossMintSemantic(decision int64, leg string, generation int64) string {
	return fmt.Sprintf("cross-mint:%d:%s:%d", decision, leg, generation)
}
