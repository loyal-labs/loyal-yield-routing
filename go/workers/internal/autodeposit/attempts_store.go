package autodeposit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type DepositPlanTarget struct {
	ID                   int64   `json:"id,string"`
	ManagedVaultID       int64   `json:"managedVaultId,string"`
	Settings             string  `json:"settings"`
	VaultIndex           int     `json:"vaultIndex"`
	Wallet               string  `json:"wallet"`
	WalletUsdcAta        string  `json:"walletUsdcAta"`
	WalletTokenAta       string  `json:"walletTokenAta"`
	VaultPubkey          string  `json:"vaultPubkey"`
	VaultUsdcAta         string  `json:"vaultUsdcAta"`
	VaultTokenAta        string  `json:"vaultTokenAta"`
	TokenMint            string  `json:"tokenMint"`
	RoutePolicyAccount   string  `json:"routePolicyAccount"`
	SweepPolicyAccount   string  `json:"sweepPolicyAccount,omitempty"`
	SetupPolicyAccount   string  `json:"setupPolicyAccount,omitempty"`
	RoutePolicySeed      int64   `json:"routePolicySeed,string"`
	CurrentReserve       *string `json:"currentReserve"`
	CurrentMarket        *string `json:"currentMarket"`
	CurrentLiquidityMint *string `json:"currentLiquidityMint"`
}

type DepositPlan struct {
	Version       int               `json:"version"`
	AmountRaw     int64             `json:"amountRaw,string"`
	Reserve       string            `json:"reserve"`
	Market        string            `json:"market"`
	LiquidityMint string            `json:"liquidityMint"`
	Target        DepositPlanTarget `json:"target"`
}

const DepositPlanVersion = 1

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func MarshalDepositPlan(plan DepositPlan) ([]byte, error) {
	if plan.AmountRaw <= 0 {
		return nil, errors.New("autodeposit deposit plan amount must be positive")
	}
	if plan.Version != DepositPlanVersion {
		return nil, fmt.Errorf("autodeposit deposit plan version %d is not supported", plan.Version)
	}
	return json.Marshal(plan)
}

func UnmarshalDepositPlan(raw []byte) (DepositPlan, error) {
	var plan DepositPlan
	if len(raw) == 0 {
		return plan, errors.New("autodeposit claim has no deposit plan")
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return plan, fmt.Errorf("parse autodeposit deposit plan: %w", err)
	}
	if plan.Version != DepositPlanVersion {
		return plan, errors.New("autodeposit claim has an invalid immutable deposit plan version")
	}
	if plan.AmountRaw <= 0 {
		return plan, errors.New("autodeposit deposit plan amount must be positive")
	}
	identity := []string{
		plan.Reserve, plan.Market, plan.LiquidityMint,
		plan.Target.Settings, plan.Target.Wallet,
		plan.Target.WalletUsdcAta, plan.Target.WalletTokenAta,
		plan.Target.VaultPubkey, plan.Target.VaultUsdcAta, plan.Target.VaultTokenAta,
		plan.Target.TokenMint, plan.Target.RoutePolicyAccount,
	}
	for _, field := range identity {
		if field == "" {
			return plan, errors.New("autodeposit deposit plan is missing its frozen destination identity")
		}
	}
	if plan.Target.ID == 0 || plan.Target.ManagedVaultID == 0 {
		return plan, errors.New("autodeposit deposit plan is missing its frozen destination identity")
	}
	return plan, nil
}

const ClaimLease = 10 * time.Minute

// AcquireClaimLease takes executor ownership of a selected claim. It succeeds
// only when no live lease exists, so two executors can never both prepare a
// pull for one claim.
func (s *Store) AcquireClaimLease(ctx context.Context, claimToken string, targetID int64, leaseToken string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_token = $3,
    autodeposit_executor_lease_expires_at = now() + make_interval(secs => $4::double precision),
    updated_at = now()
WHERE claim_token = $1
  AND target_id = $2
  AND status = 'selected'
  AND (
    autodeposit_executor_lease_token IS NULL
    OR autodeposit_executor_lease_expires_at <= now()
  )`, claimToken, targetID, leaseToken, ClaimLease.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire autodeposit claim lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) RenewClaimLease(ctx context.Context, claimToken, leaseToken string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_expires_at = now() + make_interval(secs => $3::double precision),
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND autodeposit_executor_lease_token = $2
  AND autodeposit_executor_lease_expires_at > now()`, claimToken, leaseToken, ClaimLease.Seconds())
	if err != nil {
		return fmt.Errorf("renew autodeposit claim lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: claim %s lost durable ownership", ErrOwnershipLost, claimToken)
	}
	return nil
}

