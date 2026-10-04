package autodeposit

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type desiredRequest struct {
	TargetID, Revision, Generation int64
	RequestedAt                    time.Time
	AttemptCount                   int
}
type desiredTarget struct {
	TargetID, Revision, SetupGeneration, MinimumSlot int64
	ChainStatus                                      string
	ChainSlot                                        int64
	Active, Eligible                                 bool
	Floor                                            *int64
	Artifact                                         *ArtifactTarget
}

var ErrDesiredControlsPending = errors.New("autodeposit desired controls remain unapplied")

func desiredAdmissionApplied(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	var ready bool
	err := tx.QueryRow(ctx, `SELECT cluster='mainnet-beta' AND desired_revision=applied_desired_revision AND applied_scheduling_eligible
AND NOT EXISTS(SELECT 1 FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1 AND requested_generation>processed_generation)
FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&ready)
	return ready, err
}

func (s *Store) checkUnsignedDesiredAdmission(ctx context.Context, targetID int64, claim, lease string) error {
	if !s.requireDesiredAdmission {
		return nil
	}
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, targetID).Scan(&id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT target_id FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1 AND target_id=$2 AND status='selected' AND autodeposit_executor_lease_token=$3 AND autodeposit_executor_lease_expires_at>now() FOR UPDATE`, claim, targetID, lease).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return ErrOwnershipLost
		} else if err != nil {
			return err
		}
		ready, err := desiredAdmissionApplied(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if !ready {
			return ErrDesiredControlsPending
		}
		return nil
	})
}

const schedulingEligibilitySQL = `target.cluster='mainnet-beta' AND target.desired_active AND target.chain_status='active' AND target.wallet_balance_floor_raw IS NOT NULL
AND EXISTS(SELECT 1 FROM loyal_yield.managed_vaults mv JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id
WHERE mv.active AND mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey
AND rp.active AND rp.cluster='mainnet-beta' AND rp.authority=target.authority AND rp.settings=target.settings AND rp.vault_index=target.vault_index AND rp.vault_pubkey=target.vault_pubkey AND 'same_mint_kamino'=ANY(rp.route_modes))
AND EXISTS(SELECT 1 FROM loyal_yield.user_yield_positions yp WHERE yp.settings=target.settings AND yp.vault_index=target.vault_index AND yp.wallet_address=target.wallet AND yp.status='active' AND yp.current_liquidity_mint=target.token_mint AND NULLIF(yp.current_reserve,'') IS NOT NULL AND NULLIF(yp.current_market,'') IS NOT NULL)`

func (s *Store) RequireDesiredSchema(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("desired schema check requires a store")
	}
	rows, err := s.pool.Query(ctx, `SELECT target.desired_revision,target.applied_desired_revision,target.applied_scheduling_eligible,request.requested_revision,request.processed_revision,request.requested_generation,request.processed_generation,request.requested_at FROM loyal_yield.balance_sweep_targets target LEFT JOIN loyal_yield.autodeposit_desired_control_requests request ON request.target_id=target.id LIMIT 0`)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

func (s *Store) enqueueSchedulingEligibilityChanges(ctx context.Context, limit int64) error {
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH changed AS(SELECT target.id,target.desired_revision FROM loyal_yield.balance_sweep_targets target
LEFT JOIN loyal_yield.autodeposit_desired_control_requests request ON request.target_id=target.id
WHERE target.cluster='mainnet-beta' AND target.token_mint=$1 AND target.chain_status<>'closed' AND (
 (target.applied_scheduling_eligible IS DISTINCT FROM (`+schedulingEligibilitySQL+`) AND (request.target_id IS NULL OR request.requested_generation=request.processed_generation))
 OR ((`+schedulingEligibilitySQL+`) AND request.requested_generation>request.processed_generation AND request.attempt_count>0
 AND (EXISTS(SELECT 1 FROM loyal_yield.user_yield_positions yp WHERE yp.settings=target.settings AND yp.vault_index=target.vault_index AND yp.wallet_address=target.wallet AND yp.status='active' AND yp.current_liquidity_mint=target.token_mint AND yp.updated_at>request.requested_at)
 OR EXISTS(SELECT 1 FROM loyal_yield.managed_vaults mv JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id WHERE mv.active AND mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey AND rp.active AND rp.cluster='mainnet-beta' AND GREATEST(mv.last_seen_at,rp.last_seen_at)>request.requested_at))))
ORDER BY target.id LIMIT $2 FOR UPDATE OF target SKIP LOCKED)
INSERT INTO loyal_yield.autodeposit_desired_control_requests(target_id,requested_revision) SELECT id,desired_revision FROM changed
ON CONFLICT(target_id) DO UPDATE SET requested_revision=EXCLUDED.requested_revision,requested_generation=loyal_yield.autodeposit_desired_control_requests.requested_generation+1,requested_at=clock_timestamp(),next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()`, USDCMint, limit)
		return err
	})
}

func (s *Store) claimDesiredRequest(ctx context.Context, owner string, seconds int64) (*desiredRequest, error) {
	if owner == "" || seconds <= 0 {
		return nil, errors.New("desired claim requires owner and positive lease")
	}
	var request desiredRequest
	err := s.pool.QueryRow(ctx, `WITH candidate AS(SELECT target_id FROM loyal_yield.autodeposit_desired_control_requests WHERE EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.id=target_id AND target.cluster='mainnet-beta') AND requested_generation>processed_generation AND next_attempt_at<=now() AND (claim_expires_at IS NULL OR claim_expires_at<=now()) ORDER BY requested_at,target_id LIMIT 1 FOR UPDATE SKIP LOCKED)
