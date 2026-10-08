package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// RequireCrossMintSchema is a startup prerequisite for the composition root,
// in addition to RequireSchema. It checks retained tables and custody columns;
// this feature does not install migrations or create replacement contracts.
func (s *Store) RequireCrossMintSchema(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("cross-mint store requires caller-owned pool")
	}
	if err := s.RequireSchema(ctx); err != nil {
		return err
	}
	if err := db.RequireTables(ctx, s.pool,
		"loyal_yield.rebalance_decisions", "loyal_yield.rebalance_opportunities",
		"loyal_yield.managed_vaults", "loyal_yield.route_policies",
		"loyal_yield.cross_mint_movement_controls", "loyal_yield.cross_mint_swap_policies",
		"loyal_yield.cross_mint_vault_opt_ins", "loyal_yield.cross_mint_no_effect_receipts",
		"loyal_yield.lookup_table_families", "loyal_yield.route_lookup_tables", "loyal_yield.lookup_table_addresses",
		"loyal_yield.lookup_table_manifests", "loyal_yield.lookup_table_manifest_addresses",
		"loyal_yield.lookup_table_shared_market_catalog_heads", "loyal_yield.lookup_table_shared_market_catalog_revisions",
		"loyal_yield.lookup_table_provisioning_requests", "loyal_yield.lookup_table_provisioning_request_addresses"); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `SELECT d.movement_route,d.custody_mint,d.custody_amount_raw,d.custody_account,d.custody_observed_balance_raw,d.custody_reconciled_slot,d.custody_version,d.cross_mint_activation_control_generation,d.cross_mint_preflight_certification,d.continuation_available_at,d.continuation_lease_owner,d.continuation_lease_expires_at,d.continuation_fencing_token,d.continuation_control_generation,d.active_target_reserve,d.terminal_outcome,d.terminal_evidence,d.terminal_observed_slot,s.expected_effect,s.expected_balance_anchors,s.reconciled_effect,s.reconciled_balance_anchors,s.required_commitment,s.finalized_slot,s.finalized_at,s.policy_account,r.sealed_at,a.semantic_class,a.account_role,a.is_writable,n.transaction_signature,n.movement_leg,n.leg_generation FROM loyal_yield.rebalance_decisions d CROSS JOIN loyal_yield.signed_route_submissions s CROSS JOIN loyal_yield.lookup_table_provisioning_requests r CROSS JOIN loyal_yield.lookup_table_provisioning_request_addresses a CROSS JOIN loyal_yield.cross_mint_no_effect_receipts n WHERE false`)
	if err != nil {
		return fmt.Errorf("retained cross-mint schema contract: %w", err)
	}
	return nil
}