func (s *Store) ReleaseClaimLease(ctx context.Context, claimToken, leaseToken string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_token = NULL,
    autodeposit_executor_lease_expires_at = NULL,
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND autodeposit_executor_lease_token = $2`, claimToken, leaseToken)
	if err != nil {
		return fmt.Errorf("release autodeposit claim lease: %w", err)
	}
	return nil
}

// FreezeDepositPlan stores the immutable destination/amount intent on the claim
// and returns the plan that is actually frozen. The COALESCE means an existing
// plan wins: a retry can never repoint pulled funds at a different reserve, and
// the database trigger rejects any attempted rewrite.
func (s *Store) FreezeDepositPlan(ctx context.Context, claimToken, leaseToken string, plan DepositPlan) (DepositPlan, error) {
	frozen, err := MarshalDepositPlan(plan)
	if err != nil {
		return DepositPlan{}, err
	}
	var stored []byte
	err = s.pool.QueryRow(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_deposit_plan = COALESCE(
      autodeposit_deposit_plan,
      $3::jsonb
    ),
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND autodeposit_executor_lease_token = $2
  AND autodeposit_executor_lease_expires_at > now()
  AND (
    autodeposit_deposit_plan IS NULL
    OR autodeposit_deposit_plan = $3::jsonb
  )
RETURNING autodeposit_deposit_plan`, claimToken, leaseToken, frozen).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositPlan{}, fmt.Errorf("%w: claim %s deposit plan conflicts with this execution or its lease was lost", ErrOwnershipLost, claimToken)
	}
	if err != nil {
		return DepositPlan{}, fmt.Errorf("freeze autodeposit deposit plan: %w", err)
	}
	return UnmarshalDepositPlan(stored)
}

