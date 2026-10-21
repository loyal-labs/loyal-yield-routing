package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type CrossMintFirstSendVerifier interface {
	VerifyCrossMintFirstSend(context.Context, CrossMintFirstSendRequest) error
}

type CrossMintFirstSendRequest struct {
	Movement           CrossMintMovement
	Submission         SubmissionRecord
	PolicyAccount      string
	ExpectedWireSHA256 string
	MinimumSlot        int64
	SelectedALTs       []fleet.ExecutionALT
	ExternalALTs       []fleet.CrossMintExternalALT
	LookupTableOrder   []string
}

type CrossMintActivationAdmission struct {
	Lease      fleet.RevalidationLease
	Activation CrossMintActivation
}

type CrossMintActivationSource interface {
	PrepareCrossMintActivation(context.Context, string) (*CrossMintActivationAdmission, error)
}

type CrossMintRuntime struct {
	config     Config
	store      *Store
	controller *CrossMintController
	accounts   fleet.AccountReader
	history    historyReader
	status     signatureReader
	receipts   receiptReader
	chain      chain.LandChain
	verifier   CrossMintFirstSendVerifier
	admission  CrossMintActivationSource
}

func NewCrossMintRuntime(ctx context.Context, config Config, store *Store, controller *CrossMintController, client *chain.Client, verifier CrossMintFirstSendVerifier) (*CrossMintRuntime, error) {
	if controller == nil || controller.store != store || controller.cluster != config.Cluster || controller.owner != config.Owner {
		return nil, errors.New("cross-mint runtime requires matching concrete owners, verifier and bounded whole-second lease")
	}
	runtime, err := NewCrossMintRecoveryRuntime(ctx, config, store, client, verifier)
	if err != nil {
		return nil, err
	}
	runtime.controller = controller
	return runtime, nil
}

func NewCrossMintRecoveryRuntime(ctx context.Context, config Config, store *Store, client *chain.Client, verifier CrossMintFirstSendVerifier) (*CrossMintRuntime, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if ctx == nil || store == nil || client == nil || verifier == nil || config.LeaseTTL < 10*time.Second || config.LeaseTTL > 300*time.Second || config.LeaseTTL%time.Second != 0 || config.BatchSize > 100 {
		return nil, errors.New("cross-mint recovery requires concrete owners, verifier and bounded whole-second lease")
	}
	return &CrossMintRuntime{config: config, store: store, accounts: client, history: client, status: client, receipts: client, chain: client, verifier: verifier}, nil
}

func (r *CrossMintRuntime) SetActivationSource(source CrossMintActivationSource) {
	r.admission = source
}

