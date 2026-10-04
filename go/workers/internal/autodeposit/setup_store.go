package autodeposit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const setupColumns = `id,claim_token,stage,account,plan,signature,signed_transaction_base64,signed_transaction_sha256,recent_blockhash,last_valid_block_height,attempt_state,broadcast_count,confirmed_slot,readback_slot,readback_evidence`

func scanSetup(row pgx.Row) (SetupAttempt, error) {
	var a SetupAttempt
	var raw []byte
	var evidence []byte
	var stage SetupStage
	var account string
	err := row.Scan(&a.ID, &a.ClaimToken, &stage, &account, &raw, &a.Wire.Signature, &a.Wire.SignedTransactionBase64, &a.Wire.SignedTransactionSHA256, &a.Wire.RecentBlockhash, &a.Wire.LastValidBlockHeight, &a.State, &a.BroadcastCount, &a.ConfirmedSlot, &a.ReadbackSlot, &evidence)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(raw, &a.Plan); err != nil {
		return a, err
	}
	if a.Plan.Stage != stage || a.Plan.Account != account {
		return a, errors.New("setup row and frozen plan identity disagree")
	}
	if len(evidence) > 0 {
		var readback SetupReadback
		if err = json.Unmarshal(evidence, &readback); err != nil {
			return a, err
		}
		a.ReadbackEvidence = &readback
	}
	if a.State == AttemptConfirmed && (a.ConfirmedSlot == nil || a.ReadbackSlot == nil || *a.ReadbackSlot < *a.ConfirmedSlot) {
		return a, errors.New("confirmed setup lacks account readback")
	}
	if a.State == AttemptConfirmed && (a.ReadbackEvidence == nil || a.ReadbackEvidence.Account != a.Plan.Account || a.ReadbackEvidence.ObservedSlot != *a.ReadbackSlot || !sha256HexPattern.MatchString(a.ReadbackEvidence.DataSHA256)) {
		return a, errors.New("confirmed setup account evidence is missing or contradictory")
	}
	if _, err = persistedWireTransaction(a.durable()); err != nil {
		return a, err
	}
	return a, nil
}
func guardSetupClaim(ctx context.Context, tx pgx.Tx, claimToken, leaseToken string) error {
	var owned string
	err := tx.QueryRow(ctx, `SELECT claim_token FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1 AND status='selected' AND autodeposit_executor_lease_token=$2 AND autodeposit_executor_lease_expires_at>now() FOR UPDATE`, claimToken, leaseToken).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOwnershipLost
	}
	return err
}
func (s *Store) LoadDestinationSetup(ctx context.Context, claimToken, leaseToken string) (*SetupAttempt, error) {
	var out *SetupAttempt
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := guardSetupClaim(ctx, tx, claimToken, leaseToken); err != nil {
			return err
		}
		a, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE claim_token=$1 ORDER BY id DESC LIMIT 1`, claimToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &a
		return nil
	})
	return out, err
}
func (s *Store) LoadFrozenDepositPlan(ctx context.Context, claimToken string, targetID int64, leaseToken string) (DepositPlan, error) {
	var plan DepositPlan
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := guardSetupClaim(ctx, tx, claimToken, leaseToken); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT autodeposit_deposit_plan FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1 AND target_id=$2`, claimToken, targetID).Scan(&raw); err != nil {
			return err
		}
		var err error
		plan, err = UnmarshalDepositPlan(raw)
		return err
	})
	return plan, err
}
func (s *Store) PersistDestinationSetup(ctx context.Context, claimToken, leaseToken string, plan DestinationSetupPlan, wire BuiltWire) (SetupAttempt, error) {
	candidate := SetupAttempt{ClaimToken: claimToken, Plan: plan, Wire: wire, State: AttemptPrepared}
	if _, err := persistedWireTransaction(candidate.durable()); err != nil {
		return SetupAttempt{}, err
	}
	if plan.ObservedSlot <= 0 || plan.Account == "" || wire.LastValidBlockHeight <= 0 {
		return SetupAttempt{}, errors.New("setup intent requires frozen account, observation and validity window")
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return SetupAttempt{}, err
	}
	var out SetupAttempt
	err = WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Same lock ordering as the legacy idle handoff: advisory lock, then claim.
		tag, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(format('idle-vault-handoff:%s:%s',vault.id,target.token_mint),0::bigint)) FROM loyal_yield.balance_sweep_lot_claims claim JOIN loyal_yield.balance_sweep_targets target ON target.id=claim.target_id JOIN loyal_yield.managed_vaults vault ON vault.settings=target.settings AND vault.vault_index=target.vault_index AND vault.vault_pubkey=target.vault_pubkey WHERE claim.claim_token=$1`, claimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrOwnershipLost
		}
		if err = guardSetupClaim(ctx, tx, claimToken, leaseToken); err != nil {
			return err
		}
		var frozenRaw []byte
		if err = tx.QueryRow(ctx, `SELECT autodeposit_deposit_plan FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1`, claimToken).Scan(&frozenRaw); err != nil {
			return err
		}
		frozen, err := UnmarshalDepositPlan(frozenRaw)
		if err != nil {
			return err
		}
		if err = validateDestinationSetupPlan(frozen, plan); err != nil {
			return err
		}
		existing, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE claim_token=$1 AND attempt_state IN('prepared','submitted','unknown','ambiguous') ORDER BY id DESC LIMIT 1`, claimToken))
		if err == nil {
			out = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var hasPull bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token=$1 AND operation_kind='pull' AND attempt_state IN('prepared','submitted','confirmed','unknown','ambiguous')) OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims claim JOIN loyal_yield.balance_sweep_targets target ON target.id=claim.target_id JOIN loyal_yield.managed_vaults vault ON vault.settings=target.settings AND vault.vault_index=target.vault_index AND vault.vault_pubkey=target.vault_pubkey JOIN loyal_yield.rebalance_decisions decision ON decision.vault_id=vault.id AND decision.liquidity_mint=target.token_mint AND decision.status::text IN('planned','simulating','ready','submitted','confirming') AND decision.execution_plan->>'kind'='idle_vault_deposit' WHERE claim.claim_token=$1)`, claimToken).Scan(&hasPull); err != nil {
			return err
		}
		if hasPull {
			return ErrClaimCustodyHeld
		}
		out, err = scanSetup(tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_destination_setup_attempts(claim_token,stage,account,plan,signature,signed_transaction_base64,signed_transaction_sha256,recent_blockhash,last_valid_block_height,attempt_number,attempt_state)SELECT $1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,COALESCE(MAX(attempt_number),0)+1,'prepared' FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE claim_token=$1 AND stage=$2 RETURNING `+setupColumns, claimToken, string(plan.Stage), plan.Account, raw, wire.Signature, wire.SignedTransactionBase64, wire.SignedTransactionSHA256, wire.RecentBlockhash, wire.LastValidBlockHeight))
		return err
	})
	return out, err
}
func sameSetup(a, b SetupAttempt) bool {
	ap, _ := json.Marshal(a.Plan)
	bp, _ := json.Marshal(b.Plan)
	return a.ID == b.ID && a.ClaimToken == b.ClaimToken && a.Wire == b.Wire && bytes.Equal(ap, bp)
}
func (s *Store) transitionSetup(ctx context.Context, attempt SetupAttempt, leaseToken string, update func(pgx.Tx, SetupAttempt) (SetupAttempt, error)) (SetupAttempt, error) {
	var out SetupAttempt
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := guardSetupClaim(ctx, tx, attempt.ClaimToken, leaseToken); err != nil {
			return err
		}
		saved, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE id=$1 AND claim_token=$2 FOR UPDATE`, attempt.ID, attempt.ClaimToken))
		if err != nil {
			return err
		}
		if !sameSetup(saved, attempt) {
			return fmt.Errorf("setup immutable intent differs from stored record")
		}
		out, err = update(tx, saved)
		return err
	})
	return out, err
}
func (s *Store) RecordDestinationSetupBroadcast(ctx context.Context, attempt SetupAttempt, leaseToken string) (SetupAttempt, error) {
	return s.transitionSetup(ctx, attempt, leaseToken, func(tx pgx.Tx, saved SetupAttempt) (SetupAttempt, error) {
		if saved.State == AttemptConfirmed || saved.State == AttemptFailed || saved.State == AttemptExpired || saved.State == AttemptAmbiguous {
			return SetupAttempt{}, errors.New("setup terminal or ambiguous intent cannot broadcast")
		}
		return scanSetup(tx.QueryRow(ctx, `UPDATE loyal_yield.balance_sweep_destination_setup_attempts SET attempt_state='submitted',broadcast_count=broadcast_count+1,last_broadcast_at=now(),updated_at=now() WHERE id=$1 RETURNING `+setupColumns, saved.ID))
	})
}
func (s *Store) RecordDestinationSetupObservation(ctx context.Context, attempt SetupAttempt, observation AttemptObservation, evidence *SetupReadback, leaseToken string) (SetupAttempt, error) {
	switch observation.State {
	case AttemptConfirmed:
		if observation.ConfirmedSlot == nil || *observation.ConfirmedSlot <= 0 || evidence == nil || evidence.ObservedSlot < *observation.ConfirmedSlot || evidence.ObservedSlot < attempt.Plan.ObservedSlot || evidence.MinimumSlot != *observation.ConfirmedSlot || evidence.Account != attempt.Plan.Account || evidence.Proof != "decoded_"+string(attempt.Plan.Stage)+"_identity" || !sha256HexPattern.MatchString(evidence.DataSHA256) {
			return SetupAttempt{}, errors.New("setup confirmation requires current account readback")
		}
		owner := KLendProgramID
		if attempt.Plan.Stage == SetupATA {
			owner = splTokenID
		}
		if attempt.Plan.Stage == SetupFarm {
			owner = farmsProgramID
		}
		if evidence.Owner != owner {
			return SetupAttempt{}, errors.New("setup readback owner disagrees with protocol stage")
		}
	case AttemptFailed, AttemptExpired, AttemptUnknown, AttemptAmbiguous:
	default:
		return SetupAttempt{}, errors.New("unsupported setup observation")
	}
	return s.transitionSetup(ctx, attempt, leaseToken, func(tx pgx.Tx, saved SetupAttempt) (SetupAttempt, error) {
		if saved.State == AttemptConfirmed || saved.State == AttemptFailed || saved.State == AttemptExpired {
			if saved.State != observation.State {
				return SetupAttempt{}, errors.New("setup terminal observation contradicted")
			}
			return saved, nil
		}
		var readback *int64
		var rawEvidence []byte
		if observation.State == AttemptConfirmed {
			readback = &evidence.ObservedSlot
			var err error
			rawEvidence, err = json.Marshal(evidence)
			if err != nil {
				return SetupAttempt{}, err
			}
		}
		detail := ""
		if observation.Err != nil {
			detail = observation.Err.Error()
		}
		return scanSetup(tx.QueryRow(ctx, `UPDATE loyal_yield.balance_sweep_destination_setup_attempts SET attempt_state=$2,confirmed_slot=$3,readback_slot=$4,error_detail=NULLIF($5,''),readback_evidence=$6::jsonb,last_status_checked_at=now(),updated_at=now() WHERE id=$1 RETURNING `+setupColumns, saved.ID, string(observation.State), observation.ConfirmedSlot, readback, detail, rawEvidence))
	})
}