func (s *Store) LoadLatestAttempt(ctx context.Context, claimToken string, operationKind OperationKind) (*DurableAttempt, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, claim_token, operation_kind, attempt_number, execution_id, amount_raw,
       source_pre_balance_raw, destination_pre_balance_raw, signature,
       signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
       last_valid_block_height, attempt_state, broadcast_count, confirmed_slot
FROM loyal_yield.balance_sweep_transaction_attempts
WHERE claim_token = $1
  AND operation_kind = $2
ORDER BY attempt_number DESC
LIMIT 1`, claimToken, string(operationKind))
	if err != nil {
		return nil, fmt.Errorf("load autodeposit %s attempt: %w", operationKind, err)
	}
	defer rows.Close()
	return scanAttempt(rows)
}

func scanAttempt(rows pgx.Rows) (*DurableAttempt, error) {
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var attempt DurableAttempt
	if err := rows.Scan(
		&attempt.ID, &attempt.ClaimToken, &attempt.OperationKind, &attempt.AttemptNumber, &attempt.ExecutionID,
		&attempt.AmountRaw, &attempt.SourcePreBalanceRaw, &attempt.DestinationPreBalanceRaw,
		&attempt.Signature, &attempt.SignedTransactionBase64, &attempt.SignedTransactionSHA256,
		&attempt.RecentBlockhash, &attempt.LastValidBlockHeight, &attempt.State, &attempt.BroadcastCount,
		&attempt.ConfirmedSlot,
	); err != nil {
		return nil, fmt.Errorf("scan autodeposit attempt: %w", err)
	}
	return &attempt, rows.Err()
}

type PreparedAttempt struct {
	ClaimToken               string
	TargetID                 int64
	ScheduledSlotID          int64
	OperationKind            OperationKind
	ExecutionID              *int64
	AmountRaw                int64
	SourcePreBalanceRaw      int64
	DestinationPreBalanceRaw int64
	Signature                string
	SignedTransactionBase64  string
	SignedTransactionSHA256  string
	RecentBlockhash          string
	LastValidBlockHeight     int64
	ProtectionFloorRaw       *int64
}

// PersistPreparedAttempt stores one signed transaction before its first
// broadcast and returns the durable attempt that now owns the operation.
//
// The vault's idle handoff lock and the guarded insert run in one
// transaction. An existing active attempt always wins: it may already be in
// flight, and a second wire for the same operation would create two competing
// spends. A new row is inserted only while this executor still owns the claim
// lease and the target's route policy is active.
func (s *Store) PersistPreparedAttempt(ctx context.Context, prepared PreparedAttempt, leaseToken string) (DurableAttempt, error) {
	if prepared.AmountRaw <= 0 || prepared.Signature == "" || prepared.SignedTransactionBase64 == "" {
		return DurableAttempt{}, errors.New("prepared autodeposit attempt is missing its signed wire")
	}
	if !sha256HexPattern.MatchString(prepared.SignedTransactionSHA256) {
		return DurableAttempt{}, errors.New("prepared autodeposit attempt needs a lowercase hex sha256 wire digest")
	}
	wire, err := base64.StdEncoding.DecodeString(prepared.SignedTransactionBase64)
	if err != nil {
		return DurableAttempt{}, errors.New("prepared autodeposit wire is not base64")
	}
	if _, err := chain.OwnSignedWire(wire, prepared.SignedTransactionSHA256); err != nil {
		return DurableAttempt{}, fmt.Errorf("prepared autodeposit wire identity: %w", err)
	}
	if prepared.SourcePreBalanceRaw < prepared.AmountRaw {
		return DurableAttempt{}, errors.New("prepared autodeposit attempt source balance cannot fund it")
	}
	var stored DurableAttempt
	err = WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var vaultID int64
		var mint string
		err := tx.QueryRow(ctx, `
SELECT vault.id, target.token_mint
FROM loyal_yield.balance_sweep_targets AS target
JOIN loyal_yield.managed_vaults AS vault
  ON vault.settings = target.settings
 AND vault.vault_index = target.vault_index
 AND vault.vault_pubkey = target.vault_pubkey
WHERE target.id = $1`, prepared.TargetID).Scan(&vaultID, &mint)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: autodeposit target %d has no managed vault for its idle handoff", ErrOwnershipLost, prepared.TargetID)
		}
		if err != nil {
			return fmt.Errorf("load autodeposit idle vault handoff: %w", err)
		}
		if err := LockIdleVaultHandoff(ctx, tx, vaultID, mint); err != nil {
			return err
		}
		target, insertAdmitted, err := lockPullAdmission(ctx, tx, prepared)
		if err != nil {
			return err
		}
		policyScoped, err := lockRoutePolicy(ctx, tx, target)
		if err != nil {
			return err
		}
		insertAdmitted = insertAdmitted && policyScoped
		rows, err := tx.Query(ctx, `
WITH guarded_claim AS (
  UPDATE loyal_yield.balance_sweep_lot_claims
  SET updated_at = now()
  WHERE claim_token = $1
    AND target_id = $2
    AND status = 'selected'
    AND autodeposit_executor_lease_token = $3
    AND autodeposit_executor_lease_expires_at > now()
  RETURNING claim_token
),
existing_active AS (
  SELECT id, claim_token, operation_kind, attempt_number, execution_id, amount_raw,
         source_pre_balance_raw, destination_pre_balance_raw, signature,
         signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
         last_valid_block_height, attempt_state, broadcast_count, confirmed_slot
  FROM loyal_yield.balance_sweep_transaction_attempts
  WHERE claim_token = $1
    AND operation_kind = $4
    AND attempt_state IN ('prepared', 'submitted', 'confirmed')
  ORDER BY attempt_number DESC
  LIMIT 1
),
next_attempt AS (
  SELECT COALESCE(MAX(attempt_number), 0) + 1 AS attempt_number
  FROM loyal_yield.balance_sweep_transaction_attempts
  WHERE claim_token = $1
    AND operation_kind = $4
),
inserted AS (
  INSERT INTO loyal_yield.balance_sweep_transaction_attempts (
    claim_token, target_id, scheduled_slot_id, execution_id, operation_kind,
    attempt_number, amount_raw, source_pre_balance_raw, destination_pre_balance_raw,
    signature, signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
    last_valid_block_height, attempt_state
  )
  SELECT
    $1, $2, $5, $6, $4,
    next_attempt.attempt_number, $7, $8, $9,
    $10, $11, $12, $13, $14, 'prepared'
  FROM next_attempt
  CROSS JOIN guarded_claim
  WHERE NOT EXISTS (SELECT 1 FROM existing_active)
    AND $15::boolean
  ON CONFLICT DO NOTHING
  RETURNING id, claim_token, operation_kind, attempt_number, execution_id, amount_raw,
         source_pre_balance_raw, destination_pre_balance_raw, signature,
         signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
         last_valid_block_height, attempt_state, broadcast_count, confirmed_slot
)
SELECT * FROM inserted
UNION ALL
SELECT * FROM existing_active WHERE EXISTS (SELECT 1 FROM guarded_claim)
LIMIT 1`,
			prepared.ClaimToken, prepared.TargetID, leaseToken, string(prepared.OperationKind),
			prepared.ScheduledSlotID, prepared.ExecutionID, prepared.AmountRaw,
			prepared.SourcePreBalanceRaw, prepared.DestinationPreBalanceRaw,
			prepared.Signature, prepared.SignedTransactionBase64, prepared.SignedTransactionSHA256,
			prepared.RecentBlockhash, prepared.LastValidBlockHeight, insertAdmitted)
		if err != nil {
			return fmt.Errorf("persist prepared autodeposit %s attempt: %w", prepared.OperationKind, err)
		}
		defer rows.Close()
		attempt, err := scanAttempt(rows)
		if err != nil {
			return err
		}
		if attempt == nil {
			if !policyScoped {
				return ErrRoutePolicyMissing
			}
			return fmt.Errorf("%w: could not acquire idle ownership for durable %s attempt on claim %s", ErrOwnershipLost, prepared.OperationKind, prepared.ClaimToken)
		}
		stored = *attempt
		return nil
	})
	if err != nil {
		return DurableAttempt{}, err
	}
	return stored, nil
}

func LockIdleVaultHandoff(ctx context.Context, tx pgx.Tx, vaultID int64, mint string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(format('idle-vault-handoff:%s:%s', $1::bigint, $2::text), 0::bigint))`, vaultID, mint); err != nil {
		return fmt.Errorf("lock idle handoff of vault %d mint %s: %w", vaultID, mint, err)
	}
	return nil
}