func validateCrossMintWire(w WireIdentity, p CrossMintPreparedLeg) (string, error) {
	if len(w.SignedTransaction) == 0 || len(w.SignedTransaction) > SolanaPacketLimit {
		return "", errors.New("signed leg exceeds packet bound")
	}
	hash := sha256.Sum256(w.SignedTransaction)
	if hex.EncodeToString(hash[:]) != w.SignedTransactionHash {
		return "", errors.New("signed leg hash differs from exact bytes")
	}
	tx, err := sdk.TransactionFromBytes(w.SignedTransaction)
	if err != nil {
		return "", err
	}
	raw, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(raw, w.SignedTransaction) {
		return "", errors.New("signed leg is not canonical wire")
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil || !bytes.Equal(message, p.Preparation.Transaction.Message) || len(tx.Message.AccountKeys) == 0 {
		return "", errors.New("signed leg message differs from prepared bytes")
	}
	payer := tx.Message.AccountKeys[0].String()
	if err := verifyDurableWire(SubmissionRecord{SignedTransaction: w.SignedTransaction, FeePayer: payer, Signature: w.TransactionSignature, RecentBlockhash: w.RecentBlockhash, MessageHash: w.MessageHash}); err != nil {
		return "", err
	}
	if err := verifyCrossMintPreparedWritables(tx, p); err != nil {
		return "", err
	}
	fingerprint, err := crossMintALTSelectionFingerprint(p)
	if err != nil {
		return "", err
	}
	if fingerprint != p.AltSelectionFingerprint {
		return "", errors.New("cross-mint ALT selection fingerprint differs from compiled proof")
	}
	return payer, nil
}

type crossMintQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type CrossMintReconciliation struct {
	FinalizedSlot  int64
	Effect         CrossMintEffect
	BalanceAnchors CrossMintBalanceAnchors
}

// ReconcileCrossMintLeg accepts the exact receipt and finalized readbacks from
// the family proof owner. It cannot turn a confirmation or balance guess into
// custody: the durable attempt must already carry the same finalized slot.
func (s *Store) ReconcileCrossMintLeg(ctx context.Context, l SubmissionLease, input CrossMintReconciliation) (CrossMintMovement, error) {
	var result CrossMintMovement
	if input.FinalizedSlot <= 0 || l.Submission.DecisionID == nil || l.Submission.MovementLeg == LegRoute {
		return result, ErrNotClaimable
	}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var decision, confirmed int64
		var leg, purpose, signature string
		var expectedJSON, anchorsJSON json.RawMessage
		query := `SELECT decision_id,confirmed_slot,movement_leg,leg_purpose,transaction_signature,expected_effect,expected_balance_anchors
FROM loyal_yield.signed_route_submissions WHERE id=$1 AND decision_id IS NOT NULL AND movement_leg<>'route'
AND required_commitment='finalized' AND submission_state='reconciliation_pending' AND reconciled_effect IS NULL
AND finalized_slot=$4 AND finalized_at IS NOT NULL AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3
AND confirmation_lease_expires_at>clock_timestamp() FOR UPDATE`
		err := tx.QueryRow(ctx, query, l.Submission.ID, l.Owner, l.FencingToken, input.FinalizedSlot).Scan(&decision, &confirmed, &leg, &purpose, &signature, &expectedJSON, &anchorsJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		if decision != *l.Submission.DecisionID || signature != l.Submission.Signature || leg != l.Submission.MovementLeg || input.FinalizedSlot < confirmed {
			return errors.New("finalized receipt differs from claimed durable attempt")
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM loyal_yield.rebalance_decisions WHERE id=$1 FOR UPDATE`, decision); err != nil {
			return err
		}
		m, err := readCrossMintMovement(ctx, tx, decision)
		if err != nil {
			return err
		}
		var expected CrossMintExpectedEffect
		var pre CrossMintBalanceAnchors
		if err = json.Unmarshal(expectedJSON, &expected); err != nil {
			return err
		}
		if err = json.Unmarshal(anchorsJSON, &pre); err != nil {
			return err
		}
		next, err := crossMintReceiptTransition(m, leg, purpose, expected, pre, input.Effect, input.BalanceAnchors, input.FinalizedSlot)
		if err != nil {
			return err
		}
		var id int64
		if err = tx.QueryRow(ctx, crossMintAdvanceCustodySQL, decision, next.mint, next.amount, next.account, next.observed, input.FinalizedSlot, next.outcome, signature, m.CustodyVersion, nullableJSON(next.evidence), next.reason, next.terminalSlot).Scan(&id); err != nil {
			return err
		}
		effect, _ := json.Marshal(input.Effect)
		anchors, _ := json.Marshal(input.BalanceAnchors)
		debitMint, debitAccount, debitAmount := crossMintDeltaSQL(input.Effect.Debit)
		creditMint, creditAccount, creditAmount := crossMintDeltaSQL(input.Effect.Credit)
		err = tx.QueryRow(ctx, crossMintReceiptSQL, l.Submission.ID, l.Owner, l.FencingToken, input.FinalizedSlot, effect, anchors, debitMint, debitAccount, debitAmount, creditMint, creditAccount, creditAmount).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		result, err = readCrossMintMovement(ctx, tx, decision)
		return err
	})
	return result, err
}
func crossMintDeltaSQL(d *CrossMintTokenAmount) (any, any, any) {
	if d == nil {
		return nil, nil, nil
	}
	return d.Mint, d.TokenAccount, d.AmountRaw
}

type CrossMintCapacityProjection struct {
	Cluster, TargetReserve, LiquidityMint                                             string
	ObservedSupplyUSDMicros, ObservedSlot, MaximumInflightUSDMicros, TelemetryVersion int64
}

type CrossMintActivation struct {
	Capacity                           CrossMintCapacityProjection
	InitialWithdrawCompiledFeeLamports int64
	PreflightCertification             json.RawMessage
	SourceControlGeneration            int64
}

// A continuation can be reclaimed under a newer control generation without
// granting the old activation permission to start withdrawing. Cancellation
// rechecks this fact while holding both the control and movement locks.
func (s *Store) crossMintInitialAuthority(ctx context.Context, l CrossMintContinuationLease) (bool, error) {
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT cross_mint_activation_control_generation=$4 FROM loyal_yield.rebalance_decisions WHERE id=$1 AND continuation_lease_owner=$2 AND continuation_fencing_token=$3 AND continuation_control_generation=$4 AND continuation_lease_expires_at>clock_timestamp()`, l.Movement.DecisionID, l.Owner, l.FencingToken, l.ControlGeneration).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStaleOwner
	}
	return allowed, err
}

// CancelUntouchedCrossMint releases only source-reserve custody with no
// signed attempt, under live revoked start authority. Signed bytes cannot be
// revoked by this database flag, even if they have not yet been broadcast.
func (s *Store) CancelUntouchedCrossMint(ctx context.Context, l CrossMintContinuationLease, observedSlot int64) error {
	return s.cancelUntouchedCrossMint(ctx, l, observedSlot, crossMintControlRevoked)
}

type crossMintCancellationAuthority uint8

const (
	crossMintControlRevoked crossMintCancellationAuthority = iota
	crossMintRolloutDisabled
)

// Only the controller's immutable disabled rollout selects the second basis.
// Both bases require untouched custody, zero signed attempts and live fences.
func (s *Store) cancelUntouchedCrossMint(ctx context.Context, l CrossMintContinuationLease, observedSlot int64, authority crossMintCancellationAuthority) error {
	if observedSlot <= 0 {
		return errors.New("untouched cancellation lacks finalized observation slot")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, l.Movement.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, l.Movement.Cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting || g.Generation != l.ControlGeneration {
			return ErrStaleOwner
		}
		m, err := lockCrossMintLease(ctx, tx, l)
		if err != nil {
			return err
		}
		var activation *int64
		var attempts int64
		if err = tx.QueryRow(ctx, `SELECT cross_mint_activation_control_generation,(SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id) FROM loyal_yield.rebalance_decisions d WHERE id=$1`, m.DecisionID).Scan(&activation, &attempts); err != nil {
			return err
		}
		if authority != crossMintControlRevoked && authority != crossMintRolloutDisabled || m.Phase != CrossMintSourceReserve || m.CustodyVersion != 0 || attempts != 0 || authority == crossMintControlRevoked && g.StartNewMovements && activation != nil && *activation == g.Generation {
			return errors.New("cancellation requires untouched custody and revoked initial authority")
		}
		var reserve, mint string
		var reservation, generation int64
		if err = tx.QueryRow(ctx, `SELECT id,target_reserve,liquidity_mint,reservation_generation FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1 AND reservation_state<>'released'`, m.DecisionID).Scan(&reservation, &reserve, &mint, &generation); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `SELECT 1 FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, m.Cluster, reserve, mint); err != nil {
			return err
		}
		var lockedGeneration int64
		if err = tx.QueryRow(ctx, `SELECT reservation_generation FROM loyal_yield.target_capacity_reservations WHERE id=$1 AND target_reserve=$2 AND liquidity_mint=$3 AND reservation_state<>'released' FOR UPDATE`, reservation, reserve, mint).Scan(&lockedGeneration); err != nil {
			return err
		}
		if generation != lockedGeneration {
			return ErrStaleOwner
		}
		reason := "start_authority_revoked_before_withdraw"
		if authority == crossMintRolloutDisabled {
			reason = "rollout_disabled"
		}
		evidence, _ := json.Marshal(map[string]any{"kind": "start_authority_revoked_before_withdraw", "basis": reason, "controlGeneration": g.Generation, "activationControlGeneration": activation})
		updated, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='abandoned',terminal_outcome='cancelled_before_withdraw',terminal_evidence=$4,terminal_reason='start_authority_revoked_before_withdraw',terminal_observed_slot=$5,abandon_reason=$6,continuation_available_at=NULL,continuation_lease_owner=NULL,continuation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND continuation_lease_owner=$2 AND continuation_fencing_token=$3 AND continuation_lease_expires_at>clock_timestamp()`, m.DecisionID, l.Owner, l.FencingToken, evidence, observedSlot, reason)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		updated, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='completed',terminal_reason='cancelled_before_withdraw',updated_at=clock_timestamp() WHERE id=$1 AND decision_id=$2 AND opportunity_state='decision_created'`, m.OpportunityID, m.DecisionID)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		updated, err = tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET reservation_state='released',released_at=clock_timestamp(),release_reason='cancelled_before_withdraw',state_version=state_version+1,updated_at=clock_timestamp() WHERE id=$1 AND reservation_state<>'released'`, reservation)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// ActivateCrossMintMovement freezes certified intent and reserves capacity
// before custody can leave the reserve. A crash before signed publication is
// recovered by the existing continuation claim rather than a new opportunity.
func (s *Store) ActivateCrossMintMovement(ctx context.Context, l fleet.RevalidationLease, input CrossMintActivation) (CrossMintMovement, error) {
	var result CrossMintMovement
	p := input.Capacity
	if l.OpportunityID <= 0 || l.Owner == "" || l.FencingToken <= 0 || l.Cluster == "" || l.PrincipalUSDMicros <= 0 || input.InitialWithdrawCompiledFeeLamports <= 0 || !nonemptyCrossMintObject(input.PreflightCertification) || p.Cluster != l.Cluster || p.TargetReserve != l.TargetReserve || p.LiquidityMint != l.TargetLiquidityMint || p.ObservedSupplyUSDMicros < 0 || p.ObservedSlot <= 0 || p.MaximumInflightUSDMicros <= 0 || p.TelemetryVersion < 0 {
		return result, errors.New("activation lacks certified fee, fenced opportunity or target projection")
	}
	b, err := crossMintBindings(l.ExecutionPlan)
	if err != nil {
		return result, err
	}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var existing *int64
		if err := tx.QueryRow(ctx, `SELECT decision_id FROM loyal_yield.rebalance_opportunities WHERE id=$1 AND cluster=$2`, l.OpportunityID, l.Cluster).Scan(&existing); err != nil {
			return err
		}
		if existing != nil {
			var err error
			result, err = readCrossMintMovement(ctx, tx, *existing)
			return err
		}
		if err := lockCrossMintControl(ctx, tx, l.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, l.Cluster, true)
		if err != nil {
			return err
		}
		if !g.StartNewMovements || input.SourceControlGeneration != g.Generation {
			return errors.New("starting cross-mint movements is disabled")
		}
		var opportunity int64
		err = tx.QueryRow(ctx, crossMintActivationPolicySQL, l.OpportunityID, l.Owner, l.FencingToken, b.Settings, int16(b.VaultIndex), b.VaultPubkey, b.DelegatedSigner, b.Withdraw.PolicyAccount, int64(b.Withdraw.ObservedSlot), b.Deposit.PolicyAccount, int64(b.Deposit.ObservedSlot), b.Swap.PolicyAccount, int64(b.Swap.ObservedSlot), b.Swap.SourceShard, int32(b.Swap.MaxSlippageBPS), int64(b.Swap.DailySourceMintSpendingCap), b.Swap.ManifestFingerprint, b.Swap.EnrollmentGeneration).Scan(&opportunity)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		var vault, amount, sourceAPY, targetAPY, edge, cost, attempt, principal int64
		var source, target, sourceMint, targetMint, cluster string
		var snapshot *int64
		var plan json.RawMessage
		if err = tx.QueryRow(ctx, `SELECT cluster,vault_id,source_snapshot_id,source_reserve,target_reserve,source_liquidity_mint,target_liquidity_mint,amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,attempt_generation,principal_usd_micros,execution_plan FROM loyal_yield.rebalance_opportunities WHERE id=$1`, opportunity).Scan(&cluster, &vault, &snapshot, &source, &target, &sourceMint, &targetMint, &amount, &sourceAPY, &targetAPY, &edge, &cost, &attempt, &principal, &plan); err != nil {
			return err
		}
		if cluster != l.Cluster || vault != l.VaultID || source != l.SourceReserve || target != l.TargetReserve || sourceMint != l.SourceLiquidityMint || targetMint != l.TargetLiquidityMint || amount <= 0 || uint64(amount) != l.LiquidityAmountRaw || principal != l.PrincipalUSDMicros || !sameJSON(plan, l.ExecutionPlan) {
			return errors.New("locked opportunity differs from activation certificate")
		}
		certificateMovement := fleet.CrossMintPreparationMovement{OpportunityID: opportunity, VaultID: vault, Cluster: cluster, VaultPubkey: b.VaultPubkey, SourceReserve: source, IntendedTargetReserve: target, ActiveTargetReserve: target, SourceMint: sourceMint, TargetMint: targetMint, PlannedAmountRaw: amount, ExecutionPlan: plan, PreflightCertification: input.PreflightCertification}
		if _, err = fleet.ValidateCrossMintPreflightCertificate(certificateMovement, time.Now()); err != nil {
			return fmt.Errorf("activation prewithdraw certificate: %w", err)
		}
		var active bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions WHERE vault_id=$1 AND status IN ('planned','simulating','ready','submitted','confirming'))`, vault).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrRouteContended
		}
		var supply, slot, maximum, telemetry int64
		if err = tx.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, p.Cluster, p.TargetReserve, p.LiquidityMint).Scan(&supply, &slot, &maximum, &telemetry); err != nil {
			return err
		}
		if supply != p.ObservedSupplyUSDMicros || slot != p.ObservedSlot || maximum != p.MaximumInflightUSDMicros || telemetry != p.TelemetryVersion {
			return errors.New("target telemetry changed before activation")
		}
		var committed int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(principal_usd_micros),0)::bigint FROM loyal_yield.target_capacity_reservations WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 AND reservation_state<>'released'`, p.Cluster, p.TargetReserve, p.LiquidityMint).Scan(&committed); err != nil {
			return err
		}
		if committed > math.MaxInt64-principal || committed+principal > maximum {
			return errors.New("target capacity exhausted before activation")
		}
		economics, err := fleet.ComputeReservationEconomics(l, plan, supply, committed)
		if err != nil {
			return err
		}
		if input.InitialWithdrawCompiledFeeLamports > economics.FeeCapLamports || input.InitialWithdrawCompiledFeeLamports > cost {
			return errors.New("withdraw fee exceeds locked-frontier or immutable movement budget")
		}
		var generation, reservation int64
		if err = tx.QueryRow(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET reservation_generation=reservation_generation+1,updated_at=clock_timestamp() WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 RETURNING reservation_generation`, p.Cluster, p.TargetReserve, p.LiquidityMint).Scan(&generation); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, crossMintReserveCapacitySQL, p.Cluster, p.TargetReserve, p.LiquidityMint, opportunity, principal, supply, slot, maximum, telemetry, generation, economics.ObservedTargetAPYBPS, economics.ProjectedTargetAPYBPS, economics.SourceAPYBPS, economics.EdgeBPS, economics.NetGainUSDMicros, economics.FeeCapLamports, l.FencingToken).Scan(&reservation); err != nil {
			return err
		}
		h := sha256.New()
		_, _ = h.Write([]byte("cross-mint-movement-v1"))
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], uint64(opportunity))
		_, _ = h.Write(raw[:])
		binary.LittleEndian.PutUint64(raw[:], uint64(attempt))
		_, _ = h.Write(raw[:])
		var decision int64
		if err = tx.QueryRow(ctx, crossMintActivateDecisionSQL, vault, snapshot, source, target, sourceMint, targetMint, amount, sourceAPY, targetAPY, edge, cost, plan, hex.EncodeToString(h.Sum(nil)), g.Generation, input.PreflightCertification).Scan(&decision); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, crossMintLinkOpportunitySQL, opportunity, decision, l.Owner, l.FencingToken).Scan(&opportunity); err != nil {
			return err
		}
		attached, err := tx.Exec(ctx, crossMintAttachCapacitySQL, reservation, decision)
		if err != nil {
			return err
		}
		if attached.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		result, err = readCrossMintMovement(ctx, tx, decision)
		return err
	})
	return result, err
}

// RebindCrossMintFallbackCapacity retains the movement-owned claim, locks old
// and new frontiers in reserve order, and invalidates the old continuation
// lease. The caller must reclaim and prepare against the new durable binding.
func (s *Store) refreshCrossMintFallbackProjection(ctx context.Context, m CrossMintMovement, r fleet.MarketEpochReserve) (CrossMintCapacityProjection, error) {
	p := CrossMintCapacityProjection{Cluster: m.Cluster, TargetReserve: r.Reserve, LiquidityMint: m.TargetMint}
	shared, err := fleet.NewStoreFromPool(s.pool)
	if err != nil {
		return p, err
	}
	if err = shared.RefreshTargetCapacity(ctx, m.Cluster, r.Reserve, m.TargetMint, r.TotalSupplyUSDMicros, r.Slot); err != nil {
		return p, err
	}
	err = s.pool.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, m.Cluster, r.Reserve, m.TargetMint).Scan(&p.ObservedSupplyUSDMicros, &p.ObservedSlot, &p.MaximumInflightUSDMicros, &p.TelemetryVersion)
	if err == nil && (p.ObservedSupplyUSDMicros != r.TotalSupplyUSDMicros || p.ObservedSlot != r.Slot) {
		err = errors.New("fallback capacity advanced beyond selected reserve evidence")
	}
	return p, err
}

func (s *Store) RebindCrossMintFallbackCapacity(ctx context.Context, l CrossMintContinuationLease, p CrossMintCapacityProjection) (int64, error) {
	if p.Cluster != l.Movement.Cluster || p.LiquidityMint != l.Movement.TargetMint || p.TargetReserve == "" || p.ObservedSupplyUSDMicros < 0 || p.ObservedSlot <= 0 || p.MaximumInflightUSDMicros <= 0 || p.TelemetryVersion < 0 {
		return 0, errors.New("fallback projection is not current target-mint capacity")
	}
	var generation int64
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, p.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, p.Cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting || g.Generation != l.ControlGeneration {
			return ErrStaleOwner
		}
		m, err := lockCrossMintLease(ctx, tx, l)
		if err != nil {
			return err
		}
		if m.Phase != CrossMintTargetIdle || m.ActiveTargetReserve != m.IntendedTargetReserve || p.TargetReserve == m.ActiveTargetReserve {
			return errors.New("fallback requires idle target custody and permits only one new target binding")
		}
		var current string
		var oldGeneration, principal int64
		if err = tx.QueryRow(ctx, `SELECT target_reserve,reservation_generation,principal_usd_micros FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1 AND reservation_state='active'`, m.DecisionID).Scan(&current, &oldGeneration, &principal); err != nil {
			return err
		}
		reserves := []string{current, p.TargetReserve}
		sort.Strings(reserves)
		for _, reserve := range reserves {
			var actual CrossMintCapacityProjection
			if err = tx.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, p.Cluster, reserve, p.LiquidityMint).Scan(&actual.ObservedSupplyUSDMicros, &actual.ObservedSlot, &actual.MaximumInflightUSDMicros, &actual.TelemetryVersion); err != nil {
				return err
			}
			if reserve == p.TargetReserve && (actual.ObservedSupplyUSDMicros != p.ObservedSupplyUSDMicros || actual.ObservedSlot != p.ObservedSlot || actual.MaximumInflightUSDMicros != p.MaximumInflightUSDMicros || actual.TelemetryVersion != p.TelemetryVersion) {
				return errors.New("fallback telemetry changed; reselect from current projection")
			}
		}
		var lockedCurrent string
		var lockedGeneration int64
		if err = tx.QueryRow(ctx, `SELECT target_reserve,reservation_generation FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1 AND reservation_state='active' FOR UPDATE`, m.DecisionID).Scan(&lockedCurrent, &lockedGeneration); err != nil {
			return err
		}
		if current != lockedCurrent || oldGeneration != lockedGeneration {
			return ErrStaleOwner
		}
		var committed int64
		// NULL decision IDs are unattached but already consume target capacity.
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(principal_usd_micros),0)::bigint FROM loyal_yield.target_capacity_reservations WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 AND reservation_state<>'released' AND decision_id IS DISTINCT FROM $4`, p.Cluster, p.TargetReserve, p.LiquidityMint, m.DecisionID).Scan(&committed); err != nil {
			return err
		}
		if principal <= 0 || committed > math.MaxInt64-principal || committed+principal > p.MaximumInflightUSDMicros {
			return errors.New("fallback target capacity exhausted")
		}
		if err = tx.QueryRow(ctx, crossMintFallbackGenerationSQL, p.Cluster, p.TargetReserve, p.LiquidityMint, oldGeneration).Scan(&generation); err != nil {
			return err
		}
		var reservation int64
		if err = tx.QueryRow(ctx, crossMintRebindCapacitySQL, m.DecisionID, p.TargetReserve, p.ObservedSupplyUSDMicros, p.ObservedSlot, p.MaximumInflightUSDMicros, p.TelemetryVersion, generation).Scan(&reservation); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, crossMintRebindDecisionSQL, m.DecisionID, p.TargetReserve, l.Owner, l.FencingToken)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
	return generation, err
}

