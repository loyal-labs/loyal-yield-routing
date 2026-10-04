package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
)

// The root must configure this revalidator with the exact continuation owner
// used by the D controller/runtime. Request ownership is passed unchanged;
// C verifies it against its configured owner and the actual durable lease.
type retailCrossMintAdapters struct{ revalidator *fleet.Revalidator }

var (
	_ fleetexec.CrossMintLegFactory        = (*retailCrossMintAdapters)(nil)
	_ fleetexec.CrossMintActivationSource  = (*retailCrossMintAdapters)(nil)
	_ fleetexec.CrossMintFirstSendVerifier = (*retailCrossMintAdapters)(nil)
)

func composeRetailCrossMint(ctx context.Context, cfg retailConfig, owner string, store *fleetexec.Store, revalidator *fleet.Revalidator, adapter *fleetexec.RPCAdapter, market *fleet.MarketEvidenceStore) (*fleetexec.CrossMintRuntime, error) {
	if market == nil {
		return nil, errors.New("cross-mint fallback requires the actual planner market evidence")
	}
	capabilities, err := newRetailCrossMintAdapters(revalidator)
	if err != nil {
		return nil, err
	}
	controller, err := fleetexec.NewCrossMintController(store, capabilities, fleetexec.DelegateSigner{FeePayer: cfg.delegate}, adapter, "mainnet-beta", owner, 30*time.Second, cfg.crossMintEnabled)
	if err != nil {
		return nil, err
	}
	controller.SetMarketEpochSource(market)
	runtime, err := fleetexec.NewCrossMintRuntime(ctx, fleetexec.Config{Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, SlotDuration: cfg.slotDuration}, store, controller, adapter, capabilities)
	if err != nil {
		return nil, err
	}
	runtime.SetActivationSource(capabilities)
	return runtime, nil
}

func newRetailCrossMintAdapters(revalidator *fleet.Revalidator) (*retailCrossMintAdapters, error) {
	if revalidator == nil {
		return nil, errors.New("cross-mint adapters require concrete source revalidator")
	}
	return &retailCrossMintAdapters{revalidator: revalidator}, nil
}

func (a *retailCrossMintAdapters) PrepareCrossMintLeg(ctx context.Context, q fleetexec.CrossMintLegRequest) (fleetexec.CrossMintPreparedLeg, error) {
	prepared, err := a.revalidator.PrepareCrossMintLeg(ctx, retailCrossMintLegRequest(q))
	if err != nil {
		return fleetexec.CrossMintPreparedLeg{}, retailCrossMintLegError(ctx, q.Leg, err)
	}
	return retailCrossMintPreparedLeg(prepared)
}

func retailCrossMintLegError(ctx context.Context, leg string, err error) error {
	// Only the source's validated economic shortfall permits recovery.
	// Timeouts, cancellation, policy and account failures retain custody.
	if leg == fleetexec.LegSwap && errors.Is(err, fleet.ErrCrossMintQuoteUnavailable) && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("source swap economics: %w", fleetexec.ErrCrossMintSwapUnavailable)
	}
	return err
}

func (a *retailCrossMintAdapters) PrepareCrossMintActivation(ctx context.Context, cluster string) (*fleetexec.CrossMintActivationAdmission, error) {
	prepared, _, err := a.revalidator.PrepareNextCrossMintActivation(ctx, cluster)
	if err != nil || prepared == nil {
		return nil, err
	}
	return retailCrossMintActivationAdmission(*prepared, time.Now())
}

func (a *retailCrossMintAdapters) VerifyCrossMintFirstSend(ctx context.Context, q fleetexec.CrossMintFirstSendRequest) error {
	request, err := retailCrossMintFirstSendRequest(q)
	if err != nil {
		return err
	}
	return a.revalidator.ValidateCrossMintFirstSend(ctx, request)
}

func retailCrossMintMovement(m fleetexec.CrossMintMovement) fleet.CrossMintPreparationMovement {
	return fleet.CrossMintPreparationMovement{
		DecisionID: m.DecisionID, OpportunityID: m.OpportunityID, OptimizerEpochID: m.OptimizerEpochID, VaultID: m.VaultID,
		Cluster: m.Cluster, VaultPubkey: m.VaultPubkey, SourceSnapshotID: m.SourceSnapshotID,
		SourceReserve: m.SourceReserve, IntendedTargetReserve: m.IntendedTargetReserve, ActiveTargetReserve: m.ActiveTargetReserve,
		SourceMint: m.SourceMint, TargetMint: m.TargetMint, PlannedAmountRaw: m.PlannedAmountRaw,
		ExecutionPlan: bytes.Clone(m.ExecutionPlan), PreflightCertification: bytes.Clone(m.PreflightCertification),
		CustodyMint: m.CustodyMint, CustodyAccount: m.CustodyAccount, CustodyAmountRaw: m.CustodyAmountRaw,
		CustodyObservedBalanceRaw: m.CustodyObservedBalanceRaw, CustodyReconciledSlot: m.CustodyReconciledSlot,
		CustodyVersion: m.CustodyVersion, Phase: string(m.Phase), TerminalOutcome: m.TerminalOutcome,
	}
}

