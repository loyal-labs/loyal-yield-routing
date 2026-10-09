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

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// The root adapts C's concrete finalized policy verifier. This capability has
// no signer, send method or authority to alter the persisted transaction.
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

// CrossMintRuntime owns only the retained cross-mint family. Same-mint claims
// keep their existing owner; fresh activation comes last, after recovery and
// continuation, and still passes the atomic source-owned store admission.
type CrossMintRuntime struct {
	config     Config
	store      *Store
	controller *CrossMintController
	adapter    *RPCAdapter
	accounts   finalizedAccountReader
	history    finalizedHistoryReader
	status     StatusClient
	chain      solana.LandChain
	verifier   CrossMintFirstSendVerifier
	admission  CrossMintActivationSource
}

func NewCrossMintRuntime(ctx context.Context, config Config, store *Store, controller *CrossMintController, adapter *RPCAdapter, verifier CrossMintFirstSendVerifier) (*CrossMintRuntime, error) {
	if controller == nil || controller.store != store || controller.cluster != config.Cluster || controller.owner != config.Owner {
		return nil, errors.New("cross-mint runtime requires matching concrete owners, verifier and bounded whole-second lease")
	}
	runtime, err := NewCrossMintRecoveryRuntime(ctx, config, store, adapter, verifier)
	if err != nil {
		return nil, err
	}
	runtime.controller = controller
	return runtime, nil
}

// NewCrossMintRecoveryRuntime owns existing signed packets without a signing
// key. It cannot create a continuation or activation, even if a source is set.
func NewCrossMintRecoveryRuntime(ctx context.Context, config Config, store *Store, adapter *RPCAdapter, verifier CrossMintFirstSendVerifier) (*CrossMintRuntime, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if ctx == nil || store == nil || adapter == nil || verifier == nil || config.LeaseTTL < 10*time.Second || config.LeaseTTL > 300*time.Second || config.LeaseTTL%time.Second != 0 || config.BatchSize > 100 {
		return nil, errors.New("cross-mint recovery requires concrete owners, verifier and bounded whole-second lease")
	}
	chain, err := solana.NewLandRPC(adapter.url, adapter.deadline)
	if err != nil {
		return nil, err
	}
	return &CrossMintRuntime{config: config, store: store, adapter: adapter, accounts: fleet.NewRPCClient(adapter.url), history: adapter, status: adapter, chain: chain, verifier: verifier}, nil
}

// Configure before Run. The root retains dependency and pool lifecycles.
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
		if e = r.handle(ctx, l); e != nil {
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

// Health describes this configured owner, not just a successful SQL poll.
// Manual uncertainty, unsupported payers and foreign/expired ownership remain
// closed. RPC frontiers must cover every durable finalized custody anchor.
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
	// One height-first classification, shared with land(); expiry still
	// needs the custody proof before the leg may go terminal.
	out, err := solana.Observe(workCtx, r.chain, r.attempt(l))
	if err != nil {
		return err
	}
	switch {
	case out.Kind == solana.Landed:
		return r.store.markCrossMintFinalized(workCtx, l, int64(out.Slot))
	case out.Kind == solana.Failed && out.Commitment == solana.Finalized:
		return r.hold(workCtx, l, "finalized_failure_requires_manual_custody_proof", errors.New(out.Err))
	case out.Kind == solana.Failed:
		return r.store.deferCrossMintStatus(workCtx, l, nil, nil, "signature_seen_below_finalized", false)
	case out.Kind == solana.Expired:
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
	known := map[string]bool{}
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

func (r *CrossMintRuntime) attempt(l SubmissionLease) solana.Attempt {
	return solana.Attempt{Wire: l.Submission.SignedTransaction, Signature: l.Submission.Signature,
		LastValidBlockHeight: uint64(l.Submission.LastValidBlockHeight), Sends: l.Submission.BroadcastCount, Required: solana.Finalized}
}

// land resends the leg's exact bytes until they finalize or expire. The first
// send of an unsent leg is recorded with its full custody recheck; later sends
// only count. If the lease window ends first, the next claim lands again.
func (r *CrossMintRuntime) land(workCtx, ctx context.Context, l SubmissionLease, first *CrossMintMovement) error {
	out, err := solana.Land(workCtx, r.chain, r.attempt(l), resendEvery, func(sendCtx context.Context) error {
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
	case solana.Landed:
		if err = r.store.markCrossMintFinalized(workCtx, l, int64(out.Slot)); err == nil {
			r.config.Facts.Landed(engine.FamilyFleet)
		}
		return err
	case solana.Failed:
		const reason = "finalized_failure_requires_manual_custody_proof"
		if err = r.store.deferCrossMintStatus(workCtx, l, nil, nil, reason, true); err != nil {
			return err
		}
		r.config.Facts.Failed(engine.FamilyFleet, "transaction_failed")
		return fmt.Errorf("%s; custody and capacity retained: %s", reason, out.Err)
	default:
		// Not terminal: the custody proof decides; its row is counted there.
		height, slot := int64(out.BlockHeight), int64(out.ContextSlot)
		return r.store.deferCrossMintStatus(workCtx, l, &slot, &height, "expiry_requires_finalized_custody_and_history", true)
	}
}

// recordCrossMintResend counts one more send of an already-sent leg.
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
		if err != nil {
			return err
		}
		var dropped []SubmissionLease
		leases, dropped, err = renewConflictSet(ctx, tx, leases)
		logDroppedConflictSets(dropped)
		return err
	})
	return leases, err
}