const crossMintMovementSQL = `SELECT d.id,o.id,o.optimizer_epoch_id,o.cluster,d.vault_id,v.vault_pubkey,d.source_snapshot_id,
d.source_reserve,d.target_reserve,d.active_target_reserve,d.source_liquidity_mint,d.target_liquidity_mint,
d.amount_raw,o.execution_plan,d.cross_mint_preflight_certification,d.custody_mint,d.custody_account,
d.custody_amount_raw,d.custody_observed_balance_raw,d.custody_reconciled_slot,d.custody_version,d.terminal_outcome
FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id
JOIN loyal_yield.managed_vaults v ON v.id=d.vault_id WHERE d.id=$1 AND d.movement_route='cross_mint_jupiter'`

func readCrossMintMovement(ctx context.Context, q crossMintQuerier, id int64) (CrossMintMovement, error) {
	var m CrossMintMovement
	err := q.QueryRow(ctx, crossMintMovementSQL, id).Scan(&m.DecisionID, &m.OpportunityID, &m.OptimizerEpochID, &m.Cluster, &m.VaultID, &m.VaultPubkey, &m.SourceSnapshotID, &m.SourceReserve, &m.IntendedTargetReserve, &m.ActiveTargetReserve, &m.SourceMint, &m.TargetMint, &m.PlannedAmountRaw, &m.ExecutionPlan, &m.PreflightCertification, &m.CustodyMint, &m.CustodyAccount, &m.CustodyAmountRaw, &m.CustodyObservedBalanceRaw, &m.CustodyReconciledSlot, &m.CustodyVersion, &m.TerminalOutcome)
	if err != nil {
		return m, err
	}
	if !nonemptyCrossMintObject(m.PreflightCertification) || m.SourceMint == m.TargetMint || m.CustodyAmountRaw < 0 || m.CustodyVersion < 0 {
		return m, errors.New("invalid or uncertified movement custody")
	}
	if m.TerminalOutcome != nil {
		switch *m.TerminalOutcome {
		case "completed_target":
			m.Phase = CrossMintTargetReserve
		case "recovered_source", "cancelled_before_withdraw":
			m.Phase = CrossMintSourceReserve
		case "closed_by_user":
			m.Phase = CrossMintClosedByUser
		case "manual_intervention":
			m.Phase = CrossMintManualIntervention
		default:
			return m, errors.New("unknown cross-mint terminal outcome")
		}
	} else if m.CustodyVersion == 0 {
		m.Phase = CrossMintSourceReserve
	} else if m.CustodyMint == m.SourceMint {
		m.Phase = CrossMintSourceIdle
	} else if m.CustodyMint == m.TargetMint {
		m.Phase = CrossMintTargetIdle
	} else {
		return m, errors.New("custody mint is neither source nor target")
	}
	return m, nil
}
func (s *Store) CrossMintMovement(ctx context.Context, id int64) (CrossMintMovement, error) {
	return readCrossMintMovement(ctx, s.pool, id)
}