func (r *CrossMintRuntime) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.config.TickInterval)
	defer ticker.Stop()
	for {
		if _, err := r.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Print("cross-mint tick failed; recovery remains pending")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *CrossMintRuntime) Tick(ctx context.Context) (advanced int, err error) {
	defer func() {
		if err == nil {
			r.config.Facts.Progress(engine.FamilyFleet)
		}
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	leases, e := r.store.ClaimCrossMintRecoveryWork(ctx, r.config.Cluster, r.config.Owner, r.config.LeaseTTL, r.config.BatchSize)
	if e != nil {
		return 0, e
	}
	for _, l := range leases {
		e = r.handle(ctx, l)
		if errors.Is(e, chain.ErrBehind) {
			if e = r.store.releaseNotYet(ctx, l, 5*time.Second); e == nil {
				continue
			}
		}
		if e != nil {
			return advanced, fmt.Errorf("cross-mint submission %d: %w", l.Submission.ID, e)
		}
		advanced++
	}
	if len(leases) > 0 || r.controller == nil {
		return advanced, nil
	}
	_, worked, e := r.controller.ContinueOne(ctx)
	if e != nil {
		return advanced, e
	}
	if worked {
		return advanced + 1, nil
	}
	if r.admission == nil || !r.controller.swapEnabled {
		return advanced, nil
	}
	gates, e := r.store.CrossMintGates(ctx, r.config.Cluster)
	if e != nil {
		return advanced, e
	}
	if !gates.StartNewMovements || !gates.ContinueOrRecoverExisting {
		return advanced, nil
	}
	prepared, e := r.admission.PrepareCrossMintActivation(ctx, r.config.Cluster)
	if e != nil {
		return advanced, e
	}
	if prepared == nil {
		return advanced, nil
	}
	workCtx, cancel := context.WithDeadline(ctx, prepared.Lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	if _, e = r.store.ActivateCrossMintMovement(workCtx, prepared.Lease, prepared.Activation); e != nil {
		return advanced, e
	}
	return advanced + 1, nil
}

func (r *CrossMintRuntime) handle(ctx context.Context, l SubmissionLease) error {
	deadline, err := r.store.RenewClaimLease(ctx, l, r.config.LeaseTTL)
	if err != nil {
		return err
	}
	workCtx, cancel := context.WithDeadline(ctx, deadline.Add(-5*time.Second))
	defer cancel()
	if err = verifyDurableWire(l.Submission); err != nil {
		return r.hold(workCtx, l, "persisted_wire_invalid", err)
	}
	if l.Submission.State == StateReconciliationPending {
		return r.reconcile(workCtx, l)
	}
	out, err := chain.Observe(workCtx, r.chain, r.attempt(l))
	if err != nil {
		return err
	}
	switch {
	case out.Kind == chain.Landed:
		return r.store.markCrossMintFinalized(workCtx, l, int64(out.Slot))
	case out.Kind == chain.Failed && out.Commitment == chain.Finalized:
		return r.hold(workCtx, l, "finalized_failure_requires_manual_custody_proof", errors.New(out.Err))
	case out.Kind == chain.Failed:
		return r.store.deferCrossMintStatus(workCtx, l, nil, nil, "signature_seen_below_finalized", false)
	case out.Kind == chain.Expired:
		if l.Submission.EffectCheckSlot == nil || l.Submission.ExpiryObservedBlockHeight == nil {
			slot, height := int64(out.ContextSlot), int64(out.BlockHeight)
			return r.store.deferCrossMintStatus(workCtx, l, &slot, &height, "expiry_requires_finalized_custody_and_history", true)
		}
		proofOwner := crossMintRecovery{store: r.store, accounts: r.accounts, history: r.history, status: r.status}
		proof, e := proofOwner.inspect(workCtx, l.Submission, *l.Submission.EffectCheckSlot)
		if e != nil {
			return r.hold(workCtx, l, "expiry_custody_unproven_manual_hold", e)
		}
		return r.store.expireCrossMint(workCtx, l, proof)
	}
	if l.Submission.BroadcastCount != 0 {
		return r.land(workCtx, ctx, l, nil)
	}
	if l.Submission.State != StateSigned {
		return r.store.deferCrossMintStatus(workCtx, l, nil, nil, "unsent_leg_not_signed", false)
	}
	m, err := r.store.CrossMintMovement(workCtx, *l.Submission.DecisionID)
	if err != nil {
		return err
	}
	sendEvidence, err := r.store.crossMintFirstSendEvidence(workCtx, l, m)
	if err != nil {
		return err
	}
	if err = r.verifier.VerifyCrossMintFirstSend(workCtx, sendEvidence); err != nil {
		return r.hold(workCtx, l, "first_send_policy_unproven", err)
	}
	pre, err := parseCrossMintAnchors(l.Submission.ExpectedBalanceAnchors)
	if err != nil {
		return r.hold(workCtx, l, "first_send_anchor_invalid", err)
	}
	floor := sendEvidence.MinimumSlot
	historyAnchor := floor
	if m.CustodyReconciledSlot != nil {
		historyAnchor = *m.CustodyReconciledSlot
	}
	known := journalSignatures{}
	if m.CustodyReconciledSlot != nil {
		known, err = r.store.crossMintRecognizedSignatures(workCtx, m.DecisionID, historyAnchor)
		if err != nil {
			return err
		}
	}
	observed, _, err := observeCrossMintFirstSendBank(workCtx, r.accounts, r.history, m, pre, floor, historyAnchor, known)
	if err != nil || !sameCrossMintAnchorAmounts(pre, observed) {
		if err == nil {
			err = errors.New("signed prebalance or position changed")
		}
		return r.hold(workCtx, l, "first_send_custody_unproven", err)
	}
	return r.land(workCtx, ctx, l, &m)
}

func (r *CrossMintRuntime) attempt(l SubmissionLease) chain.Attempt {
	return chain.Attempt{Wire: l.Submission.SignedTransaction, Signature: l.Submission.Signature,
		LastValidBlockHeight: uint64(l.Submission.LastValidBlockHeight), Required: chain.Finalized}
}

func (r *CrossMintRuntime) land(workCtx, ctx context.Context, l SubmissionLease, first *CrossMintMovement) error {
	out, err := chain.Land(workCtx, r.chain, r.attempt(l), resendEvery, func(sendCtx context.Context) error {
		if first != nil {
			m := *first
			first = nil
			return r.store.recordCrossMintBroadcastIntent(sendCtx, l, m)
		}
		if err := r.store.recordCrossMintResend(sendCtx, l); err != nil {
			return err
		}
		l.Submission.State = StateSubmitted
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return err
	}
	switch out.Kind {
	case chain.Landed:
		if err = r.store.markCrossMintFinalized(workCtx, l, int64(out.Slot)); err == nil {
			r.config.Facts.Landed(engine.FamilyFleet)
		}
		return err
	case chain.Failed:
		const reason = "finalized_failure_requires_manual_custody_proof"
		if err = r.store.deferCrossMintStatus(workCtx, l, nil, nil, reason, true); err != nil {
			return err
		}
		r.config.Facts.Failed(engine.FamilyFleet, "transaction_failed")
		return fmt.Errorf("%s; custody and capacity retained: %s", reason, out.Err)
	default:
		height, slot := int64(out.BlockHeight), int64(out.ContextSlot)
		return r.store.deferCrossMintStatus(workCtx, l, &slot, &height, "expiry_requires_finalized_custody_and_history", true)
	}
}

func (s *Store) recordCrossMintResend(ctx context.Context, l SubmissionLease) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='submitted',submitted_at=COALESCE(submitted_at,clock_timestamp()),broadcast_count=broadcast_count+1,last_broadcast_at=clock_timestamp(),last_status_checked_at=clock_timestamp(),error_detail='broadcast_intent_persisted',updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp() AND broadcast_count>0 AND submission_state IN ('signed','submitted') AND movement_leg IN ('withdraw','swap','deposit')`, l.Submission.ID, l.Owner, l.FencingToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return nil
}

func (r *CrossMintRuntime) hold(ctx context.Context, l SubmissionLease, reason string, cause error) error {
	if errors.Is(cause, chain.ErrUnavailable) {
		return cause
	}
	if err := r.store.deferCrossMintStatus(ctx, l, nil, nil, reason, true); err != nil {
		return err
	}
	return fmt.Errorf("%s; custody and capacity retained: %w", reason, cause)
}

func (s *Store) ClaimCrossMintRecoveryWork(ctx context.Context, cluster, owner string, ttl time.Duration, limit int) ([]SubmissionLease, error) {
	if cluster == "" || owner == "" || ttl < 10*time.Second || ttl > 300*time.Second || ttl%time.Second != 0 || limit <= 0 || limit > 100 {
		return nil, errors.New("cross-mint recovery claim is not bounded")
	}
	leases := []SubmissionLease{}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting {
			return nil
		}
		rows, err := tx.Query(ctx, `WITH candidates AS (SELECT s.id FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id AND o.decision_id=d.id WHERE s.cluster=$1 AND d.movement_route='cross_mint_jupiter' AND d.terminal_outcome IS NULL AND s.movement_leg IN ('withdraw','swap','deposit') AND s.required_commitment='finalized' AND s.fee_payer_kind='policy' AND o.execution_plan->>'kind'='cross_mint_jupiter' AND s.submission_state IN ('signed','submitted','confirmed','reconciliation_pending','expiry_check_pending','effect_ambiguous') AND (s.confirmation_lease_owner IS NULL OR s.confirmation_lease_expires_at<clock_timestamp()) AND s.confirmation_available_at<=clock_timestamp() ORDER BY CASE s.submission_state WHEN 'effect_ambiguous' THEN 0 WHEN 'expiry_check_pending' THEN 1 WHEN 'reconciliation_pending' THEN 2 WHEN 'confirmed' THEN 3 WHEN 'submitted' THEN 4 ELSE 5 END,s.confirmation_available_at,s.id LIMIT $2 FOR UPDATE OF s SKIP LOCKED) UPDATE loyal_yield.signed_route_submissions s SET confirmation_lease_owner=$3,confirmation_lease_expires_at=clock_timestamp()+$4::interval,confirmation_fencing_token=confirmation_fencing_token+1,confirmation_attempt_count=confirmation_attempt_count+1 FROM candidates WHERE s.id=candidates.id RETURNING s.id,s.cluster,s.semantic_key,s.opportunity_id,s.decision_id,s.transaction_signature,s.message_hash,s.recent_blockhash,s.last_valid_block_height,s.fee_payer,s.movement_leg,s.leg_purpose,s.leg_generation,s.signed_transaction,s.submission_state,s.broadcast_count,s.submitted_slot,s.confirmed_slot,s.effect_check_slot,s.expiry_observed_block_height,s.expected_effect,s.expected_balance_anchors,s.conflict_account_keys,s.confirmation_fencing_token,s.confirmation_lease_expires_at,s.confirmation_attempt_count`, cluster, limit, owner, formatInterval(ttl))
		if err != nil {
			return err
		}
		for rows.Next() {
			l, e := scanSubmissionLease(rows, owner)
			if e != nil {
				rows.Close()
				return e
			}
			leases = append(leases, l)
		}
		err = rows.Err()
		rows.Close()
		return err
	})
	return leases, err
}

func deferredSubmissionState(sub SubmissionRecord, expiryObserved, ambiguous bool) SubmissionState {
	unsent := sub.State == StateSigned && sub.BroadcastCount == 0
	switch {
	case expiryObserved:
		return StateExpiryCheckPending
	case ambiguous && sub.State != StateReconciliationPending && !unsent:
		return StateEffectAmbiguous
	case !ambiguous && sub.State == StateSigned && sub.BroadcastCount > 0:
		return StateSubmitted
	}
	return sub.State
}

func (s *Store) deferCrossMintStatus(ctx context.Context, l SubmissionLease, floor, height *int64, reason string, ambiguous bool) error {
	if floor != nil && *floor <= 0 || height != nil && *height <= l.Submission.LastValidBlockHeight {
		return errors.New("expiry observation lacks positive slot or expired height")
	}
	if len(reason) > 512 {
		return errors.New("cross-mint operational reason exceeds bound")
	}
	next := deferredSubmissionState(l.Submission, floor != nil, ambiguous)
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions s SET submission_state=$4,error_detail=$5,effect_check_slot=COALESCE($6,effect_check_slot),expiry_observed_block_height=COALESCE($7,expiry_observed_block_height),submitted_at=CASE WHEN $4='submitted' THEN COALESCE(submitted_at,clock_timestamp()) ELSE submitted_at END,last_status_checked_at=clock_timestamp(),confirmation_available_at=clock_timestamp()+interval '5 seconds',confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp() AND transaction_signature=$8 AND submission_state=$9 AND movement_leg IN ('withdraw','swap','deposit') AND EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.id=s.decision_id AND d.movement_route='cross_mint_jupiter' AND d.terminal_outcome IS NULL)`, l.Submission.ID, l.Owner, l.FencingToken, string(next), reason, floor, height, l.Submission.Signature, string(l.Submission.State))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		if next == StateEffectAmbiguous {
			_, err = tx.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE submission_id=$1 AND writable_account_key LIKE 'fleet-shared-write-lane:%'`, l.Submission.ID)
		}
		return err
	})
}

func (s *Store) markCrossMintFinalized(ctx context.Context, l SubmissionLease, slot int64) error {
	if slot <= 0 {
		return errors.New("cross-mint finality lacks slot")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions s SET submission_state='reconciliation_pending',submitted_slot=COALESCE(submitted_slot,$4),submitted_at=COALESCE(submitted_at,last_broadcast_at,clock_timestamp()),confirmed_slot=$4,confirmed_at=COALESCE(confirmed_at,clock_timestamp()),finalized_slot=$4,finalized_at=COALESCE(finalized_at,clock_timestamp()),error_detail=NULL,last_status_checked_at=clock_timestamp(),confirmation_available_at=clock_timestamp(),confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp() AND transaction_signature=$5 AND submission_state=$6 AND required_commitment='finalized' AND movement_leg IN ('withdraw','swap','deposit') AND (finalized_slot IS NULL OR finalized_slot=$4) AND (confirmed_slot IS NULL OR confirmed_slot=$4) AND EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.id=s.decision_id AND d.movement_route='cross_mint_jupiter' AND d.terminal_outcome IS NULL)`, l.Submission.ID, l.Owner, l.FencingToken, slot, l.Submission.Signature, string(l.Submission.State))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		_, err = tx.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE submission_id=$1 AND (writable_account_key LIKE 'fleet-shared-write-lane:%' OR writable_account_key LIKE 'policy-setup-funding:%')`, l.Submission.ID)
		return err
	})
}

func (s *Store) crossMintFirstSendEvidence(ctx context.Context, l SubmissionLease, m CrossMintMovement) (CrossMintFirstSendRequest, error) {
	var out CrossMintFirstSendRequest
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		out, err = loadCrossMintSendEvidence(ctx, tx, l, m)
		return err
	})
	return out, err
}

func loadCrossMintSendEvidence(ctx context.Context, tx pgx.Tx, l SubmissionLease, m CrossMintMovement) (CrossMintFirstSendRequest, error) {
	out := CrossMintFirstSendRequest{Movement: m, Submission: l.Submission}
	var epochs json.RawMessage
	var selection string
	if err := tx.QueryRow(ctx, `SELECT policy_account,signed_transaction_hash,alt_mutation_epochs,alt_selection_fingerprint FROM loyal_yield.signed_route_submissions WHERE id=$1 AND decision_id=$2 AND opportunity_id=$3 AND transaction_signature=$4 AND signed_transaction=$5 AND submission_state='signed' AND broadcast_count=0 AND confirmation_lease_owner=$6 AND confirmation_fencing_token=$7 AND confirmation_lease_expires_at>clock_timestamp() AND fee_payer_kind='policy' AND required_commitment='finalized'`, l.Submission.ID, m.DecisionID, m.OpportunityID, l.Submission.Signature, l.Submission.SignedTransaction, l.Owner, l.FencingToken).Scan(&out.PolicyAccount, &out.ExpectedWireSHA256, &epochs, &selection); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrStaleOwner
		}
		return out, err
	}
	wireHash := sha256.Sum256(l.Submission.SignedTransaction)
	if out.ExpectedWireSHA256 != hex.EncodeToString(wireHash[:]) {
		return out, errors.New("durable signed wire hash differs from bytes")
	}
	bindings, err := crossMintBindings(m.ExecutionPlan)
	if err != nil {
		return out, err
	}
	slot := bindings.Withdraw.ObservedSlot
	if l.Submission.MovementLeg == LegSwap {
		slot = bindings.Swap.ObservedSlot
	} else if l.Submission.MovementLeg == LegDeposit && l.Submission.LegPurpose != PurposeRecoverSource {
		slot = bindings.Deposit.ObservedSlot
	}
	if slot == 0 || slot > math.MaxInt64 {
		return out, errors.New("first send policy clock is unknown or outside SQL range")
	}
	out.MinimumSlot = int64(slot)
	if m.CustodyReconciledSlot != nil && *m.CustodyReconciledSlot > out.MinimumSlot {
		out.MinimumSlot = *m.CustodyReconciledSlot
	}
	var selected struct {
		Tables []struct {
			TableID, MutationEpoch, FamilyID, Generation int64
			TableAddress                                 string
			BindingID                                    *int64
		}
	}
	if _, err = parsePreparedALTEpochs(epochs); err != nil {
		return out, err
	}
	if err = json.Unmarshal(epochs, &selected); err != nil {
		return out, err
	}
	for _, e := range selected.Tables {
		a := fleet.ExecutionALT{TableID: e.TableID, MutationEpoch: e.MutationEpoch, FamilyID: e.FamilyID, Generation: e.Generation, Address: e.TableAddress, BindingID: e.BindingID}
		var warm, verified *int64
		var complete *bool
		if err = tx.QueryRow(ctx, `SELECT array_agg(address ORDER BY ordinal),max(usable_after_slot),max(last_verified_slot),bool_and(last_verified_slot IS NOT NULL AND usable_after_slot IS NOT NULL) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, e.TableID).Scan(&a.Addresses, &warm, &verified, &complete); err != nil {
			return out, err
		}
		if warm == nil || verified == nil || complete == nil || !*complete || *verified <= 0 {
			return out, errors.New("durable ALT membership clock is unknown")
		}
		if *warm > out.MinimumSlot {
			out.MinimumSlot = *warm
		}
		if *verified > out.MinimumSlot {
			out.MinimumSlot = *verified
		}
		out.SelectedALTs = append(out.SelectedALTs, a)
	}
	external, order, _, err := crossMintJournalALTProof(epochs, out.SelectedALTs, l.Submission.SignedTransaction, selection)
	if err != nil {
		return out, err
	}
	out.ExternalALTs, out.LookupTableOrder = external, order
	for _, snapshot := range external {
		out.MinimumSlot = max(out.MinimumSlot, snapshot.ObservedSlot, snapshot.UsableAfterSlot)
	}
	return out, nil
}