// Nonterminal observations cannot release movement capacity or synthesize a
// receipt. The explicit backoff also prevents ambiguous work spinning forever.
func (s *Store) deferCrossMintStatus(ctx context.Context, l SubmissionLease, floor, height *int64, reason string, ambiguous bool) error {
	if floor != nil && *floor <= 0 || height != nil && *height <= l.Submission.LastValidBlockHeight {
		return errors.New("expiry observation lacks positive slot or expired height")
	}
	if len(reason) > 512 {
		return errors.New("cross-mint operational reason exceeds bound")
	}
	next := l.Submission.State
	if floor != nil {
		next = StateExpiryCheckPending
	} else if ambiguous && l.Submission.State != StateReconciliationPending && !(l.Submission.State == StateSigned && l.Submission.BroadcastCount == 0) {
		next = StateEffectAmbiguous
	} else if !ambiguous && l.Submission.State == StateSigned && l.Submission.BroadcastCount > 0 {
		next = StateSubmitted
	}
	// A first-send policy failure keeps signed bytes unsent, but eligible only
	// for recovery status; ambiguous custody never becomes a resend permission.
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
			if err = tx.QueryRow(ctx, `SELECT submission_id=$3 AND expires_at>clock_timestamp() FROM loyal_yield.route_account_conflict_leases WHERE cluster=$1 AND writable_account_key=$2 FOR UPDATE`, m.Cluster, key, id).Scan(&retained); err != nil {
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

// Observe every token anchor and the reserve/obligation pair in ONE finalized
// bank. History follows the balance observation, so restoration cannot hide
// an external debit. The source-backed decoder owns exchange-rate rounding.
func observeCrossMintBank(ctx context.Context, reader finalizedAccountReader, history finalizedHistoryReader, m CrossMintMovement, expected CrossMintBalanceAnchors, floor, historyAnchor int64, recognized map[string]bool, requireTokenAnchor bool) (CrossMintBalanceAnchors, int64, error) {
	return observeCrossMintBankWithAnchorPolicy(ctx, reader, history, m, expected, floor, historyAnchor, recognized, requireTokenAnchor, "")
}

// A fresh swap has not yet touched its destination ATA. Its existing custody
// account must retain the reconciled receipt anchor; other exact token anchors
// must have no unrecognized activity, but need no invented prior receipt.
// Receipt and expiry proofs retain the stricter all-account anchor policy.
func observeCrossMintFirstSendBank(ctx context.Context, reader finalizedAccountReader, history finalizedHistoryReader, m CrossMintMovement, expected CrossMintBalanceAnchors, floor, historyAnchor int64, recognized map[string]bool) (CrossMintBalanceAnchors, int64, error) {
	if m.CustodyReconciledSlot == nil {
		return observeCrossMintBank(ctx, reader, history, m, expected, floor, historyAnchor, recognized, false)
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
	return observeCrossMintBankWithAnchorPolicy(ctx, reader, history, m, expected, floor, historyAnchor, recognized, true, m.CustodyAccount)
}

func observeCrossMintBankWithAnchorPolicy(ctx context.Context, reader finalizedAccountReader, history finalizedHistoryReader, m CrossMintMovement, expected CrossMintBalanceAnchors, floor, historyAnchor int64, recognized map[string]bool, requireTokenAnchor bool, custodyOnly string) (CrossMintBalanceAnchors, int64, error) {
	var out CrossMintBalanceAnchors
	if reader == nil || history == nil || floor <= 0 || requireTokenAnchor && (historyAnchor <= 0 || historyAnchor > floor) {
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
	slot, accounts, err := reader.FinalizedAccountsAllowingAbsent(ctx, keys, floor)
	if err != nil {
		return out, 0, err
	}
	if slot < floor || len(accounts) != len(keys) {
		return out, 0, errors.New("cross-mint coherent bank is incomplete or stale")
	}
	byKey := map[string]fleet.Account{}
	for i, a := range accounts {
		if a.Address != keys[i] || byKey[a.Address].Address != "" {
			return out, 0, errors.New("cross-mint bank contains wrong or repeated identity")
		}
		byKey[a.Address] = a
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
		if requireTokenAnchor {
			if _, e = verifyCustodyHistory(ctx, history, a.TokenAccount, historyAnchor, slot, recognized, custodyOnly == "" || a.TokenAccount == custodyOnly); e != nil {
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
		exists := accountExists(account)
		collateral := int64(0)
		if exists {
			collateral, e = obligationCollateral(account, p.Market, m.VaultPubkey, p.Reserve)
			if e != nil {
				return out, 0, e
			}
		}
		minimum, e := backyard.KaminoMinimumDepositAmount(backyard.ConfirmedAccount{Address: reserve.Address, Owner: reserve.Owner, Lamports: reserve.Lamports, Data: reserve.Data, Executable: reserve.Executable}, market, mint)
		if e != nil || minimum == 0 || minimum > math.MaxInt64 {
			return out, 0, errors.New("finalized minimum-deposit conversion is unknown or out of range")
		}
		out.Position = &CrossMintPositionAnchor{Reserve: p.Reserve, Market: p.Market, Obligation: p.Obligation, ObligationExists: exists, CollateralRaw: collateral, MinimumDepositAmountRaw: crossRuntimeInt(int64(minimum))}
		if requireTokenAnchor {
			if _, e = verifyCustodyHistory(ctx, history, p.Obligation, historyAnchor, slot, recognized, false); e != nil {
				return out, 0, e
			}
		}
	}
	return out, slot, nil
}

func crossRuntimeInt(v int64) *int64 { return &v }

func crossMintReceiptEffects(receipt *TransactionReceipt, m CrossMintMovement, pre CrossMintBalanceAnchors) (CrossMintEffect, CrossMintBalanceAnchors, error) {
	var effect CrossMintEffect
	var post CrossMintBalanceAnchors
	if receipt == nil {
		return effect, post, errors.New("cross-mint receipt is absent")
	}
	for i, anchor := range []*CrossMintTokenAmount{pre.Debit, pre.Credit} {
		if anchor == nil {
			continue
		}
		var match *TokenDelta
		for n := range receipt.TokenDeltas {
			d := &receipt.TokenDeltas[n]
			if d.Account == anchor.TokenAccount {
				if match != nil {
					return effect, post, errors.New("receipt repeats anchored token account")
				}
				match = d
			}
		}
		program, err := canonicalCustodyTokenProgram(anchor.Mint)
		if err != nil || match == nil || match.Mint != anchor.Mint || match.PreRaw == nil || match.PostRaw == nil || *match.PreRaw > math.MaxInt64 || *match.PostRaw > math.MaxInt64 || int64(*match.PreRaw) != anchor.AmountRaw || match.PreOwner != m.VaultPubkey || match.PostOwner != m.VaultPubkey || match.PreProgram != program || match.PostProgram != program {
			return effect, post, errors.New("receipt custody pre/post mint, owner, program or raw balance differs or is unknown")
		}
		before, after := int64(*match.PreRaw), int64(*match.PostRaw)
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
	receipt, err := r.status.FinalizedTransaction(ctx, l.Submission.Signature)
	if err != nil {
		return err
	}
	if receipt == nil {
		return r.store.deferCrossMintStatus(ctx, l, nil, nil, "finalized_receipt_not_yet_available", false)
	}
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
	known, err := r.store.crossMintRecognizedSignatures(ctx, m.DecisionID, receipt.Slot)
	if err != nil {
		return err
	}
	known[l.Submission.Signature] = true // Exact finalized wire just verified above.
	bank, _, err := observeCrossMintBank(ctx, r.accounts, r.history, m, pre, receipt.Slot, receipt.Slot, known, true)
	if err != nil {
		return r.hold(ctx, l, "finalized_custody_history_unproven", err)
	}
	metaPost.Position = bank.Position
	if !sameCrossMintAnchorAmounts(metaPost, bank) {
		return r.hold(ctx, l, "finalized_aggregate_changed_after_receipt", errors.New("finalized account balance differs from receipt postbalance"))
	}
	_, err = r.store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: receipt.Slot, Effect: effect, BalanceAnchors: bank})
	return err
}

func (s *Store) verifyCrossMintReceiptLoadedAccounts(ctx context.Context, record SubmissionRecord, receipt *TransactionReceipt) error {
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
	for _, writable := range []bool{true, false} {
		for _, lookup := range tx.Message.AddressTableLookups {
			members, ok := byAddress[lookup.AccountKey.String()]
			if !ok {
				return errors.New("receipt lookup has no persisted registered identity")
			}
			indexes := lookup.ReadonlyIndexes
			if writable {
				indexes = lookup.WritableIndexes
			}
			for _, index := range indexes {
				if int(index) >= len(members) {
					return errors.New("receipt lookup index exceeds original members")
				}
				keys = append(keys, members[index])
			}
		}
	}
	_, err = receipt.WithAccountAddresses(keys)
	return err
}