func retailCrossMintLegRequest(q fleetexec.CrossMintLegRequest) fleet.CrossMintPreparationRequest {
	return fleet.CrossMintPreparationRequest{
		Movement: retailCrossMintMovement(q.Movement), Leg: q.Leg, Purpose: q.Purpose,
		Generation: q.Generation, RemainingFeeLamports: q.RemainingFeeLamports,
		ContinuationOwner: q.ContinuationOwner, ContinuationFencingToken: q.ContinuationFencingToken,
		ControlGeneration: q.ControlGeneration, ExpiresAt: q.ExpiresAt,
	}
}

func retailCrossMintALTAddresses(records []fleet.ALTManifestAddress) []fleetexec.CrossMintALTAddress {
	if records == nil {
		return nil
	}
	out := make([]fleetexec.CrossMintALTAddress, len(records))
	for i, r := range records {
		out[i] = fleetexec.CrossMintALTAddress{Address: r.Address, SemanticClass: r.SemanticClass, AccountRole: r.AccountRole, Ordinal: r.Ordinal, Writable: r.Writable}
	}
	return out
}

func retailCrossMintPreparedLeg(p fleet.CrossMintLegPreparation) (fleetexec.CrossMintPreparedLeg, error) {
	out := fleetexec.CrossMintPreparedLeg{
		Preparation: p.Preparation, LastValidBlockHeight: p.LastValidBlockHeight, PolicyAccount: p.PolicyAccount,
		ConflictKeys: p.ConflictKeys, SelectedALTs: p.SelectedALTs, AltSelectionFingerprint: p.AltSelectionFingerprint,
		ExternalALTs: p.ExternalALTs,
		WaitingALT:   p.WaitingALT, MissingAddresses: p.MissingAddresses,
		SharedAddresses: retailCrossMintALTAddresses(p.SharedAddresses), VaultAddresses: retailCrossMintALTAddresses(p.VaultAddresses),
	}
	if p.WaitingALT {
		// Waiting demand has no simulated executable effect or custody proof.
		return out, nil
	}
	if p.Preparation.Transaction.FeeLamports == 0 || p.Preparation.Transaction.FeeLamports > math.MaxInt64 || p.LastValidBlockHeight <= 0 {
		return fleetexec.CrossMintPreparedLeg{}, errors.New("cross-mint preparation fee or height is outside SQL range")
	}
	if err := retailCrossMintDecode(p.ExpectedEffect, &out.ExpectedEffect); err != nil {
		return fleetexec.CrossMintPreparedLeg{}, fmt.Errorf("source effect contract: %w", err)
	}
	if err := retailCrossMintDecode(p.BalanceAnchors, &out.BalanceAnchors); err != nil {
		return fleetexec.CrossMintPreparedLeg{}, fmt.Errorf("source balance contract: %w", err)
	}
	if err := retailCrossMintContracts(out.ExpectedEffect, out.BalanceAnchors); err != nil {
		return fleetexec.CrossMintPreparedLeg{}, err
	}
	return out, nil
}

func retailCrossMintDecode(raw []byte, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("expected complete JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing data in source JSON object")
	}
	return nil
}

// Decode the receipt contract without synthesizing absent accounts or amounts.
// D separately checks leg-specific meaning against its locked movement.
func retailCrossMintContracts(e fleetexec.CrossMintExpectedEffect, a fleetexec.CrossMintBalanceAnchors) error {
	if (e.Debit == nil) != (a.Debit == nil) || (e.CreditMint == nil) != (e.CreditTokenAccount == nil) || (e.CreditMint == nil) != (e.MinimumCreditAmountRaw == nil) || (e.CreditMint == nil) != (a.Credit == nil) || e.Debit == nil && e.CreditMint == nil {
		return errors.New("source effect and balance accounts are incomplete")
	}
	for _, anchor := range []*fleetexec.CrossMintTokenAmount{a.Debit, a.Credit} {
		if anchor != nil && (anchor.Mint == "" || anchor.TokenAccount == "" || anchor.AmountRaw < 0) {
			return errors.New("source token anchor is invalid")
		}
	}
	if e.Debit != nil && (e.Debit.Mint == "" || e.Debit.TokenAccount == "" || e.Debit.AmountRaw <= 0 || e.Debit.Mint != a.Debit.Mint || e.Debit.TokenAccount != a.Debit.TokenAccount || e.Debit.AmountRaw > a.Debit.AmountRaw) {
		return errors.New("source debit is not covered by its exact balance anchor")
	}
	if e.CreditMint != nil && (*e.CreditMint == "" || *e.CreditTokenAccount == "" || *e.MinimumCreditAmountRaw <= 0 || *e.CreditMint != a.Credit.Mint || *e.CreditTokenAccount != a.Credit.TokenAccount) {
		return errors.New("source credit is not bound to its balance anchor")
	}
	if a.Debit != nil && a.Credit != nil && a.Debit.TokenAccount == a.Credit.TokenAccount {
		return errors.New("source token anchors duplicate an account")
	}
	if p := a.Position; p != nil && (p.Reserve == "" || p.Market == "" || p.Obligation == "" || p.CollateralRaw < 0 || !p.ObligationExists && p.CollateralRaw != 0 || p.MinimumDepositAmountRaw != nil && *p.MinimumDepositAmountRaw <= 0) {
		return errors.New("source position anchor is invalid")
	}
	return nil
}