func (s *Store) crossMintRecognizedSignatures(ctx context.Context, decision, anchor int64) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT transaction_signature FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND submission_state='reconciled' AND finalized_slot>=$2 ORDER BY finalized_slot,id LIMIT 10001`, decision, anchor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var signature string
		if err = rows.Scan(&signature); err != nil {
			return nil, err
		}
		known[signature] = true
		if len(known) > 10000 {
			return nil, errors.New("recognized movement history exceeds bounded proof window")
		}
	}
	return known, rows.Err()
}

func nonemptyCrossMintObject(raw []byte) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil && len(v) > 0
}

func crossMintALTHash(addresses []CrossMintALTAddress) string {
	h := sha256.New()
	var length [8]byte
	var ordinal [4]byte
	for _, a := range addresses {
		for _, v := range []string{a.Address, a.SemanticClass, a.AccountRole} {
			binary.LittleEndian.PutUint64(length[:], uint64(len(v)))
			_, _ = h.Write(length[:])
			_, _ = h.Write([]byte(v))
		}
		binary.LittleEndian.PutUint32(ordinal[:], uint32(a.Ordinal))
		_, _ = h.Write(ordinal[:])
		w := byte(0)
		if a.Writable {
			w = 1
		}
		_, _ = h.Write([]byte{w})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateCrossMintALTManifest(p CrossMintPreparedLeg) error {
	if !p.WaitingALT || len(p.MissingAddresses) == 0 || len(p.VaultAddresses) == 0 || len(p.SharedAddresses)+len(p.VaultAddresses) > 256 || p.Preparation.RouteFingerprint == "" || p.Preparation.RequirementsFingerprint == "" || len(p.Preparation.Transaction.UnsignedWire) != 0 {
		return errors.New("waiting ALT lacks bounded full manifest or unexpectedly carries wire")
	}
	seen := map[string]bool{}
	missing := map[string]bool{}
	for _, key := range p.MissingAddresses {
		if missing[key] || key == "" {
			return errors.New("missing ALT key is empty or duplicated")
		}
		missing[key] = true
	}
	foundMissingVault := false
	for _, set := range []struct {
		class     string
		addresses []CrossMintALTAddress
	}{{"shared_market", p.SharedAddresses}, {"vault", p.VaultAddresses}} {
		for i, a := range set.addresses {
			if _, err := sdk.PublicKeyFromBase58(a.Address); err != nil {
				return err
			}
			if seen[a.Address] || a.SemanticClass != set.class || a.Ordinal != int32(i) || a.AccountRole == "" {
				return errors.New("ALT demand is not complete normalized unique manifest")
			}
			seen[a.Address] = true
			if missing[a.Address] {
				if set.class != "vault" {
					return errors.New("missing shared ALT addresses require catalog owner, not vault provisioning")
				}
				foundMissingVault = true
				delete(missing, a.Address)
			}
		}
	}
	if !foundMissingVault || len(missing) != 0 {
		return errors.New("missing ALT keys do not belong to complete vault manifest")
	}
	return nil
}

// QueueCrossMintALT ports the retained full-manifest request/seal protocol.
// It keeps decision_created intent immutable and schedules another fenced
// continuation; it never treats a provider lookup address as a registered ID.
func (s *Store) QueueCrossMintALT(ctx context.Context, l CrossMintContinuationLease, r CrossMintLegRequest, p CrossMintPreparedLeg) (int64, error) {
	if err := validateCrossMintALTManifest(p); err != nil {
		return 0, err
	}
	if r.Movement.DecisionID != l.Movement.DecisionID || !sameJSON(p.Preparation.ExecutionPlan, l.Movement.ExecutionPlan) {
		return 0, ErrStaleOwner
	}
	sharedHash, vaultHash := crossMintALTHash(p.SharedAddresses), crossMintALTHash(p.VaultAddresses)
	var id int64
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, l.Movement.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, l.Movement.Cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting || g.Generation != l.ControlGeneration {
			return ErrStaleOwner
		}
		m, err := lockCrossMintLease(ctx, tx, l)
		if err != nil {
			return err
		}
		var holding bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND submission_state NOT IN ('reconciled','expired','failed'))`, m.DecisionID).Scan(&holding); err != nil {
			return err
		}
		if holding {
			return ErrRouteContended
		}
		shared := make([]fleet.ALTManifestAddress, 0, len(p.SharedAddresses))
		for _, a := range p.SharedAddresses {
			shared = append(shared, fleet.ALTManifestAddress{Address: a.Address, SemanticClass: a.SemanticClass, AccountRole: a.AccountRole, Ordinal: a.Ordinal, Writable: a.Writable})
		}
		if err = fleet.LockSharedALTManifestCoverage(ctx, tx, m.Cluster, shared); err != nil {
			return fmt.Errorf("shared catalog coverage missing: %w", err)
		}
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_requests(cluster,vault_id,route_fingerprint,requirements_fingerprint,desired_shared_hash,desired_vault_hash,desired_shared_address_count,desired_vault_address_count,request_status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'requested') ON CONFLICT(cluster,vault_id,requirements_fingerprint) DO NOTHING RETURNING id`, m.Cluster, m.VaultID, p.Preparation.RouteFingerprint, p.Preparation.RequirementsFingerprint, sharedHash, vaultHash, len(p.SharedAddresses), len(p.VaultAddresses)).Scan(&id)
		if err == nil {
			for _, set := range [][]CrossMintALTAddress{p.SharedAddresses, p.VaultAddresses} {
				for _, a := range set {
					if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable) VALUES($1,$2,$3,$4,$5,$6)`, id, a.Address, a.SemanticClass, a.Ordinal, a.AccountRole, a.Writable); err != nil {
						return err
					}
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
				return err
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			var actualShared, actualVault, status, errorCode string
			var sharedCount, vaultCount int
			var sealed bool
			if err = tx.QueryRow(ctx, `SELECT id,desired_shared_hash,desired_vault_hash,desired_shared_address_count,desired_vault_address_count,sealed_at IS NOT NULL,request_status,COALESCE(error_code,'') FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster=$1 AND vault_id=$2 AND requirements_fingerprint=$3 FOR UPDATE`, m.Cluster, m.VaultID, p.Preparation.RequirementsFingerprint).Scan(&id, &actualShared, &actualVault, &sharedCount, &vaultCount, &sealed, &status, &errorCode); err != nil {
				return err
			}
			if !sealed || actualShared != sharedHash || actualVault != vaultHash || sharedCount != len(p.SharedAddresses) || vaultCount != len(p.VaultAddresses) {
				return errors.New("sealed ALT request idempotency collision")
			}
			rows, err := tx.Query(ctx, `SELECT address,semantic_class,ordinal,account_role,is_writable FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 ORDER BY semantic_class,ordinal`, id)
			if err != nil {
				return err
			}
			actual := []CrossMintALTAddress{}
			for rows.Next() {
				var a CrossMintALTAddress
				if err = rows.Scan(&a.Address, &a.SemanticClass, &a.Ordinal, &a.AccountRole, &a.Writable); err != nil {
					rows.Close()
					return err
				}
				actual = append(actual, a)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			expected := append(append([]CrossMintALTAddress(nil), p.SharedAddresses...), p.VaultAddresses...)
			if len(actual) != len(expected) {
				return errors.New("sealed ALT request address count differs")
			}
			for i := range actual {
				if actual[i] != expected[i] {
					return errors.New("sealed ALT request address roles differ")
				}
			}
			if status == "failed" && errorCode == "terminal_lookup_table_operation" {
				return errors.New("ALT request has terminal operation failure")
			}
			if status == "failed" || status == "cancelled" || status == "satisfied" {
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status='requested',requested_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,error_code=NULL,error_detail=NULL,satisfied_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
					return err
				}
			}
		} else {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET continuation_available_at=clock_timestamp()+interval '5 seconds',continuation_lease_owner=NULL,continuation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND continuation_lease_owner=$2 AND continuation_fencing_token=$3 AND continuation_control_generation=$4 AND continuation_lease_expires_at>clock_timestamp()`, m.DecisionID, l.Owner, l.FencingToken, l.ControlGeneration)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
	return id, err
}

func (s *Store) CrossMintGates(ctx context.Context, cluster string) (CrossMintGates, error) {
	if cluster == "" {
		return CrossMintGates{}, errors.New("missing movement cluster")
	}
	return readCrossMintGates(ctx, s.pool, cluster, false)
}
func readCrossMintGates(ctx context.Context, q crossMintQuerier, cluster string, locked bool) (CrossMintGates, error) {
	g := CrossMintGates{ContinueOrRecoverExisting: true}
	sql := `SELECT start_new_movements,continue_or_recover_existing,generation FROM loyal_yield.cross_mint_movement_controls WHERE cluster=$1`
	if locked {
		sql += " FOR SHARE"
	}
	err := q.QueryRow(ctx, sql, cluster).Scan(&g.StartNewMovements, &g.ContinueOrRecoverExisting, &g.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrossMintGates{ContinueOrRecoverExisting: true}, nil
	}
	return g, err
}
func lockCrossMintControl(ctx context.Context, tx pgx.Tx, cluster string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "loyal-yield-cross-mint-control:"+cluster)
	return err
}

func (s *Store) ClaimCrossMintContinuation(ctx context.Context, cluster, owner string, ttl time.Duration) (*CrossMintContinuationLease, error) {
	if cluster == "" || owner == "" || ttl < 10*time.Second || ttl > 300*time.Second || ttl%time.Second != 0 {
		return nil, errors.New("continuation requires identity and 10-300 second lease")
	}
	var lease *CrossMintContinuationLease
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, cluster, true)
		if err != nil || !g.ContinueOrRecoverExisting {
			return err
		}
		l := CrossMintContinuationLease{Owner: owner, ControlGeneration: g.Generation}
		var id int64
		err = tx.QueryRow(ctx, crossMintClaimSQL, cluster, owner, int32(ttl/time.Second), g.Generation).Scan(&id, &l.FencingToken, &l.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		l.Movement, err = readCrossMintMovement(ctx, tx, id)
		if err == nil {
			lease = &l
		}
		return err
	})
	return lease, err
}

func lockCrossMintLease(ctx context.Context, tx pgx.Tx, l CrossMintContinuationLease) (CrossMintMovement, error) {
	var expires, timeNow time.Time
	err := tx.QueryRow(ctx, `SELECT continuation_lease_expires_at FROM loyal_yield.rebalance_decisions
WHERE id=$1 AND movement_route='cross_mint_jupiter' AND status='confirming' AND terminal_outcome IS NULL
AND continuation_lease_owner=$2 AND continuation_fencing_token=$3 AND continuation_control_generation=$4
AND continuation_lease_expires_at>clock_timestamp() FOR UPDATE`, l.Movement.DecisionID, l.Owner, l.FencingToken, l.ControlGeneration).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrossMintMovement{}, ErrStaleOwner
	}
	if err != nil {
		return CrossMintMovement{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&timeNow); err != nil {
		return CrossMintMovement{}, err
	}
	if !expires.After(timeNow) {
		return CrossMintMovement{}, ErrStaleOwner
	}
	m, err := readCrossMintMovement(ctx, tx, l.Movement.DecisionID)
	if err == nil && (m.OpportunityID != l.Movement.OpportunityID || m.Cluster != l.Movement.Cluster || m.CustodyVersion != l.Movement.CustodyVersion || m.CustodyAmountRaw != l.Movement.CustodyAmountRaw || m.CustodyMint != l.Movement.CustodyMint || m.CustodyAccount != l.Movement.CustodyAccount || m.ActiveTargetReserve != l.Movement.ActiveTargetReserve) {
		return m, ErrStaleOwner
	}
	return m, err
}

func (s *Store) CrossMintLegBudget(ctx context.Context, l CrossMintContinuationLease, leg string) (int64, int64, error) {
	if leg != LegWithdraw && leg != LegSwap && leg != LegDeposit {
		return 0, 0, ErrNotClaimable
	}
	var generation, remaining int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT max(leg_generation) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND movement_leg=$2),0)+1,
o.estimated_cost_lamports-COALESCE((SELECT sum(compiled_fee_lamports) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),0)::bigint
FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id
WHERE d.id=$1 AND d.continuation_lease_owner=$3 AND d.continuation_fencing_token=$4 AND d.continuation_control_generation=$5
AND d.continuation_lease_expires_at>clock_timestamp() AND d.terminal_outcome IS NULL`, l.Movement.DecisionID, leg, l.Owner, l.FencingToken, l.ControlGeneration).Scan(&generation, &remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrStaleOwner
	}
	if err == nil && remaining <= 0 {
		err = errors.New("movement fee budget exhausted")
	}
	return generation, remaining, err
}