func lockPullAdmission(ctx context.Context, tx pgx.Tx, prepared PreparedAttempt) (lockedTarget, bool, error) {
	var active bool
	var floor, maximum *int64
	if err := tx.QueryRow(ctx, `SELECT desired_active AND chain_status='active',wallet_balance_floor_raw,max_amount_per_period
 FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, prepared.TargetID).Scan(&active, &floor, &maximum); err != nil {
		return lockedTarget{}, false, fmt.Errorf("lock autodeposit pull controls: %w", err)
	}
	admitted := prepared.OperationKind != OperationPull || (active && floor != nil && prepared.ProtectionFloorRaw != nil &&
		*floor == *prepared.ProtectionFloorRaw && prepared.SourcePreBalanceRaw-prepared.AmountRaw >= *floor &&
		(maximum == nil || prepared.AmountRaw <= *maximum))
	return lockedTarget{id: prepared.TargetID}, admitted, nil
}

func (s *Store) RecordAttemptBroadcast(ctx context.Context, attempt DurableAttempt, leaseToken string) (DurableAttempt, error) {
	rows, err := s.pool.Query(ctx, `
UPDATE loyal_yield.balance_sweep_transaction_attempts
SET attempt_state = 'submitted',
    broadcast_count = broadcast_count + 1,
    last_broadcast_at = now(),
    last_status_checked_at = now(),
    error_detail = NULL,
    updated_at = now()
WHERE id = $1
  AND signature = $2
  AND signed_transaction_sha256 = $3
  AND attempt_state IN ('prepared', 'submitted')
  AND EXISTS (
    SELECT 1
    FROM loyal_yield.balance_sweep_lot_claims AS claim
    WHERE claim.claim_token = $4
      AND claim.status = 'selected'
      AND claim.autodeposit_executor_lease_token = $5
      AND claim.autodeposit_executor_lease_expires_at > now()
  )
RETURNING id, claim_token, operation_kind, attempt_number, execution_id, amount_raw,
       source_pre_balance_raw, destination_pre_balance_raw, signature,
       signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
       last_valid_block_height, attempt_state, broadcast_count, confirmed_slot`,
		attempt.ID, attempt.Signature, attempt.SignedTransactionSHA256, attempt.ClaimToken, leaseToken)
	if err != nil {
		return DurableAttempt{}, fmt.Errorf("record autodeposit attempt broadcast: %w", err)
	}
	defer rows.Close()
	recorded, err := scanAttempt(rows)
	if err != nil {
		return DurableAttempt{}, err
	}
	if recorded == nil {
		return DurableAttempt{}, fmt.Errorf("durable attempt %d lost its immutable broadcast identity", attempt.ID)
	}
	return *recorded, nil
}

func (s *Store) RecordAttemptObservation(ctx context.Context, attempt DurableAttempt, observation AttemptObservation, leaseToken string) (DurableAttempt, error) {
	if observation.State == AttemptPrepared || observation.State == AttemptSubmitted {
		return DurableAttempt{}, fmt.Errorf("attempt observation must be a chain outcome, not %s", observation.State)
	}
	var errorDetail *string
	if observation.Err != nil {
		detail := observation.Err.Error()
		if len(detail) > 4000 {
			detail = detail[:4000]
		}
		errorDetail = &detail
	}
	rows, err := s.pool.Query(ctx, `
UPDATE loyal_yield.balance_sweep_transaction_attempts
SET attempt_state = $2,
    confirmed_slot = $3,
    last_status_checked_at = now(),
    error_detail = $4,
    updated_at = now()
WHERE id = $1
  AND signature = $5
  AND signed_transaction_sha256 = $6
  AND attempt_state IN ('prepared', 'submitted')
  AND EXISTS (
    SELECT 1
    FROM loyal_yield.balance_sweep_lot_claims AS claim
    WHERE claim.claim_token = $7
      AND claim.status = 'selected'
      AND claim.autodeposit_executor_lease_token = $8
      AND claim.autodeposit_executor_lease_expires_at > now()
  )
RETURNING id, claim_token, operation_kind, attempt_number, execution_id, amount_raw,
       source_pre_balance_raw, destination_pre_balance_raw, signature,
       signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
       last_valid_block_height, attempt_state, broadcast_count, confirmed_slot`,
		attempt.ID, string(observation.State), observation.ConfirmedSlot, errorDetail,
		attempt.Signature, attempt.SignedTransactionSHA256, attempt.ClaimToken, leaseToken)
	if err != nil {
		return DurableAttempt{}, fmt.Errorf("record autodeposit attempt observation: %w", err)
	}
	defer rows.Close()
	recorded, err := scanAttempt(rows)
	if err != nil {
		return DurableAttempt{}, err
	}
	if recorded != nil {
		return *recorded, nil
	}
	current, err := s.LoadLatestAttempt(ctx, attempt.ClaimToken, attempt.OperationKind)
	if err != nil {
		return DurableAttempt{}, err
	}
	if current != nil && current.Signature == attempt.Signature {
		return *current, nil
	}
	return DurableAttempt{}, fmt.Errorf("durable attempt %d could not record signature observation", attempt.ID)
}

type PullRecoveryContext struct {
	Attempt DurableAttempt
	Plan    DepositPlan
}

func (s *Store) LoadPullRecoveryContext(ctx context.Context, claimToken string, targetID, scheduledSlotID int64) (*PullRecoveryContext, error) {
	rows, err := s.pool.Query(ctx, `
SELECT attempt.id, attempt.claim_token, attempt.operation_kind, attempt.attempt_number,
       attempt.execution_id, attempt.amount_raw, attempt.source_pre_balance_raw,
       attempt.destination_pre_balance_raw, attempt.signature, attempt.signed_transaction_base64,
       attempt.signed_transaction_sha256, attempt.recent_blockhash, attempt.last_valid_block_height,
       attempt.attempt_state, attempt.broadcast_count, attempt.confirmed_slot,
       claim.autodeposit_deposit_plan
FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
JOIN loyal_yield.balance_sweep_lot_claims AS claim
  ON claim.claim_token = attempt.claim_token
 AND claim.target_id = attempt.target_id
 AND claim.status = 'selected'
JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
  ON slot.id = attempt.scheduled_slot_id
 AND slot.target_id = attempt.target_id
 AND slot.claim_token = attempt.claim_token
WHERE attempt.claim_token = $1
  AND attempt.target_id = $2
  AND attempt.scheduled_slot_id = $3
  AND attempt.operation_kind = 'pull'
  AND attempt.attempt_state IN ('prepared', 'submitted', 'confirmed')
ORDER BY attempt.attempt_number DESC
LIMIT 1`, claimToken, targetID, scheduledSlotID)
	if err != nil {
		return nil, fmt.Errorf("load autodeposit recovery context: %w", err)
	}
	defer rows.Close()
	var (
		attempt DurableAttempt
		planRaw []byte
		found   bool
	)
	if rows.Next() {
		if err := rows.Scan(
			&attempt.ID, &attempt.ClaimToken, &attempt.OperationKind, &attempt.AttemptNumber,
			&attempt.ExecutionID, &attempt.AmountRaw, &attempt.SourcePreBalanceRaw,
			&attempt.DestinationPreBalanceRaw, &attempt.Signature, &attempt.SignedTransactionBase64,
			&attempt.SignedTransactionSHA256, &attempt.RecentBlockhash, &attempt.LastValidBlockHeight,
			&attempt.State, &attempt.BroadcastCount, &attempt.ConfirmedSlot, &planRaw,
		); err != nil {
			return nil, fmt.Errorf("scan autodeposit recovery context: %w", err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	if attempt.State == AttemptConfirmed && attempt.ConfirmedSlot == nil {
		return nil, fmt.Errorf("confirmed autodeposit pull %d has no confirmed slot", attempt.ID)
	}
	plan, err := UnmarshalDepositPlan(planRaw)
	if err != nil {
		return nil, fmt.Errorf("confirmed pull %s lost its immutable deposit plan: %w", attempt.Signature, err)
	}
	if plan.Target.ID != targetID || plan.AmountRaw != attempt.AmountRaw {
		return nil, fmt.Errorf("confirmed pull %s does not match its immutable deposit plan", attempt.Signature)
	}
	return &PullRecoveryContext{Attempt: attempt, Plan: plan}, nil
}

func (s *Store) LoadConfirmedSiblingTopUpsSince(ctx context.Context, targetID int64, claimToken string, afterAttemptID int64) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(amount_raw), 0)
FROM loyal_yield.balance_sweep_transaction_attempts
WHERE target_id = $1
  AND operation_kind = 'top_up'
  AND attempt_state = 'confirmed'
  AND claim_token <> $2
  AND id > $3`, targetID, claimToken, afterAttemptID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("load confirmed sibling autodeposit top-ups: %w", err)
	}
	return total, nil
}

var ErrRoutePolicyMissing = errors.New("autodeposit target has no active same_mint_kamino route policy")

func lockRoutePolicy(ctx context.Context, tx pgx.Tx, target lockedTarget) (bool, error) {
	var policy int64
	err := tx.QueryRow(ctx, `SELECT rp.id FROM loyal_yield.balance_sweep_targets target
JOIN loyal_yield.managed_vaults mv ON mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey AND mv.active
JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id AND rp.active AND rp.cluster='mainnet-beta'
AND rp.authority=target.authority AND rp.settings=target.settings AND rp.vault_index=target.vault_index AND rp.vault_pubkey=target.vault_pubkey
WHERE target.id=$1 AND 'same_mint_kamino'=ANY(rp.route_modes)
FOR SHARE OF mv,rp`, target.id).Scan(&policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
