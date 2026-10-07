package fleetexec

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
)

// The root must configure this revalidator with the exact continuation owner
// used by the D controller/runtime. Request ownership is passed unchanged;
// C verifies it against its configured owner and the actual durable lease.
type RevalidatorCrossMint struct{ revalidator *fleet.Revalidator }

var (
	_ CrossMintLegFactory        = (*RevalidatorCrossMint)(nil)
	_ CrossMintActivationSource  = (*RevalidatorCrossMint)(nil)
	_ CrossMintFirstSendVerifier = (*RevalidatorCrossMint)(nil)
)

// NewRevalidatorCrossMint binds the cross-mint runtime to Go route preparation.
func NewRevalidatorCrossMint(revalidator *fleet.Revalidator) (*RevalidatorCrossMint, error) {
	if revalidator == nil {
		return nil, errors.New("cross-mint adapters require concrete source revalidator")
	}
	return &RevalidatorCrossMint{revalidator: revalidator}, nil
}

func (a *RevalidatorCrossMint) PrepareCrossMintLeg(ctx context.Context, q CrossMintLegRequest) (CrossMintPreparedLeg, error) {
	prepared, err := a.revalidator.PrepareCrossMintLeg(ctx, revalidatorCrossMintLegRequest(q))
	if err != nil {
		return CrossMintPreparedLeg{}, revalidatorCrossMintLegError(ctx, q.Leg, err)
	}
	return revalidatorCrossMintPreparedLeg(prepared)
}

func revalidatorCrossMintLegError(ctx context.Context, leg string, err error) error {
	// Only the source's validated economic shortfall permits recovery.
	// Timeouts, cancellation, policy and account failures retain custody.
	if leg == LegSwap && errors.Is(err, fleet.ErrCrossMintQuoteUnavailable) && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("source swap economics: %w", ErrCrossMintSwapUnavailable)
	}
	return err
}

func (a *RevalidatorCrossMint) PrepareCrossMintActivation(ctx context.Context, cluster string) (*CrossMintActivationAdmission, error) {
	prepared, _, err := a.revalidator.PrepareNextCrossMintActivation(ctx, cluster)
	if err != nil || prepared == nil {
		return nil, err
	}
	return revalidatorCrossMintActivationAdmission(*prepared, time.Now())
}

func (a *RevalidatorCrossMint) VerifyCrossMintFirstSend(ctx context.Context, q CrossMintFirstSendRequest) error {
	request, err := revalidatorCrossMintFirstSendRequest(q)
	if err != nil {
		return err
	}
	return a.revalidator.ValidateCrossMintFirstSend(ctx, request)
}