func (s *Store) AppendCrossMintLeg(ctx context.Context, l CrossMintContinuationLease, r CrossMintLegRequest, p CrossMintPreparedLeg, wire WireIdentity) (int64, error) {
	if r.Movement.DecisionID != l.Movement.DecisionID || r.Movement.CustodyVersion != l.Movement.CustodyVersion {
		return 0, ErrStaleOwner
	}
	if err := validateCrossMintPrepared(r, p); err != nil {
		return 0, err
	}
	if wire.MessageHash != p.Preparation.Transaction.MessageSHA256 || wire.LastValidBlockHeight != p.LastValidBlockHeight {
		return 0, errors.New("signed leg differs from prepared transaction")
	}
	payer, err := validateCrossMintWire(wire, p)
	if err != nil {
		return 0, err
	}
	epochs, err := marshalCrossMintALTEvidence(p)
	if err != nil {
		return 0, err
	}
	effect, _ := json.Marshal(p.ExpectedEffect)
	anchors, _ := json.Marshal(p.BalanceAnchors)
	semantic := crossMintSemantic(l.Movement.DecisionID, r.Leg, r.Generation)
	var id int64
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, l.Movement.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, l.Movement.Cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting || g.Generation != l.ControlGeneration {
			return ErrStaleOwner
		}
		m, err := lockCrossMintLease(ctx, tx, l)
		if err != nil {
			return err
		}
		if err = validateCrossMintLeg(CrossMintLegRequest{Movement: m, Leg: r.Leg, Purpose: r.Purpose}, p.ExpectedEffect, p.BalanceAnchors); err != nil {
			return err
		}
		bindings, err := crossMintBindings(m.ExecutionPlan)
		if err != nil {
			return err
		}
		policy := bindings.Deposit.PolicyAccount
		if r.Leg == LegWithdraw || r.Purpose == PurposeRecoverSource {
			policy = bindings.Withdraw.PolicyAccount
		} else if r.Leg == LegSwap {
			policy = bindings.Swap.PolicyAccount
		}
		if p.PolicyAccount != policy {
			return errors.New("leg policy differs from immutable movement binding")
		}
		if r.Leg == LegWithdraw && r.Generation == 1 {
			var activation *int64
			if err = tx.QueryRow(ctx, `SELECT cross_mint_activation_control_generation FROM loyal_yield.rebalance_decisions WHERE id=$1`, m.DecisionID).Scan(&activation); err != nil {
				return err
			}
			if !g.StartNewMovements || activation == nil || *activation != g.Generation {
				return ErrStaleOwner
			}
			if err = checkCrossMintInitialPolicies(ctx, tx, m, bindings); err != nil {
				return err
			}
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND submission_state NOT IN ('reconciled','expired','failed'))`, m.DecisionID).Scan(&live); err != nil {
			return err
		}
		if live {
			return ErrRouteContended
		}
		var previous int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(leg_generation),0) FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND movement_leg=$2`, m.DecisionID, r.Leg).Scan(&previous); err != nil {
			return err
		}
		if previous == math.MaxInt64 || r.Generation != previous+1 {
			return errors.New("leg generation is not next generation")
		}
		if previous > 0 {
			var proved bool
			if err = tx.QueryRow(ctx, `SELECT s.submission_state IN ('expired','failed') AND EXISTS(SELECT 1 FROM loyal_yield.cross_mint_no_effect_receipts n WHERE n.submission_id=s.id AND n.decision_id=s.decision_id AND n.movement_leg=s.movement_leg AND n.leg_generation=s.leg_generation AND n.transaction_signature=s.transaction_signature) FROM loyal_yield.signed_route_submissions s WHERE decision_id=$1 AND movement_leg=$2 ORDER BY leg_generation DESC LIMIT 1`, m.DecisionID, r.Leg).Scan(&proved); err != nil {
				return err
			}
			if !proved {
				return errors.New("higher leg generation requires terminal no-effect receipt")
			}
		}
		var fees, cap int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(s.compiled_fee_lamports),0)::bigint,o.estimated_cost_lamports FROM loyal_yield.rebalance_opportunities o LEFT JOIN loyal_yield.signed_route_submissions s ON s.decision_id=o.decision_id WHERE o.id=$1 GROUP BY o.estimated_cost_lamports`, m.OpportunityID).Scan(&fees, &cap); err != nil {
			return err
		}
		fee := int64(p.Preparation.Transaction.FeeLamports)
		if fees > math.MaxInt64-fee || fees+fee > cap {
			return errors.New("movement cumulative fee cap exceeded")
		}
		admission := fleet.ExecutionAdmission{Lease: fleet.RevalidationLease{Cluster: m.Cluster, VaultID: m.VaultID}, SelectedALTs: p.SelectedALTs, Preparation: p.Preparation, Evidence: fleet.FreshRouteEvidence{Slot: p.Preparation.Simulation.Slot}}
		if err = lockAdmissionALTs(ctx, tx, admission); err != nil {
			return err
		}
		// Signing consumer owns prepared usage. The C factory is read-only and
		// cannot acquire leases; exact registered members are locked above.
		for _, table := range p.SelectedALTs {
			var leaseID int64
			err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,binding_id,route_fingerprint,requirements_fingerprint,expires_at) VALUES($1,'prepared_transaction',$2,$3,$4,$5,$6,$7,clock_timestamp()+interval '5 minutes') ON CONFLICT(lease_kind,reference_key,route_lookup_table_id) DO UPDATE SET expires_at=GREATEST(loyal_yield.lookup_table_usage_leases.expires_at,EXCLUDED.expires_at),updated_at=clock_timestamp() WHERE loyal_yield.lookup_table_usage_leases.cluster=EXCLUDED.cluster AND loyal_yield.lookup_table_usage_leases.vault_id=EXCLUDED.vault_id AND loyal_yield.lookup_table_usage_leases.binding_id IS NOT DISTINCT FROM EXCLUDED.binding_id AND loyal_yield.lookup_table_usage_leases.requirements_fingerprint=EXCLUDED.requirements_fingerprint AND loyal_yield.lookup_table_usage_leases.released_at IS NULL RETURNING id`, m.Cluster, semantic, table.TableID, m.VaultID, table.BindingID, p.Preparation.RouteFingerprint, p.Preparation.RequirementsFingerprint).Scan(&leaseID)
			if err != nil {
				return err
			}
		}
		if len(p.SelectedALTs) > 0 {
			if err = checkPreparedALTUsage(ctx, tx, epochs, semantic, m.Cluster, p.Preparation.RequirementsFingerprint); err != nil {
				return err
			}
		}
		keys := append([]string(nil), p.ConflictKeys...)
		sort.Strings(keys)
		for i, key := range keys {
			if key == "" || i > 0 && key == keys[i-1] {
				return errors.New("invalid or duplicate conflict key")
			}
			var acquired string
			if err = tx.QueryRow(ctx, crossMintConflictSQL, m.Cluster, key, m.OpportunityID, l.Owner, l.FencingToken, time.Now().Add(10*time.Minute)).Scan(&acquired); err != nil {
				return fmt.Errorf("%w: %v", ErrConflictLeaseHeld, err)
			}
		}
		// Only the retained policy payer is supported by this consumer. A future
		// fee-only shard needs its existing durable spend-reservation protocol.
		if payer != bindings.DelegatedSigner {
			return errors.New("cross-mint fee-only payer reservation is not configured")
		}
		var bound bool
		if err = tx.QueryRow(ctx, crossMintPolicyPayerSQL, m.OpportunityID, m.Cluster, payer, epochs).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return errors.New("policy payer is not bound to current vault and ALT authority")
		}
		err = tx.QueryRow(ctx, crossMintInsertLegSQL, m.Cluster, semantic, m.OpportunityID, m.DecisionID, wire.SignedTransaction, wire.SignedTransactionHash, wire.MessageHash, wire.TransactionSignature, wire.RecentBlockhash, wire.LastValidBlockHeight, m.SourceSnapshotID, m.OptimizerEpochID, p.Preparation.RequirementsFingerprint, p.AltSelectionFingerprint, epochs, payer, "policy", fee, p.Preparation.Transaction.WritableAccounts, keys, l.Owner, l.FencingToken, r.Leg, r.Purpose, r.Generation, p.PolicyAccount, effect, anchors).Scan(&id)
		if err != nil {
			return err
		}
		attached, err := tx.Exec(ctx, crossMintAttachConflictsSQL, m.Cluster, m.OpportunityID, l.Owner, l.FencingToken, id, keys)
		if err != nil {
			return err
		}
		if attached.RowsAffected() != int64(len(keys)) {
			return ErrStaleOwner
		}

		advanced, err := tx.Exec(ctx, crossMintClearContinuationSQL, m.DecisionID, l.Owner, l.FencingToken)
		if err != nil {
			return err
		}
		if advanced.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
	return id, err
}