func (s *Store) recordCrossMintBroadcastIntent(ctx context.Context, l SubmissionLease, m CrossMintMovement) error {
	if err := verifyDurableWire(l.Submission); err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCrossMintControl(ctx, tx, m.Cluster); err != nil {
			return err
		}
		g, err := readCrossMintGates(ctx, tx, m.Cluster, true)
		if err != nil {
			return err
		}
		if !g.ContinueOrRecoverExisting {
			return errors.New("cross-mint recovery authority is disabled")
		}
		var id int64
		if err = tx.QueryRow(ctx, `SELECT s.id FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id WHERE s.id=$1 AND s.decision_id=$2 AND s.confirmation_lease_owner=$3 AND s.confirmation_fencing_token=$4 AND s.confirmation_lease_expires_at>clock_timestamp() AND s.submission_state='signed' AND s.broadcast_count=0 AND s.transaction_signature=$5 AND s.signed_transaction=$6 AND d.movement_route='cross_mint_jupiter' AND d.status='confirming' AND d.terminal_outcome IS NULL FOR UPDATE OF s,d`, l.Submission.ID, m.DecisionID, l.Owner, l.FencingToken, l.Submission.Signature, l.Submission.SignedTransaction).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return err
		}
		actual, err := readCrossMintMovement(ctx, tx, m.DecisionID)
		if err != nil {
			return err
		}
		if !sameCrossMintCustody(m, actual) {
			return errors.New("movement custody changed during first-send verification")
		}
		evidence, err := loadCrossMintSendEvidence(ctx, tx, l, actual)
		if err != nil {
			return err
		}
		b, err := crossMintBindings(actual.ExecutionPlan)
		if err != nil {
			return err
		}
		policy := b.Deposit.PolicyAccount
		if l.Submission.MovementLeg == LegWithdraw || l.Submission.LegPurpose == PurposeRecoverSource {
			policy = b.Withdraw.PolicyAccount
		} else if l.Submission.MovementLeg == LegSwap {
			policy = b.Swap.PolicyAccount
		}
		if evidence.PolicyAccount != policy || l.Submission.FeePayer != b.DelegatedSigner {
			return errors.New("durable leg policy or payer changed")
		}
		if l.Submission.MovementLeg == LegWithdraw {
			if err = checkCrossMintInitialPolicies(ctx, tx, actual, b); err != nil {
				return err
			}
		}
		var epochs json.RawMessage
		var requirements string
		var fee, total, cap int64
		var writable []string
		if err = tx.QueryRow(ctx, `SELECT s.alt_mutation_epochs,s.alt_requirements_fingerprint,s.compiled_fee_lamports,s.writable_account_keys,o.estimated_cost_lamports,(SELECT sum(compiled_fee_lamports)::bigint FROM loyal_yield.signed_route_submissions WHERE decision_id=s.decision_id) FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id WHERE s.id=$1 FOR SHARE OF o`, id).Scan(&epochs, &requirements, &fee, &writable, &cap, &total); err != nil {
			return err
		}
		if fee <= 0 || total > cap || cap <= 0 {
			return errors.New("durable movement fees exceed immutable budget")
		}
		if err = checkPreparedALTUsage(ctx, tx, epochs, l.Submission.SemanticKey, m.Cluster, requirements); err != nil {
			return err
		}
		a := fleet.ExecutionAdmission{Lease: fleet.RevalidationLease{Cluster: m.Cluster, VaultID: m.VaultID}, SelectedALTs: evidence.SelectedALTs, Evidence: fleet.FreshRouteEvidence{Slot: evidence.MinimumSlot}}
		if err = lockAdmissionALTs(ctx, tx, a); err != nil {
			return err
		}
		transaction, err := sdk.TransactionFromBytes(l.Submission.SignedTransaction)
		if err != nil {
			return err
		}
		transaction.Signatures[0] = sdk.Signature{}
		unsigned, err := transaction.MarshalBinary()
		if err != nil {
			return err
		}
		p := CrossMintPreparedLeg{SelectedALTs: evidence.SelectedALTs, ExternalALTs: evidence.ExternalALTs}
		p.Preparation.Transaction = fleet.PreparedTransaction{UnsignedWire: unsigned, WritableAccounts: writable, LookupTables: evidence.LookupTableOrder}
		if err = verifyCrossMintPreparedWritables(transaction, p); err != nil {
			return err
		}
		var bound bool
		if err = tx.QueryRow(ctx, crossMintPolicyPayerSQL, m.OpportunityID, m.Cluster, l.Submission.FeePayer, epochs).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return errors.New("durable policy payer lost registered authority")
		}
		for _, key := range l.Submission.ConflictAccountKeys {
			var retained bool
			if err = tx.QueryRow(ctx, `SELECT submission_id=$3 FROM loyal_yield.route_account_conflict_leases WHERE cluster=$1 AND writable_account_key=$2 FOR UPDATE`, m.Cluster, key, id).Scan(&retained); err != nil {
				return err
			}
			if !retained {
				return ErrConflictLeaseHeld
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET broadcast_count=1,last_broadcast_at=clock_timestamp(),last_status_checked_at=clock_timestamp(),error_detail='broadcast_intent_persisted',updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp() AND broadcast_count=0 AND submission_state='signed'`, id, l.Owner, l.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

func sameCrossMintCustody(a, b CrossMintMovement) bool {
	return a.DecisionID == b.DecisionID && a.OpportunityID == b.OpportunityID && a.VaultPubkey == b.VaultPubkey && a.CustodyVersion == b.CustodyVersion && a.CustodyMint == b.CustodyMint && a.CustodyAccount == b.CustodyAccount && a.CustodyAmountRaw == b.CustodyAmountRaw && a.ActiveTargetReserve == b.ActiveTargetReserve && a.TerminalOutcome == nil && b.TerminalOutcome == nil && sameJSON(a.ExecutionPlan, b.ExecutionPlan) && sameJSON(a.PreflightCertification, b.PreflightCertification) && equalCrossMintInt(a.CustodyObservedBalanceRaw, b.CustodyObservedBalanceRaw) && equalCrossMintInt(a.CustodyReconciledSlot, b.CustodyReconciledSlot)
}

func equalCrossMintInt(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func sameCrossMintAnchorAmounts(a, b CrossMintBalanceAnchors) bool {
	for _, pair := range [][2]*CrossMintTokenAmount{{a.Debit, b.Debit}, {a.Credit, b.Credit}} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return false
		}
		if pair[0] != nil && *pair[0] != *pair[1] {
			return false
		}
	}
	if (a.Position == nil) != (b.Position == nil) {
		return false
	}
	if a.Position != nil {
		p, q := a.Position, b.Position
		return p.Reserve == q.Reserve && p.Market == q.Market && p.Obligation == q.Obligation && p.ObligationExists == q.ObligationExists && p.CollateralRaw == q.CollateralRaw
	}
	return true
}

type anchorPolicy struct {
	history bool
	custody string
}

var (
	anchorNoHistory  = anchorPolicy{}
	anchorEveryToken = anchorPolicy{history: true}
)

func anchorCustodyToken(account string) anchorPolicy {
	return anchorPolicy{history: true, custody: account}
}

func (p anchorPolicy) requiresReceipt(account string) bool {
	return p.custody == "" || account == p.custody
}

func observeCrossMintFirstSendBank(ctx context.Context, reader fleet.AccountReader, history historyReader, m CrossMintMovement, expected CrossMintBalanceAnchors, floor, historyAnchor int64, recognized journalSignatures) (CrossMintBalanceAnchors, int64, error) {
	if m.CustodyReconciledSlot == nil {
		return observeCrossMintBank(ctx, reader, history, m, expected, floor, historyAnchor, recognized, anchorNoHistory)
	}
	bound := false
	for _, anchor := range []*CrossMintTokenAmount{expected.Debit, expected.Credit} {
		if anchor != nil && anchor.TokenAccount == m.CustodyAccount && anchor.Mint == m.CustodyMint {
			bound = true
		}
	}
	if !bound || m.CustodyAccount == "" || (m.Phase != CrossMintSourceIdle && m.Phase != CrossMintTargetIdle) {
		return CrossMintBalanceAnchors{}, 0, errors.New("first-send history lacks the attributable idle custody account")
	}
	return observeCrossMintBank(ctx, reader, history, m, expected, floor, historyAnchor, recognized, anchorCustodyToken(m.CustodyAccount))
}

func observeCrossMintBank(ctx context.Context, reader fleet.AccountReader, history historyReader, m CrossMintMovement, expected CrossMintBalanceAnchors, floor, historyAnchor int64, recognized journalSignatures, policy anchorPolicy) (CrossMintBalanceAnchors, int64, error) {
	var out CrossMintBalanceAnchors
	if reader == nil || history == nil || floor <= 0 || policy.history && (historyAnchor <= 0 || historyAnchor > floor) {
		return out, 0, errors.New("cross-mint bank needs finalized proof dependencies and positive floor")
	}
	keys := []string{}
	for _, a := range []*CrossMintTokenAmount{expected.Debit, expected.Credit} {
		if a != nil {
			keys = append(keys, a.TokenAccount)
		}
	}
	if expected.Position != nil {
		keys = append(keys, expected.Position.Reserve, expected.Position.Obligation)
	}
	if len(keys) == 0 {
		return out, 0, errors.New("cross-mint bank has no signed accounts")
	}
	// A withdraw leg that empties the source closes its obligation inside the
	// route (KLend), so null accounts are returned; token anchors and the
	// reserve stay required by custodyTokenAmount and reservePostIdentity.
	slot, accounts, err := fleet.ReadAccounts(ctx, reader, keys, rpc.CommitmentFinalized, floor)
	if err != nil {
		return out, 0, err
	}
	byKey := map[string]*chain.Account{}
	for i, a := range accounts {
		if _, repeated := byKey[keys[i]]; repeated {
			return out, 0, errors.New("cross-mint bank contains wrong or repeated identity")
		}
		byKey[keys[i]] = a
	}
	for i, a := range []*CrossMintTokenAmount{expected.Debit, expected.Credit} {
		if a == nil {
			continue
		}
		amount, e := custodyTokenAmount(byKey[a.TokenAccount], a.Mint, m.VaultPubkey)
		if e != nil {
			return out, 0, e
		}
		program, e := canonicalCustodyTokenProgram(a.Mint)
		if e != nil {
			return out, 0, e
		}
		canonical, e := associatedCustodyAccount(m.VaultPubkey, a.Mint, program)
		if e != nil || canonical != a.TokenAccount {
			return out, 0, errors.New("signed token anchor is not canonical vault ATA")
		}
		anchor := &CrossMintTokenAmount{Mint: a.Mint, TokenAccount: a.TokenAccount, AmountRaw: amount}
		if i == 0 {
			out.Debit = anchor
		} else {
			out.Credit = anchor
		}
		if policy.history {
			if _, e = verifyCustodyHistory(ctx, history, a.TokenAccount, historyAnchor, slot, recognized, policy.requiresReceipt(a.TokenAccount)); e != nil {
				return out, 0, e
			}
		}
	}
	if p := expected.Position; p != nil {
		reserve := byKey[p.Reserve]
		mint := m.CustodyMint
		if m.Phase == CrossMintSourceReserve {
			mint = m.SourceMint
		}
		market, obligation, _, program, e := reservePostIdentity(reserve, mint, m.VaultPubkey)
		canonical, e2 := canonicalCustodyTokenProgram(mint)
		if e != nil || e2 != nil || market != p.Market || obligation != p.Obligation || program != canonical {
			return out, 0, errors.New("finalized position identity or token program changed")
		}
		account := byKey[p.Obligation]
		exists := account != nil
		collateral := int64(0)
		if exists {
			collateral, e = obligationCollateral(account, p.Market, m.VaultPubkey, p.Reserve)
			if e != nil {
				return out, 0, e
			}
		}
		decoded, e := kamino.DecodeReserve(reserve)
		var minimum uint64
		if e == nil {
			minimum, e = decoded.MinimumDeposit()
		}
		if e != nil || minimum == 0 || minimum > math.MaxInt64 {
			return out, 0, errors.New("finalized minimum-deposit conversion is unknown or out of range")
		}
		out.Position = &CrossMintPositionAnchor{Reserve: p.Reserve, Market: p.Market, Obligation: p.Obligation, ObligationExists: exists, CollateralRaw: collateral, MinimumDepositAmountRaw: crossRuntimeInt(int64(minimum))}
		if policy.history {
			if _, e = verifyCustodyHistory(ctx, history, p.Obligation, historyAnchor, slot, recognized, false); e != nil {
				return out, 0, e
			}
		}
	}
	return out, slot, nil
}

func crossRuntimeInt(v int64) *int64 { return &v }

func crossMintReceiptEffects(receipt chain.Receipt, m CrossMintMovement, pre CrossMintBalanceAnchors) (CrossMintEffect, CrossMintBalanceAnchors, error) {
	var effect CrossMintEffect
	var post CrossMintBalanceAnchors
	for i, anchor := range []*CrossMintTokenAmount{pre.Debit, pre.Credit} {
		if anchor == nil {
			continue
		}
		account, keyErr := sdk.PublicKeyFromBase58(anchor.TokenAccount)
		was, preKnown := receipt.Pre[account]
		is, postKnown := receipt.Post[account]
		program, err := canonicalCustodyTokenProgram(anchor.Mint)
		if err != nil || keyErr != nil || !preKnown || !postKnown || was.Mint.String() != anchor.Mint || is.Mint != was.Mint || was.Amount > math.MaxInt64 || is.Amount > math.MaxInt64 || int64(was.Amount) != anchor.AmountRaw || was.Owner.String() != m.VaultPubkey || is.Owner.String() != m.VaultPubkey || was.Program.String() != program || is.Program.String() != program {
			return effect, post, errors.New("receipt custody pre/post mint, owner, program or raw balance differs or is unknown")
		}
		before, after := int64(was.Amount), int64(is.Amount)
		amount := before - after
		if i == 1 {
			amount = after - before
		}
		if amount <= 0 {
			return effect, post, errors.New("receipt did not perform positive signed directional effect")
		}
		delta := &CrossMintTokenAmount{Mint: anchor.Mint, TokenAccount: anchor.TokenAccount, AmountRaw: amount}
		balance := &CrossMintTokenAmount{Mint: anchor.Mint, TokenAccount: anchor.TokenAccount, AmountRaw: after}
		if i == 0 {
			effect.Debit, post.Debit = delta, balance
		} else {
			effect.Credit, post.Credit = delta, balance
		}
	}
	return effect, post, nil
}

func (r *CrossMintRuntime) reconcile(ctx context.Context, l SubmissionLease) error {
	if l.Submission.ConfirmedSlot == nil {
		return r.hold(ctx, l, "finalized_slot_missing", errors.New("confirmed slot is unknown"))
	}
	receipt, err := finalizedReceipt(ctx, r.receipts, l.Submission.Signature)
	if errors.Is(err, chain.ErrNotFound) {
		return r.store.deferCrossMintStatus(ctx, l, nil, nil, "finalized_receipt_not_yet_available", false)
	}
	if err != nil {
		return err
	}
	slot := int64(receipt.Slot)
	if err = VerifyReceiptIdentity(receipt, l.Submission, *l.Submission.ConfirmedSlot); err != nil {
		return r.hold(ctx, l, "finalized_receipt_identity_invalid", err)
	}
	m, err := r.store.CrossMintMovement(ctx, *l.Submission.DecisionID)
	if err != nil {
		return err
	}
	pre, err := parseCrossMintAnchors(l.Submission.ExpectedBalanceAnchors)
	if err != nil {
		return r.hold(ctx, l, "finalized_signed_anchor_invalid", err)
	}
	if err = r.store.verifyCrossMintReceiptLoadedAccounts(ctx, l.Submission, receipt); err != nil {
		return r.hold(ctx, l, "finalized_lookup_index_unproven", err)
	}
	effect, metaPost, err := crossMintReceiptEffects(receipt, m, pre)
	if err != nil {
		return r.hold(ctx, l, "finalized_token_effect_invalid", err)
	}
	known, err := r.store.crossMintRecognizedSignatures(ctx, m.DecisionID, slot)
	if err != nil {
		return err
	}
	known.set[l.Submission.Signature] = true
	bank, _, err := observeCrossMintBank(ctx, r.accounts, r.history, m, pre, slot, slot, known, anchorEveryToken)
	if err != nil {
		return r.hold(ctx, l, "finalized_custody_history_unproven", err)
	}
	metaPost.Position = bank.Position
	if !sameCrossMintAnchorAmounts(metaPost, bank) {
		return r.hold(ctx, l, "finalized_aggregate_changed_after_receipt", errors.New("finalized account balance differs from receipt postbalance"))
	}
	_, err = r.store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: slot, Effect: effect, BalanceAnchors: bank})
	return err
}

func (s *Store) verifyCrossMintReceiptLoadedAccounts(ctx context.Context, record SubmissionRecord, receipt chain.Receipt) error {
	tx, err := sdk.TransactionFromBytes(record.SignedTransaction)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	var fingerprint string
	if err = s.pool.QueryRow(ctx, `SELECT alt_mutation_epochs,alt_selection_fingerprint FROM loyal_yield.signed_route_submissions WHERE id=$1 AND transaction_signature=$2 AND signed_transaction=$3`, record.ID, record.Signature, record.SignedTransaction).Scan(&raw, &fingerprint); err != nil {
		return err
	}
	var epochs struct {
		Tables []struct {
			TableID, MutationEpoch, FamilyID, Generation int64
			TableAddress                                 string
			BindingID                                    *int64
		}
	}
	if _, err = parsePreparedALTEpochs(raw); err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &epochs); err != nil {
		return err
	}
	selected := []fleet.ExecutionALT{}
	byAddress := map[string][]string{}
	for _, e := range epochs.Tables {
		a := fleet.ExecutionALT{TableID: e.TableID, MutationEpoch: e.MutationEpoch, FamilyID: e.FamilyID, Generation: e.Generation, Address: e.TableAddress, BindingID: e.BindingID}
		if err = s.pool.QueryRow(ctx, `SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, e.TableID).Scan(&a.Addresses); err != nil {
			return err
		}
		selected = append(selected, a)
		byAddress[a.Address] = a.Addresses
	}
	_, _, vectors, err := crossMintJournalALTProof(raw, selected, record.SignedTransaction, fingerprint)
	if err != nil {
		return err
	}
	byAddress = vectors
	keys := []string{}
	for _, key := range tx.Message.AccountKeys {
		keys = append(keys, key.String())
	}
	for _, lookup := range tx.Message.AddressTableLookups {
		if _, ok := byAddress[lookup.AccountKey.String()]; !ok {
			return errors.New("receipt lookup has no persisted registered identity")
		}
	}
	for _, slot := range fleet.LoadedLookupSlots(tx.Message.AddressTableLookups) {
		members := byAddress[slot.Table.String()]
		if int(slot.Index) >= len(members) {
			return errors.New("receipt lookup index exceeds original members")
		}
		keys = append(keys, members[slot.Index])
	}
	if len(receipt.Keys) != len(keys) {
		return errors.New("receipt account list differs from durable account index evidence")
	}
	for i, key := range receipt.Keys {
		if key.String() != keys[i] {
			return fmt.Errorf("receipt account %d differs from durable evidence", i)
		}
	}
	return nil
}