func revalidatorCrossMintMovement(m CrossMintMovement) fleet.CrossMintPreparationMovement {
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

func revalidatorCrossMintLegRequest(q CrossMintLegRequest) fleet.CrossMintPreparationRequest {
	return fleet.CrossMintPreparationRequest{
		Movement: revalidatorCrossMintMovement(q.Movement), Leg: q.Leg, Purpose: q.Purpose,
		Generation: q.Generation, RemainingFeeLamports: q.RemainingFeeLamports,
		ContinuationOwner: q.ContinuationOwner, ContinuationFencingToken: q.ContinuationFencingToken,
		ControlGeneration: q.ControlGeneration, ExpiresAt: q.ExpiresAt,
	}
}

func revalidatorCrossMintALTAddresses(records []fleet.ALTManifestAddress) []CrossMintALTAddress {
	if records == nil {
		return nil
	}
	out := make([]CrossMintALTAddress, len(records))
	for i, r := range records {
		out[i] = CrossMintALTAddress{Address: r.Address, SemanticClass: r.SemanticClass, AccountRole: r.AccountRole, Ordinal: r.Ordinal, Writable: r.Writable}
	}
	return out
}

func revalidatorCrossMintPreparedLeg(p fleet.CrossMintLegPreparation) (CrossMintPreparedLeg, error) {
	out := CrossMintPreparedLeg{
		Preparation: p.Preparation, LastValidBlockHeight: p.LastValidBlockHeight, PolicyAccount: p.PolicyAccount,
		ConflictKeys: p.ConflictKeys, SelectedALTs: p.SelectedALTs, AltSelectionFingerprint: p.AltSelectionFingerprint,
		ExternalALTs: p.ExternalALTs,
		WaitingALT:   p.WaitingALT, MissingAddresses: p.MissingAddresses,
		SharedAddresses: revalidatorCrossMintALTAddresses(p.SharedAddresses), VaultAddresses: revalidatorCrossMintALTAddresses(p.VaultAddresses),
	}
	if p.WaitingALT {
		// Waiting demand has no simulated executable effect or custody proof.
		return out, nil
	}
	if p.Preparation.Transaction.FeeLamports == 0 || p.Preparation.Transaction.FeeLamports > math.MaxInt64 || p.LastValidBlockHeight <= 0 {
		return CrossMintPreparedLeg{}, errors.New("cross-mint preparation fee or height is outside SQL range")
	}
	if err := revalidatorCrossMintDecode(p.ExpectedEffect, &out.ExpectedEffect); err != nil {
		return CrossMintPreparedLeg{}, fmt.Errorf("source effect contract: %w", err)
	}
	if err := revalidatorCrossMintDecode(p.BalanceAnchors, &out.BalanceAnchors); err != nil {
		return CrossMintPreparedLeg{}, fmt.Errorf("source balance contract: %w", err)
	}
	if err := revalidatorCrossMintContracts(out.ExpectedEffect, out.BalanceAnchors); err != nil {
		return CrossMintPreparedLeg{}, err
	}
	return out, nil
}

func revalidatorCrossMintDecode(raw []byte, target any) error {
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
func revalidatorCrossMintContracts(e CrossMintExpectedEffect, a CrossMintBalanceAnchors) error {
	if (e.Debit == nil) != (a.Debit == nil) || (e.CreditMint == nil) != (e.CreditTokenAccount == nil) || (e.CreditMint == nil) != (e.MinimumCreditAmountRaw == nil) || (e.CreditMint == nil) != (a.Credit == nil) || e.Debit == nil && e.CreditMint == nil {
		return errors.New("source effect and balance accounts are incomplete")
	}
	for _, anchor := range []*CrossMintTokenAmount{a.Debit, a.Credit} {
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

func revalidatorCrossMintActivationAdmission(p fleet.CrossMintActivationPreparation, now time.Time) (*CrossMintActivationAdmission, error) {
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
	return &CrossMintActivationAdmission{Lease: l, Activation: CrossMintActivation{
		SourceControlGeneration:            p.ControlGeneration,
		Capacity:                           CrossMintCapacityProjection{Cluster: capacity.Cluster, TargetReserve: capacity.TargetReserve, LiquidityMint: capacity.LiquidityMint, ObservedSupplyUSDMicros: capacity.ObservedSupplyUSDMicros, ObservedSlot: capacity.ObservedSlot, MaximumInflightUSDMicros: capacity.MaximumInflightUSDMicros, TelemetryVersion: capacity.TelemetryVersion},
		InitialWithdrawCompiledFeeLamports: int64(fee), PreflightCertification: certificate,
	}}, nil
}

func revalidatorCrossMintFirstSendRequest(q CrossMintFirstSendRequest) (fleet.CrossMintFirstSendRequest, error) {
	s := q.Submission
	if s.DecisionID == nil || *s.DecisionID != q.Movement.DecisionID || s.OpportunityID != q.Movement.OpportunityID || s.Cluster != q.Movement.Cluster {
		return fleet.CrossMintFirstSendRequest{}, errors.New("first-send journal identity differs from movement")
	}
	return fleet.CrossMintFirstSendRequest{
		Movement: revalidatorCrossMintMovement(q.Movement), Leg: s.MovementLeg, Purpose: s.LegPurpose,
		PolicyAccount: q.PolicyAccount, MinimumSlot: q.MinimumSlot,
		SignedWire: bytes.Clone(s.SignedTransaction), ExpectedWireSHA256: q.ExpectedWireSHA256, ExpectedMessageSHA256: s.MessageHash,
		Signature: s.Signature, RecentBlockhash: s.RecentBlockhash, LastValidBlockHeight: s.LastValidBlockHeight,
		SelectedALTs: q.SelectedALTs,
		ExternalALTs: q.ExternalALTs,
	}, nil
}