func crossMintBindings(plan []byte) (fleet.CrossMintPolicyBindings, error) {
	var p struct {
		Bindings fleet.CrossMintPolicyBindings `json:"policy_bindings"`
	}
	if err := json.Unmarshal(plan, &p); err != nil {
		return p.Bindings, err
	}
	b := p.Bindings
	if b.Settings == "" || b.VaultPubkey == "" || b.DelegatedSigner == "" || b.Withdraw.PolicyAccount == "" || b.Deposit.PolicyAccount == "" || b.Swap.PolicyAccount == "" || b.Withdraw.PolicyAccount == b.Swap.PolicyAccount || b.Deposit.PolicyAccount == b.Swap.PolicyAccount || b.Withdraw.ObservedSlot <= 0 || b.Withdraw.ObservedSlot > math.MaxInt64 || b.Deposit.ObservedSlot <= 0 || b.Deposit.ObservedSlot > math.MaxInt64 || b.Swap.ObservedSlot <= 0 || b.Swap.ObservedSlot > math.MaxInt64 || b.Withdraw.ConstraintIndex != 0 || b.Deposit.ConstraintIndex != 1 || b.Withdraw.SourceCommitment != "finalized" || b.Deposit.SourceCommitment != "finalized" || b.Swap.SourceCommitment != "confirmed" && b.Swap.SourceCommitment != "finalized" || b.Swap.SourceShard != "classic" && b.Swap.SourceShard != "token_2022" || b.Swap.EnrollmentGeneration <= 0 || b.Swap.MaxSlippageBPS == 0 || b.Swap.MaxSlippageBPS > 10000 || b.Swap.DailySourceMintSpendingCap == 0 || b.Swap.DailySourceMintSpendingCap > math.MaxInt64 || b.Swap.ManifestFingerprint == "" {
		return b, errors.New("invalid immutable cross-mint policy bindings")
	}
	return b, nil
}
func checkCrossMintInitialPolicies(ctx context.Context, tx pgx.Tx, m CrossMintMovement, b fleet.CrossMintPolicyBindings) error {
	var id int64
	err := tx.QueryRow(ctx, crossMintInitialPolicySQL, m.Cluster, b.Settings, int16(b.VaultIndex), b.VaultPubkey, b.DelegatedSigner, b.Withdraw.PolicyAccount, m.SourceMint, b.Deposit.PolicyAccount, b.Deposit.ObservedSlot, m.TargetMint, b.Swap.PolicyAccount, b.Swap.ObservedSlot, b.Swap.SourceShard, int32(b.Swap.MaxSlippageBPS), b.Withdraw.ObservedSlot, int64(b.Swap.DailySourceMintSpendingCap), b.Swap.ManifestFingerprint, b.Swap.EnrollmentGeneration).Scan(&id)
	return err
}

// Retained movement.rs SQL predicates; clock_timestamp fences lock waits.
const crossMintClaimSQL = `WITH candidate AS (
    SELECT decision.id
    FROM loyal_yield.rebalance_decisions decision
    JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.decision_id = decision.id
     AND opportunity.opportunity_state = 'decision_created'
     AND opportunity.cluster = $1
    WHERE decision.movement_route = 'cross_mint_jupiter'
      AND decision.status = 'confirming'::loyal_yield.decision_status
      AND decision.terminal_outcome IS NULL
      AND decision.continuation_available_at <= clock_timestamp()
      AND (
          decision.continuation_lease_expires_at IS NULL
          OR decision.continuation_lease_expires_at <= clock_timestamp()
      )
      AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.signed_route_submissions submission
          WHERE submission.decision_id = decision.id
            AND submission.submission_state NOT IN (
                'reconciled', 'expired', 'failed'
            )
      )
    ORDER BY decision.continuation_available_at,
             decision.created_at, decision.id
    FOR UPDATE OF decision SKIP LOCKED
    LIMIT 1
)
UPDATE loyal_yield.rebalance_decisions decision
SET continuation_lease_owner = $2,
    continuation_lease_expires_at = clock_timestamp()
        + make_interval(secs => $3::INTEGER),
    continuation_fencing_token = continuation_fencing_token + 1,
    continuation_attempt_count = continuation_attempt_count + 1,
    continuation_control_generation = $4,
    updated_at = clock_timestamp()
FROM candidate
WHERE decision.id = candidate.id
RETURNING decision.id,
          decision.continuation_fencing_token,
          decision.continuation_lease_expires_at`

const crossMintConflictSQL = `INSERT INTO loyal_yield.route_account_conflict_leases AS conflict
    (cluster, writable_account_key, opportunity_id, lease_owner,
     fencing_token, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cluster, writable_account_key) DO UPDATE
SET opportunity_id = EXCLUDED.opportunity_id,
    lease_owner = EXCLUDED.lease_owner,
    fencing_token = EXCLUDED.fencing_token,
    expires_at = EXCLUDED.expires_at,
    submission_id = NULL,
    updated_at = clock_timestamp()
WHERE conflict.submission_id IS NULL
  AND (
      conflict.expires_at <= clock_timestamp()
      OR (
          conflict.opportunity_id = EXCLUDED.opportunity_id
          AND conflict.lease_owner = EXCLUDED.lease_owner
          AND conflict.fencing_token = EXCLUDED.fencing_token
      )
  )
RETURNING writable_account_key`

const crossMintInsertLegSQL = `INSERT INTO loyal_yield.signed_route_submissions
    (cluster, semantic_key, opportunity_id, decision_id,
     signed_transaction, signed_transaction_hash, message_hash,
     transaction_signature, recent_blockhash,
     last_valid_block_height, source_snapshot_id,
     optimizer_epoch_id, alt_requirements_fingerprint,
     alt_selection_fingerprint, alt_mutation_epochs, fee_payer,
     fee_payer_kind, compiled_fee_lamports,
     writable_account_keys, conflict_account_keys,
     executor_owner, executor_fencing_token,
     movement_leg, leg_purpose, leg_generation,
     required_commitment, policy_account, expected_effect,
     expected_balance_anchors)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
     $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
     $21, $22, $23, $24, $25, 'finalized', $26, $27, $28)
RETURNING id`

const crossMintAttachConflictsSQL = `UPDATE loyal_yield.route_account_conflict_leases
SET submission_id = $5,
    expires_at = GREATEST(expires_at, clock_timestamp() + interval '10 minutes'),
    updated_at = clock_timestamp()
WHERE cluster = $1
  AND opportunity_id = $2
  AND lease_owner = $3
  AND fencing_token = $4
  AND writable_account_key = ANY($6)
  AND submission_id IS NULL`

const crossMintClearContinuationSQL = `UPDATE loyal_yield.rebalance_decisions
SET continuation_available_at = NULL,
    continuation_lease_owner = NULL,
    continuation_lease_expires_at = NULL,
    signature = NULL,
    confirmed_slot = NULL,
    updated_at = clock_timestamp()
WHERE id = $1
  AND continuation_lease_owner = $2
  AND continuation_fencing_token = $3
  AND continuation_lease_expires_at > clock_timestamp()`