func retailCrossMintActivationAdmission(p fleet.CrossMintActivationPreparation, now time.Time) (*fleetexec.CrossMintActivationAdmission, error) {
	l, capacity := p.Lease, p.Capacity
	fee := p.InitialWithdrawalPreparation.Preparation.Transaction.FeeLamports
	if p.WaitingALT || p.InitialWithdrawalPreparation.WaitingALT || p.ControlGeneration < 0 || l.LiquidityAmountRaw == 0 || l.LiquidityAmountRaw > math.MaxInt64 || fee == 0 || fee > math.MaxInt64 || fee > uint64(l.FeeCapLamports) || l.FeeCapLamports <= 0 || !l.ExpiresAt.After(now.Add(5*time.Second)) || p.ObservedAt.IsZero() || p.ObservedAt.After(now) || now.Sub(p.ObservedAt) > 15*time.Second || capacity.Cluster != l.Cluster || capacity.TargetReserve != l.TargetReserve || capacity.LiquidityMint != l.TargetLiquidityMint || capacity.ObservedSlot != p.ObservedSlot || capacity.ObservedSlot <= 0 || capacity.ObservedSupplyUSDMicros != p.TargetObservedSupplyUSDMicros || capacity.ObservedSupplyUSDMicros < 0 || capacity.MaximumInflightUSDMicros <= 0 || capacity.TelemetryVersion < 0 {
		return nil, errors.New("source activation lacks live exact capacity and independently compiled withdrawal")
	}
	certificate, err := json.Marshal(p.Certificate)
	if err != nil {
		return nil, err
	}
	m := fleet.CrossMintPreparationMovement{Cluster: l.Cluster, VaultPubkey: l.VaultPubkey, SourceMint: l.SourceLiquidityMint, TargetMint: l.TargetLiquidityMint, IntendedTargetReserve: l.TargetReserve, PlannedAmountRaw: int64(l.LiquidityAmountRaw), ExecutionPlan: l.ExecutionPlan, PreflightCertification: certificate}
	if _, err = fleet.ValidateCrossMintPreflightCertificate(m, now); err != nil {
		return nil, err
	}
	return &fleetexec.CrossMintActivationAdmission{Lease: l, Activation: fleetexec.CrossMintActivation{
		SourceControlGeneration:            p.ControlGeneration,
		Capacity:                           fleetexec.CrossMintCapacityProjection{Cluster: capacity.Cluster, TargetReserve: capacity.TargetReserve, LiquidityMint: capacity.LiquidityMint, ObservedSupplyUSDMicros: capacity.ObservedSupplyUSDMicros, ObservedSlot: capacity.ObservedSlot, MaximumInflightUSDMicros: capacity.MaximumInflightUSDMicros, TelemetryVersion: capacity.TelemetryVersion},
		InitialWithdrawCompiledFeeLamports: int64(fee), PreflightCertification: certificate,
	}}, nil
}

func retailCrossMintFirstSendRequest(q fleetexec.CrossMintFirstSendRequest) (fleet.CrossMintFirstSendRequest, error) {
	s := q.Submission
	if s.DecisionID == nil || *s.DecisionID != q.Movement.DecisionID || s.OpportunityID != q.Movement.OpportunityID || s.Cluster != q.Movement.Cluster {
		return fleet.CrossMintFirstSendRequest{}, errors.New("first-send journal identity differs from movement")
	}
	return fleet.CrossMintFirstSendRequest{
		Movement: retailCrossMintMovement(q.Movement), Leg: s.MovementLeg, Purpose: s.LegPurpose,
		PolicyAccount: q.PolicyAccount, MinimumSlot: q.MinimumSlot,
		SignedWire: bytes.Clone(s.SignedTransaction), ExpectedWireSHA256: q.ExpectedWireSHA256, ExpectedMessageSHA256: s.MessageHash,
		Signature: s.Signature, RecentBlockhash: s.RecentBlockhash, LastValidBlockHeight: s.LastValidBlockHeight,
		SelectedALTs: q.SelectedALTs,
		ExternalALTs: q.ExternalALTs,
	}, nil
}