UPDATE loyal_yield.autodeposit_desired_control_requests request SET claim_owner=$1,claim_expires_at=now()+($2::bigint*interval '1 second'),attempt_count=attempt_count+1,updated_at=now() FROM candidate WHERE request.target_id=candidate.target_id
RETURNING request.target_id,request.requested_revision,request.requested_generation,request.requested_at,request.attempt_count`, owner, seconds).Scan(&request.TargetID, &request.Revision, &request.Generation, &request.RequestedAt, &request.AttemptCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &request, err
}

func (s *Store) loadDesiredTarget(ctx context.Context, id int64) (*desiredTarget, error) {
	var target desiredTarget
	err := s.pool.QueryRow(ctx, `SELECT target.id,target.desired_revision,target.setup_generation,target.desired_active,target.wallet_balance_floor_raw,(`+schedulingEligibilitySQL+`),GREATEST(target.chain_observation_slot,COALESCE((SELECT observed_slot FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=target.id AND mint=target.token_mint),0)),target.chain_status,target.chain_observation_slot FROM loyal_yield.balance_sweep_targets target WHERE target.id=$1 AND target.cluster='mainnet-beta' AND target.token_mint=$2`, id, USDCMint).Scan(&target.TargetID, &target.Revision, &target.SetupGeneration, &target.Active, &target.Floor, &target.Eligible, &target.MinimumSlot, &target.ChainStatus, &target.ChainSlot)
	if err != nil {
		return nil, err
	}
	if target.Active {
		target.Artifact, err = s.LoadArtifactTarget(ctx, id)
	}
	return &target, err
}

func (s *Store) retryDesiredRequest(ctx context.Context, request desiredRequest, owner string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.autodeposit_desired_control_requests SET claim_owner=NULL,claim_expires_at=NULL,last_error='desired control proof unavailable',next_attempt_at=CASE WHEN requested_generation>$3 THEN now() ELSE now()+($4::bigint*interval '1 second') END,updated_at=now() WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now()`, request.TargetID, owner, request.Generation, ReconciliationRetryBackoffSeconds(15, request.AttemptCount))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrOwnershipLost
	}
	return nil
}

func (s *Store) desiredRuntimeHealth(ctx context.Context, chain ConfirmedSlotReader) (uint64, error) {
	if chain == nil {
		return 0, errRuntimeProofUnavailable
	}
	slot, err := chain.ConfirmedSlot(ctx)
	if err != nil || slot <= 0 {
		return 0, errRuntimeProofUnavailable
	}
	var pending bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.autodeposit_desired_control_requests WHERE EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.id=target_id AND target.cluster='mainnet-beta') AND requested_generation>processed_generation)
OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.cluster='mainnet-beta' AND target.chain_status<>'closed' AND target.token_mint=$1 AND (target.desired_revision<>target.applied_desired_revision OR (`+mainnetSourceAheadSQL+`) OR target.applied_scheduling_eligible IS DISTINCT FROM (`+schedulingEligibilitySQL+`)))`, USDCMint, slot).Scan(&pending)
	if err != nil || pending {
		return 0, errRuntimeProofUnavailable
	}
	return uint64(slot), nil
}

func (s *Store) applyDesiredObservation(ctx context.Context, request desiredRequest, owner string, expected desiredTarget, o ControlObservation, observedAt time.Time, proofs []VerifiedArtifactCreationProof) error {
	if request.Revision != expected.Revision || request.TargetID != expected.TargetID || request.RequestedAt.IsZero() {
		return errors.New("desired proof revision or source clock invalid")
	}
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var revision, setup int64
		var chainStatus string
		var active bool
		var floor *int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision,setup_generation,desired_active,wallet_balance_floor_raw,chain_status FROM loyal_yield.balance_sweep_targets WHERE id=$1 AND cluster='mainnet-beta' FOR UPDATE`, request.TargetID).Scan(&revision, &setup, &active, &floor, &chainStatus); err != nil {
			return err
		}
		var capturedRevision, capturedGeneration int64
		if err := tx.QueryRow(ctx, `SELECT requested_revision,requested_generation FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now() FOR UPDATE`, request.TargetID, owner).Scan(&capturedRevision, &capturedGeneration); errors.Is(err, pgx.ErrNoRows) {
			return ErrOwnershipLost
		} else if err != nil {
			return err
		}
		if revision != request.Revision || revision != expected.Revision || setup != expected.SetupGeneration || active != expected.Active || chainStatus != expected.ChainStatus || !reflect.DeepEqual(floor, expected.Floor) || capturedRevision != request.Revision || capturedGeneration != request.Generation {
			return errors.New("desired revision changed during proof")
		}
		claims, err := tx.Query(ctx, `SELECT claim_token FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1 ORDER BY claim_token FOR UPDATE`, request.TargetID)
		if err != nil {
			return err
		}
		for claims.Next() {
		}
		err = claims.Err()
		claims.Close()
		if err != nil {
			return err
		}
		if active && chainStatus == "closed" && expected.ChainSlot <= 0 {
			return errRuntimeProofUnavailable
		}
		if active && chainStatus != "closed" {
			if floor == nil || *floor < 0 || expected.Artifact == nil || o.ObservedSlot <= 0 || o.ObservedSlot < expected.MinimumSlot || o.WalletBalanceRaw < 0 || !sha256HexPattern.MatchString(o.WalletAccountDataSHA256) || o.status() != "active" || observedAt.IsZero() {
				return errRuntimeProofUnavailable
			}
			current, err := scanArtifactTarget(tx.QueryRow(ctx, `SELECT `+artifactTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE id=$1`, request.TargetID))
			if err != nil {
				return err
			}
			if !sameArtifactIdentity(current, *expected.Artifact) || !reflect.DeepEqual(o.Target, current.ControlTarget) {
				return errors.New("desired chain identity changed during proof")
			}
			if err = s.lockDesiredEligibility(ctx, tx, request.TargetID, o.ObservedSlot); err != nil {
				return err
			}
			for _, role := range []ArtifactRole{ArtifactPolicy, ArtifactDelegation} {
				signature, slot := current.PolicySignature, current.PolicyConfirmedSlot
				if role == ArtifactDelegation {
					signature, slot = current.DelegationSignature, current.DelegationConfirmedSlot
				}
				if signature != nil && *signature != "" && slot != nil && *slot > 0 {
					continue
				}
				var proof *VerifiedArtifactCreationProof
				for i := range proofs {
					if proofs[i].role == role {
						proof = &proofs[i]
						break
					}
				}
				if proof == nil || !sameArtifactIdentity(proof.target, current) || proof.signature == "" || proof.slot <= 0 || !sha256HexPattern.MatchString(proof.instructionSHA256) {
					return ErrArtifactCreationProofPending
				}
				account := current.Policy
				if role == ArtifactDelegation {
					account = current.RecurringDelegation
				}
				if proof.account != account || signature != nil && *signature != proof.signature || slot != nil && *slot != proof.slot {
					return errors.New("desired creator contradicts retained stage proof")
				}
				query := `UPDATE loyal_yield.balance_sweep_targets SET policy_signature=COALESCE(policy_signature,$2),policy_confirmed_slot=COALESCE(policy_confirmed_slot,$3) WHERE id=$1`
				if role == ArtifactDelegation {
					query = `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation_signature=COALESCE(recurring_delegation_signature,$2),recurring_delegation_confirmed_slot=COALESCE(recurring_delegation_confirmed_slot,$3) WHERE id=$1`
				}
				if _, err = tx.Exec(ctx, query, request.TargetID, proof.signature, proof.slot); err != nil {
					return err
				}
			}
			var held bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1 AND (status='selected' OR autodeposit_executor_lease_expires_at>now())) OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts a JOIN loyal_yield.balance_sweep_lot_claims c ON c.claim_token=a.claim_token WHERE c.target_id=$1 AND c.status<>'executed' AND a.attempt_state=ANY($2::text[])) OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_destination_setup_attempts setup JOIN loyal_yield.balance_sweep_lot_claims c ON c.claim_token=setup.claim_token WHERE c.target_id=$1 AND setup.attempt_state IN('prepared','submitted','unknown','ambiguous'))`, request.TargetID, ClaimHoldingPullAttemptStates).Scan(&held); err != nil {
				return err
			}
			if held {
				return ErrClaimCustodyHeld
			}
			var currentSlot, currentAmount int64
			var currentHash *string
			var commitment string
			err = tx.QueryRow(ctx, `SELECT observed_slot,amount_raw,account_data_hash,source_commitment FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2 FOR UPDATE`, request.TargetID, USDCMint).Scan(&currentSlot, &currentAmount, &currentHash, &commitment)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if currentSlot > o.ObservedSlot {
				return errors.New("desired wallet proof behind projection")
			}
			if currentSlot == o.ObservedSlot && (commitment == "confirmed" || commitment == "finalized") && (currentAmount != o.WalletBalanceRaw || currentHash != nil && *currentHash != o.WalletAccountDataSHA256) {
				return errors.New("desired wallet proof contradicts same-slot projection")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balances_current(target_id,wallet,wallet_usdc_ata,wallet_token_ata,amount_raw,owner,mint,observed_slot,observed_at,source,source_commitment,account_data_hash,raw_evidence,updated_at)
VALUES($1,$2,$3,$3,$4,$2,$5,$6,$7,'go_autodeposit_desired_snapshot','confirmed',$8,jsonb_build_object('desiredRevision',$9::bigint,'observationClock','rpc_read'),now())
ON CONFLICT(target_id,mint) DO UPDATE SET wallet=EXCLUDED.wallet,wallet_usdc_ata=EXCLUDED.wallet_usdc_ata,wallet_token_ata=EXCLUDED.wallet_token_ata,amount_raw=EXCLUDED.amount_raw,owner=EXCLUDED.owner,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,source=EXCLUDED.source,source_commitment=EXCLUDED.source_commitment,account_data_hash=EXCLUDED.account_data_hash,raw_evidence=EXCLUDED.raw_evidence,updated_at=now()
WHERE loyal_yield.balance_sweep_wallet_balances_current.observed_slot<EXCLUDED.observed_slot OR loyal_yield.balance_sweep_wallet_balances_current.source_commitment<>'finalized'`, request.TargetID, current.Wallet, current.WalletTokenATA, o.WalletBalanceRaw, USDCMint, o.ObservedSlot, observedAt, o.WalletAccountDataSHA256, request.Revision); err != nil {
				return err
			}
		}
		if err = s.rebaselineDesiredLots(ctx, tx, request, expected, o, observedAt); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET applied_desired_revision=$2,applied_scheduling_eligible=$3 WHERE id=$1`, request.TargetID, request.Revision, active && chainStatus != "closed"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.autodeposit_desired_control_requests SET processed_revision=GREATEST(processed_revision,$3),processed_generation=GREATEST(processed_generation,$4),claim_owner=NULL,claim_expires_at=NULL,attempt_count=0,last_error=NULL,updated_at=now() WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now()`, request.TargetID, owner, request.Revision, request.Generation)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrOwnershipLost
		}
		return nil
	})
}

func (s *Store) lockDesiredEligibility(ctx context.Context, tx pgx.Tx, id, slot int64) error {
	var position int64
	err := tx.QueryRow(ctx, `SELECT yp.id FROM loyal_yield.balance_sweep_targets target
JOIN loyal_yield.managed_vaults mv ON mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey AND mv.active
JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id AND rp.active AND rp.cluster='mainnet-beta' AND rp.authority=target.authority AND rp.settings=target.settings AND rp.vault_index=target.vault_index AND rp.vault_pubkey=target.vault_pubkey AND 'same_mint_kamino'=ANY(rp.route_modes)
JOIN loyal_yield.user_yield_positions yp ON yp.settings=target.settings AND yp.vault_index=target.vault_index AND yp.wallet_address=target.wallet AND yp.status='active' AND yp.current_liquidity_mint=target.token_mint
WHERE target.id=$1 AND target.cluster='mainnet-beta' AND target.chain_status='active' AND rp.last_seen_slot<=$2 AND yp.current_observed_slot<=$2 AND NULLIF(yp.current_reserve,'') IS NOT NULL AND NULLIF(yp.current_market,'') IS NOT NULL ORDER BY yp.updated_at DESC,yp.id DESC LIMIT 1 FOR SHARE OF mv,rp,yp`, id, slot).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return errRuntimeProofUnavailable
	}
	return err
}

func (s *Store) rebaselineDesiredLots(ctx context.Context, tx pgx.Tx, request desiredRequest, target desiredTarget, o ControlObservation, observedAt time.Time) error {
	// Preserve the latest coalesced deadline before suppressing unheld lots.
	var due time.Time
	if err := tx.QueryRow(ctx, `SELECT GREATEST($2::timestamptz+interval '1 hour',COALESCE(max(slot.eligible_after),$2::timestamptz+interval '1 hour')) FROM loyal_yield.balance_sweep_scheduled_slots slot WHERE slot.target_id=$1 AND slot.token_mint=$3 AND (slot.status IN('scheduled','requested') OR (slot.status IN('failed','released') AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots lot WHERE lot.scheduled_slot_id=slot.id AND lot.status='open' AND lot.remaining_amount_raw>0)))`, request.TargetID, request.RequestedAt, USDCMint).Scan(&due); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_surplus_lots lot SET status='suppressed',updated_at=now() WHERE lot.target_id=$1 AND lot.status='open' AND lot.remaining_amount_raw>0 AND NOT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claim_items item JOIN loyal_yield.balance_sweep_lot_claims claim ON claim.claim_token=item.claim_token WHERE item.lot_id=lot.id AND (claim.status='selected' OR claim.autodeposit_executor_lease_expires_at>now() OR (claim.status<>'executed' AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts a WHERE a.claim_token=claim.claim_token AND a.attempt_state=ANY($2::text[])))))`, request.TargetID, ClaimHoldingPullAttemptStates)
	if err != nil {
		return err
	}
	if !target.Active || target.ChainStatus == "closed" {
		return nil
	}
	if target.Artifact.StartTimestamp != nil {
		start := time.Unix(*target.Artifact.StartTimestamp, 0)
		if start.After(due) {
			due = start
		}
	}
	amount, ok := InitialSurplusAmount(o.WalletBalanceRaw, target.Floor)
	if !ok {
		return nil
	}
	var event int64
	if err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balance_events(event_id,target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,previous_amount_raw,amount_raw,delta_amount_raw,observed_slot,observed_at,source,source_commitment,account_data_hash,raw_evidence,projected_at)
VALUES(nextval('loyal_yield.balance_sweep_floor_rebaseline_event_id_seq'),$1,$2,$3,$3,$4,$5,$5,0,$6,$7,'go_autodeposit_desired_rebaseline','confirmed',$8,jsonb_build_object('desiredRevision',$9::bigint,'desiredGeneration',$10::bigint,'requestedAt',$11::timestamptz,'observationClock','rpc_read','floorRaw',$12::bigint),now()) RETURNING event_id`, request.TargetID, target.Artifact.Wallet, target.Artifact.WalletTokenATA, USDCMint, o.WalletBalanceRaw, o.ObservedSlot, observedAt, o.WalletAccountDataSHA256, request.Revision, request.Generation, request.RequestedAt, *target.Floor).Scan(&event); err != nil {
		return err
	}
	var slot int64
	err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1 AND token_mint=$2 AND status IN('scheduled','requested') ORDER BY CASE WHEN status='requested' THEN 0 ELSE 1 END,eligible_after,id LIMIT 1 FOR UPDATE`, request.TargetID, USDCMint).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_scheduled_slots(target_id,token_mint,eligible_after,status) VALUES($1,$2,$3,'scheduled') RETURNING id`, request.TargetID, USDCMint, due).Scan(&slot)
	} else if err == nil {
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET eligible_after=GREATEST(eligible_after,$2),updated_at=now() WHERE id=$1`, slot, due)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_surplus_lots(target_id,scheduled_slot_id,source_event_id,original_amount_raw,remaining_amount_raw,classification,eligible_after,status,confidence,reason) VALUES($1,$2,$3,$4,$4,'initial_surplus',$5,'open','confirmed_snapshot','desired Autodeposit controls applied by engine')`, request.TargetID, slot, event, amount, due)
	return err
}