const crossMintInitialPolicySQL = `SELECT withdraw_policy.id
FROM loyal_yield.route_policies withdraw_policy
JOIN loyal_yield.route_policies deposit_policy
  ON deposit_policy.authority = withdraw_policy.authority
 AND deposit_policy.cluster = $1
 AND deposit_policy.settings = $2
 AND deposit_policy.vault_index = $3
 AND deposit_policy.vault_pubkey = $4
 AND deposit_policy.delegated_signers = ARRAY[$5]::TEXT[]
 AND deposit_policy.threshold = 1
 AND deposit_policy.policy_account = $8
 AND deposit_policy.last_seen_slot >= $9
 AND deposit_policy.active
 AND deposit_policy.finalized_eligible
 AND deposit_policy.source_commitment = 'finalized'
 AND 'same_mint_kamino' = ANY(deposit_policy.route_modes)
 AND $10 = ANY(deposit_policy.stable_mints)
 AND $10 = ANY(deposit_policy.kamino_liquidity_mints)
JOIN loyal_yield.cross_mint_swap_policies swap_policy
  ON swap_policy.authority = withdraw_policy.authority
 AND swap_policy.cluster = $1
 AND swap_policy.settings = $2
 AND swap_policy.vault_index = $3
 AND swap_policy.vault_pubkey = $4
 AND swap_policy.delegated_signer = $5
 AND swap_policy.policy_account = $11
 AND swap_policy.last_seen_slot >= $12
 AND swap_policy.source_shard = $13
 AND swap_policy.max_slippage_bps = $14
 AND swap_policy.daily_source_mint_spending_cap = $16
 AND swap_policy.manifest_fingerprint = $17
 AND swap_policy.active
 AND swap_policy.start_eligible
 AND swap_policy.source_commitment IN ('confirmed', 'finalized')
 AND swap_policy.last_mutation IN ('create', 'update')
JOIN loyal_yield.cross_mint_vault_opt_ins opt_in
  ON opt_in.enabled = TRUE
 AND opt_in.cluster = swap_policy.cluster
 AND opt_in.settings = swap_policy.settings
 AND opt_in.vault_index = swap_policy.vault_index
 AND opt_in.vault_pubkey = swap_policy.vault_pubkey
 AND opt_in.generation = $18
 AND NOT EXISTS (
     SELECT 1
     FROM loyal_yield.cross_mint_swap_policies newer_swap_policy
     WHERE newer_swap_policy.cluster = swap_policy.cluster
       AND newer_swap_policy.settings = swap_policy.settings
       AND newer_swap_policy.vault_index = swap_policy.vault_index
       AND newer_swap_policy.vault_pubkey = swap_policy.vault_pubkey
       AND newer_swap_policy.source_shard = swap_policy.source_shard
       AND (newer_swap_policy.last_seen_slot, newer_swap_policy.id)
           > (swap_policy.last_seen_slot, swap_policy.id)
 )
JOIN loyal_yield.cross_mint_swap_policies sibling_policy
  ON sibling_policy.cluster = swap_policy.cluster
 AND sibling_policy.settings = swap_policy.settings
 AND sibling_policy.authority = swap_policy.authority
 AND sibling_policy.vault_index = swap_policy.vault_index
 AND sibling_policy.vault_pubkey = swap_policy.vault_pubkey
 AND sibling_policy.delegated_signer = swap_policy.delegated_signer
 AND sibling_policy.policy_account <> swap_policy.policy_account
 AND sibling_policy.source_shard <> swap_policy.source_shard
 AND sibling_policy.active
 AND sibling_policy.start_eligible
 AND sibling_policy.source_commitment IN ('confirmed', 'finalized')
 AND sibling_policy.last_mutation IN ('create', 'update')
 AND sibling_policy.max_slippage_bps = swap_policy.max_slippage_bps
 AND sibling_policy.daily_source_mint_spending_cap =
     swap_policy.daily_source_mint_spending_cap
 AND NOT EXISTS (
     SELECT 1
     FROM loyal_yield.cross_mint_swap_policies newer_sibling_policy
     WHERE newer_sibling_policy.cluster = sibling_policy.cluster
       AND newer_sibling_policy.settings = sibling_policy.settings
       AND newer_sibling_policy.vault_index = sibling_policy.vault_index
       AND newer_sibling_policy.vault_pubkey = sibling_policy.vault_pubkey
       AND newer_sibling_policy.source_shard = sibling_policy.source_shard
       AND (newer_sibling_policy.last_seen_slot, newer_sibling_policy.id)
           > (sibling_policy.last_seen_slot, sibling_policy.id)
 )
 AND 2 = (
     SELECT count(DISTINCT sibling.source_shard)
     FROM loyal_yield.cross_mint_swap_policies sibling
     WHERE sibling.cluster = swap_policy.cluster
       AND sibling.settings = swap_policy.settings
       AND sibling.authority = swap_policy.authority
       AND sibling.vault_index = swap_policy.vault_index
       AND sibling.vault_pubkey = swap_policy.vault_pubkey
       AND sibling.delegated_signer = swap_policy.delegated_signer
       AND sibling.active
       AND sibling.start_eligible
       AND sibling.source_commitment IN ('confirmed', 'finalized')
       AND sibling.last_mutation IN ('create', 'update')
       AND sibling.max_slippage_bps = swap_policy.max_slippage_bps
       AND sibling.daily_source_mint_spending_cap =
           swap_policy.daily_source_mint_spending_cap
 )
WHERE withdraw_policy.cluster = $1
  AND withdraw_policy.settings = $2
  AND withdraw_policy.vault_index = $3
  AND withdraw_policy.vault_pubkey = $4
  AND withdraw_policy.delegated_signers = ARRAY[$5]::TEXT[]
  AND withdraw_policy.threshold = 1
  AND withdraw_policy.policy_account = $6
  AND withdraw_policy.last_seen_slot >= $15
  AND withdraw_policy.active
  AND withdraw_policy.finalized_eligible
  AND withdraw_policy.source_commitment = 'finalized'
  AND 'same_mint_kamino' = ANY(withdraw_policy.route_modes)
  AND $7 = ANY(withdraw_policy.stable_mints)
  AND $7 = ANY(withdraw_policy.kamino_liquidity_mints)
FOR SHARE OF withdraw_policy, deposit_policy, swap_policy, sibling_policy, opt_in`

const crossMintActivationPolicySQL = `SELECT opportunity.id
FROM loyal_yield.rebalance_opportunities opportunity
JOIN loyal_yield.optimizer_epochs epoch
  ON epoch.id = opportunity.optimizer_epoch_id
 AND epoch.cluster = opportunity.cluster
JOIN loyal_yield.managed_vaults vault
  ON vault.id = opportunity.vault_id
 AND vault.active
 AND vault.settings = $4
 AND vault.vault_index = $5
 AND vault.vault_pubkey = $6
JOIN loyal_yield.route_policies withdraw_policy
  ON withdraw_policy.active
 AND withdraw_policy.finalized_eligible
 AND withdraw_policy.source_commitment = 'finalized'
 AND withdraw_policy.cluster = opportunity.cluster
 AND withdraw_policy.settings = $4
 AND withdraw_policy.vault_index = $5
 AND withdraw_policy.vault_pubkey = $6
 AND withdraw_policy.delegated_signers = ARRAY[$7]::TEXT[]
 AND withdraw_policy.threshold = 1
 AND withdraw_policy.policy_account = $8
 AND withdraw_policy.last_seen_slot >= $9
 AND 'same_mint_kamino' = ANY(withdraw_policy.route_modes)
 AND opportunity.source_liquidity_mint = ANY(withdraw_policy.stable_mints)
 AND opportunity.source_liquidity_mint = ANY(withdraw_policy.kamino_liquidity_mints)
JOIN loyal_yield.route_policies deposit_policy
  ON deposit_policy.active
 AND deposit_policy.finalized_eligible
 AND deposit_policy.source_commitment = 'finalized'
 AND deposit_policy.cluster = opportunity.cluster
 AND deposit_policy.settings = $4
 AND deposit_policy.authority = withdraw_policy.authority
 AND deposit_policy.vault_index = $5
 AND deposit_policy.vault_pubkey = $6
 AND deposit_policy.delegated_signers = ARRAY[$7]::TEXT[]
 AND deposit_policy.threshold = 1
 AND deposit_policy.policy_account = $10
 AND deposit_policy.last_seen_slot >= $11
 AND 'same_mint_kamino' = ANY(deposit_policy.route_modes)
 AND opportunity.target_liquidity_mint = ANY(deposit_policy.stable_mints)
 AND opportunity.target_liquidity_mint = ANY(deposit_policy.kamino_liquidity_mints)
JOIN loyal_yield.cross_mint_swap_policies swap_policy
  ON swap_policy.cluster = opportunity.cluster
 AND swap_policy.settings = $4
 AND swap_policy.authority = withdraw_policy.authority
 AND swap_policy.vault_index = $5
 AND swap_policy.vault_pubkey = $6
 AND swap_policy.delegated_signer = $7
 AND swap_policy.policy_account = $12
 AND swap_policy.last_seen_slot >= $13
 AND swap_policy.source_shard = $14
 AND swap_policy.max_slippage_bps = $15
 AND swap_policy.daily_source_mint_spending_cap = $16
 AND swap_policy.manifest_fingerprint = $17
 AND swap_policy.active
 AND swap_policy.start_eligible
 AND swap_policy.source_commitment IN ('confirmed', 'finalized')
 AND swap_policy.last_mutation IN ('create', 'update')
JOIN loyal_yield.cross_mint_vault_opt_ins opt_in
  ON opt_in.enabled = TRUE
 AND opt_in.cluster = swap_policy.cluster
 AND opt_in.settings = swap_policy.settings
 AND opt_in.vault_index = swap_policy.vault_index
 AND opt_in.vault_pubkey = swap_policy.vault_pubkey
 AND opt_in.generation = $18
 AND NOT EXISTS (
     SELECT 1
     FROM loyal_yield.cross_mint_swap_policies newer_swap_policy
     WHERE newer_swap_policy.cluster = swap_policy.cluster
       AND newer_swap_policy.settings = swap_policy.settings
       AND newer_swap_policy.vault_index = swap_policy.vault_index
       AND newer_swap_policy.vault_pubkey = swap_policy.vault_pubkey
       AND newer_swap_policy.source_shard = swap_policy.source_shard
       AND (newer_swap_policy.last_seen_slot, newer_swap_policy.id)
           > (swap_policy.last_seen_slot, swap_policy.id)
 )
JOIN loyal_yield.cross_mint_swap_policies sibling_policy
  ON sibling_policy.cluster = swap_policy.cluster
 AND sibling_policy.settings = swap_policy.settings
 AND sibling_policy.authority = swap_policy.authority
 AND sibling_policy.vault_index = swap_policy.vault_index
 AND sibling_policy.vault_pubkey = swap_policy.vault_pubkey
 AND sibling_policy.delegated_signer = swap_policy.delegated_signer
 AND sibling_policy.policy_account <> swap_policy.policy_account
 AND sibling_policy.source_shard <> swap_policy.source_shard
 AND sibling_policy.active
 AND sibling_policy.start_eligible
 AND sibling_policy.source_commitment IN ('confirmed', 'finalized')
 AND sibling_policy.last_mutation IN ('create', 'update')
 AND sibling_policy.max_slippage_bps = swap_policy.max_slippage_bps
 AND sibling_policy.daily_source_mint_spending_cap =
     swap_policy.daily_source_mint_spending_cap
 AND NOT EXISTS (
     SELECT 1
     FROM loyal_yield.cross_mint_swap_policies newer_sibling_policy
     WHERE newer_sibling_policy.cluster = sibling_policy.cluster
       AND newer_sibling_policy.settings = sibling_policy.settings
       AND newer_sibling_policy.vault_index = sibling_policy.vault_index
       AND newer_sibling_policy.vault_pubkey = sibling_policy.vault_pubkey
       AND newer_sibling_policy.source_shard = sibling_policy.source_shard
       AND (newer_sibling_policy.last_seen_slot, newer_sibling_policy.id)
           > (sibling_policy.last_seen_slot, sibling_policy.id)
 )
 AND 2 = (
     SELECT count(DISTINCT sibling.source_shard)
     FROM loyal_yield.cross_mint_swap_policies sibling
     WHERE sibling.cluster = swap_policy.cluster
       AND sibling.settings = swap_policy.settings
       AND sibling.authority = swap_policy.authority
       AND sibling.vault_index = swap_policy.vault_index
       AND sibling.vault_pubkey = swap_policy.vault_pubkey
       AND sibling.delegated_signer = swap_policy.delegated_signer
       AND sibling.active
       AND sibling.start_eligible
       AND sibling.source_commitment IN ('confirmed', 'finalized')
       AND sibling.last_mutation IN ('create', 'update')
       AND sibling.max_slippage_bps = swap_policy.max_slippage_bps
       AND sibling.daily_source_mint_spending_cap =
           swap_policy.daily_source_mint_spending_cap
 )
WHERE opportunity.id = $1
  AND opportunity.opportunity_state = 'leased'
  AND opportunity.lease_kind = 'execute'
  AND opportunity.lease_owner = $2
  AND opportunity.fencing_token = $3
  AND opportunity.lease_expires_at > clock_timestamp()
  AND opportunity.source_reserve IS NOT NULL
  AND opportunity.source_liquidity_mint
        <> opportunity.target_liquidity_mint
  AND opportunity.execution_plan ->> 'kind' = 'cross_mint_jupiter'
FOR UPDATE OF opportunity, vault, withdraw_policy, deposit_policy,
    swap_policy, sibling_policy, opt_in`

const crossMintActivateDecisionSQL = `INSERT INTO loyal_yield.rebalance_decisions
    (vault_id, source_snapshot_id, status, source_reserve,
     target_reserve, liquidity_mint, source_liquidity_mint,
     target_liquidity_mint, amount_raw, source_apy_bps,
     target_apy_bps, estimated_edge_bps,
     estimated_cost_lamports, decision_reason, execution_plan,
     idempotency_key, movement_route, active_target_reserve,
     custody_mint, custody_amount_raw, custody_account,
     custody_version, continuation_available_at,
     cross_mint_activation_control_generation,
     cross_mint_preflight_certification)
VALUES
    ($1, $2, 'confirming'::loyal_yield.decision_status,
     $3, $4, NULL, $5, $6, $7, $8, $9, $10, $11,
     'target_supply_apy_exceeds_source'::loyal_yield.decision_reason,
     $12, $13, 'cross_mint_jupiter', $4, $5, $7, $3, 0, clock_timestamp(), $14,
     $15)
RETURNING id`

const crossMintLinkOpportunitySQL = `UPDATE loyal_yield.rebalance_opportunities
SET opportunity_state = 'decision_created',
    decision_id = $2,
    lease_kind = NULL,
    lease_owner = NULL,
    lease_expires_at = NULL,
    terminal_reason = NULL,
    updated_at = clock_timestamp()
WHERE id = $1
  AND opportunity_state = 'leased'
  AND lease_kind = 'execute'
  AND lease_owner = $3
  AND fencing_token = $4
  AND lease_expires_at > clock_timestamp()
RETURNING id`

const crossMintAttachCapacitySQL = `UPDATE loyal_yield.target_capacity_reservations
SET decision_id = $2,
    state_version = state_version + 1,
    updated_at = clock_timestamp()
WHERE id = $1
  AND decision_id IS NULL
  AND signed_submission_id IS NULL
  AND reservation_state = 'active'`

const crossMintAdvanceCustodySQL = `UPDATE loyal_yield.rebalance_decisions
SET custody_mint = $2,
    custody_amount_raw = $3,
    custody_account = $4,
    custody_observed_balance_raw = $5,
    custody_reconciled_slot = $6,
    custody_version = custody_version + 1,
    continuation_available_at = NULL,
    continuation_lease_owner = NULL,
    continuation_lease_expires_at = NULL,
    terminal_outcome = $7,
    terminal_evidence = $10,
    terminal_reason = $11,
    terminal_observed_slot = $12,
    status = CASE
        WHEN $7::text IS NULL THEN 'confirming'::loyal_yield.decision_status
        ELSE 'confirmed'::loyal_yield.decision_status
    END,
    signature = CASE WHEN $7::text IS NULL THEN signature ELSE $8 END,
    confirmed_slot = CASE WHEN $7::text IS NULL THEN confirmed_slot ELSE $6 END,
    updated_at = clock_timestamp()
WHERE id = $1
  AND movement_route = 'cross_mint_jupiter'
  AND status = 'confirming'::loyal_yield.decision_status
  AND terminal_outcome IS NULL
  AND custody_version = $9
RETURNING id`

const crossMintReceiptSQL = `UPDATE loyal_yield.signed_route_submissions
SET submission_state = 'reconciled',
    reconciled_slot = $4,
    reconciled_at = clock_timestamp(),
    reconciled_effect = $5,
    reconciled_balance_anchors = $6,
    effect_debit_mint = $7,
    effect_debit_account = $8,
    effect_debit_amount_raw = $9,
    effect_credit_mint = $10,
    effect_credit_account = $11,
    effect_credit_amount_raw = $12,
    confirmation_lease_owner = NULL,
    confirmation_lease_expires_at = NULL,
    error_detail = NULL,
    updated_at = clock_timestamp()
WHERE id = $1
  AND submission_state = 'reconciliation_pending'
  AND finalized_slot = $4
  AND finalized_at IS NOT NULL
  AND reconciled_effect IS NULL
  AND confirmation_lease_owner = $2
  AND confirmation_fencing_token = $3
  AND confirmation_lease_expires_at > clock_timestamp()
RETURNING id`

const crossMintFallbackGenerationSQL = `UPDATE loyal_yield.target_capacity_frontiers
SET reservation_generation = GREATEST(
        reservation_generation,
        $4
    ) + 1,
    updated_at = clock_timestamp()
WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
RETURNING reservation_generation`

const crossMintRebindCapacitySQL = `UPDATE loyal_yield.target_capacity_reservations
SET target_reserve = $2,
    admitted_observed_supply_usd_micros = $3,
    admitted_observed_slot = $4,
    admitted_maximum_inflight_usd_micros = $5,
    admitted_telemetry_version = $6,
    reservation_generation = $7,
    state_version = state_version + 1,
    updated_at = clock_timestamp()
WHERE decision_id = $1
  AND reservation_state = 'active'
RETURNING id`

const crossMintRebindDecisionSQL = `UPDATE loyal_yield.rebalance_decisions
SET active_target_reserve = $2,
    continuation_lease_owner = NULL,
    continuation_lease_expires_at = NULL,
    continuation_available_at = clock_timestamp(),
    updated_at = clock_timestamp()
WHERE id = $1
  AND continuation_lease_owner = $3
  AND continuation_fencing_token = $4
  AND continuation_lease_expires_at > clock_timestamp()`

const crossMintPolicyPayerSQL = `SELECT EXISTS (
    SELECT 1
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.managed_vaults vault
      ON vault.id = opportunity.vault_id
     AND vault.active
    JOIN loyal_yield.route_policies policy
      ON policy.id = vault.active_policy_id
     AND policy.active
    WHERE opportunity.id = $1
      AND opportunity.cluster = $2
      AND $3 = ANY(policy.delegated_signers)
      AND (
          (
              opportunity.execution_plan->>'kind' = 'voltr_kamino'
              AND opportunity.execution_plan->>'guardian' = $3
              AND NULLIF($4::jsonb->>'routeBundleSha256', '') =
                  opportunity.execution_plan->>'route_bundle_sha256'
              AND NULLIF($4::jsonb->>'lookupTable', '') IS NOT NULL
              AND NULLIF(
                  $4::jsonb->>'lookupTableOrderedAddressesSha256', ''
              ) IS NOT NULL
              AND ($4::jsonb->>'lookupTableAddressCount')::BIGINT > 0
          )
          OR (
              EXISTS (
                  SELECT 1 FROM loyal_yield.rebalance_decisions decision
                  WHERE decision.id = opportunity.decision_id
                    AND decision.movement_route = 'cross_mint_jupiter'
                    AND decision.terminal_outcome IS NULL
              )
              AND jsonb_array_length(COALESCE($4::jsonb -> 'tables', '[]'::jsonb)) = 0
              AND jsonb_array_length(COALESCE($4::jsonb -> 'externalSnapshots', '[]'::jsonb)) > 0
              AND jsonb_array_length(COALESCE($4::jsonb -> 'lookupTableOrder', '[]'::jsonb)) > 0
              AND jsonb_array_length(COALESCE($4::jsonb #> '{resolver,externalLookupTables}', '[]'::jsonb))
                  = jsonb_array_length($4::jsonb -> 'externalSnapshots')
          )
          OR (
              jsonb_array_length(
                  COALESCE($4::jsonb -> 'tables', '[]'::jsonb)
              ) > 0
              AND NOT EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements(
                      COALESCE($4::jsonb -> 'tables', '[]'::jsonb)
                  ) selected
                  LEFT JOIN loyal_yield.route_lookup_tables route_table
                    ON route_table.id = (selected ->> 'tableId')::BIGINT
                  LEFT JOIN loyal_yield.lookup_table_families family
                    ON family.id = route_table.family_id
                  WHERE route_table.id IS NULL
                     OR route_table.cluster <> $2
                     OR route_table.authority <> $3
                     OR route_table.payer <> $3
                     OR route_table.family_id IS NULL
                     OR family.id IS NULL
                     OR family.cluster <> $2
                     OR family.provisioning_authority <> $3
                     OR family.payer <> $3
              )
          )
      )
)`

const crossMintReserveCapacitySQL = `INSERT INTO loyal_yield.target_capacity_reservations
    (cluster, target_reserve, liquidity_mint, opportunity_id,
     principal_usd_micros, admitted_observed_supply_usd_micros,
     admitted_observed_slot, admitted_maximum_inflight_usd_micros,
     admitted_telemetry_version, reservation_generation,
     admitted_observed_target_apy_bps,
     admitted_projected_target_apy_bps, admitted_source_apy_bps,
     admitted_edge_bps, admitted_net_holding_gain_usd_micros,
     admitted_fee_cap_lamports, reservation_fencing_token)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
        $13, $14, $15, $16, $17)
RETURNING id`
