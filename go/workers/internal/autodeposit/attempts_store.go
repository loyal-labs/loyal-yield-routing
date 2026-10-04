package autodeposit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// DepositPlanTarget is the frozen destination identity of an autodeposit top-up.
// It is persisted before the pull is broadcast so recovery knows exactly where
// the pulled amount must land, even if the fleet later changes the vault's
// current reserve. The JSON shape is the legacy executor's: raw custody values
// and long ids travel as strings so no JSON number can lose precision.
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

// DepositPlan is the immutable intent stored on a selected claim: the exact
// pull amount plus the frozen destination. The database trigger keeps the
// column immutable once written; the first plan wins.
type DepositPlan struct {
	Version       int               `json:"version"`
	AmountRaw     int64             `json:"amountRaw,string"`
	Reserve       string            `json:"reserve"`
	Market        string            `json:"market"`
	LiquidityMint string            `json:"liquidityMint"`
	Target        DepositPlanTarget `json:"target"`
}

// DepositPlanVersion is the only plan shape this family reads.
const DepositPlanVersion = 1

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MarshalDepositPlan renders the plan exactly as the legacy executor serialized
// it, so a plan frozen by a Go worker parses in the TypeScript recovery path
// and vice versa.
func MarshalDepositPlan(plan DepositPlan) ([]byte, error) {
	if plan.AmountRaw <= 0 {
		return nil, errors.New("autodeposit deposit plan amount must be positive")
	}
	if plan.Version != DepositPlanVersion {
		return nil, fmt.Errorf("autodeposit deposit plan version %d is not supported", plan.Version)
	}
	return json.Marshal(plan)
}

// UnmarshalDepositPlan parses a stored plan and rejects anything this family
// cannot execute exactly: the same fields the legacy parser requires.
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

// ClaimLeaseSeconds is the executor ownership window on a selected claim.
const ClaimLeaseSeconds = "10 minutes"

// AcquireClaimLease takes executor ownership of a selected claim. It succeeds
// only when no live lease exists, so two executors can never both prepare a
// pull for one claim.
func (s *Store) AcquireClaimLease(ctx context.Context, claimToken string, targetID int64, leaseToken string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_token = $3,
    autodeposit_executor_lease_expires_at = now() + interval '`+ClaimLeaseSeconds+`',
    updated_at = now()
WHERE claim_token = $1
  AND target_id = $2
  AND status = 'selected'
  AND (
    autodeposit_executor_lease_token IS NULL
    OR autodeposit_executor_lease_expires_at <= now()
  )`, claimToken, targetID, leaseToken)
	if err != nil {
		return false, fmt.Errorf("acquire autodeposit claim lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RenewClaimLease extends an owned lease or reports lost ownership: a lease
// that another executor took means this execution must stop touching the claim.
func (s *Store) RenewClaimLease(ctx context.Context, claimToken, leaseToken string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_expires_at = now() + interval '`+ClaimLeaseSeconds+`',
    updated_at = now()
WHERE claim_token = $1
  AND status = 'selected'
  AND autodeposit_executor_lease_token = $2
  AND autodeposit_executor_lease_expires_at > now()`, claimToken, leaseToken)
	if err != nil {
		return fmt.Errorf("renew autodeposit claim lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: claim %s lost durable ownership", ErrOwnershipLost, claimToken)
	}
	return nil
}

// ReleaseClaimLease drops executor ownership when the execution defers without
// finishing. Only the token that owns the lease can clear it.
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

// LoadLatestAttempt reads the newest attempt of one operation for a claim.
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

// PreparedAttempt is the exact signed wire to persist before any broadcast.
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
	// Only fresh pull admission uses this protection revision. Existing signed
	// attempts remain recoverable when controls change.
	ProtectionFloorRaw *int64
}

// PersistPreparedAttempt stores one signed transaction before its first
// broadcast and returns the durable attempt that now owns the operation.
//
// The idle-vault-handoff advisory lock and the guarded insert run in one
// transaction. An existing active attempt always wins: it may already be in
// flight, and a second wire for the same operation would create two competing
// spends. A new row is inserted only while this executor still owns the claim
// lease and — for pulls — while no fleet idle-deposit decision is concurrently
// moving the same vault balance.
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
	if _, err := solana.OwnSignedWire(wire, prepared.SignedTransactionSHA256); err != nil {
		return DurableAttempt{}, fmt.Errorf("prepared autodeposit wire identity: %w", err)
	}
	if prepared.SourcePreBalanceRaw < prepared.AmountRaw {
		return DurableAttempt{}, errors.New("prepared autodeposit attempt source balance cannot fund it")
	}
	var stored DurableAttempt
	err = WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		lock, err := tx.Exec(ctx, `
SELECT pg_advisory_xact_lock(
    hashtextextended(
      format('idle-vault-handoff:%s:%s', vault.id, target.token_mint),
      0::bigint
    )
)
FROM loyal_yield.balance_sweep_targets AS target
JOIN loyal_yield.managed_vaults AS vault
  ON vault.settings = target.settings
 AND vault.vault_index = target.vault_index
 AND vault.vault_pubkey = target.vault_pubkey
WHERE target.id = $1`, prepared.TargetID)
		if err != nil {
			return fmt.Errorf("lock autodeposit idle vault handoff: %w", err)
		}
		if lock.RowsAffected() != 1 {
			return fmt.Errorf("%w: autodeposit target %d has no managed vault for its idle handoff", ErrOwnershipLost, prepared.TargetID)
		}
		// Serialize user control changes with new signed pull publication. The
		// fresh checks apply only to inserted wire, never adoption/recovery.
		var admissionActive bool
		var floor, maximum *int64
		if err := tx.QueryRow(ctx, `SELECT desired_active AND chain_status='active',wallet_balance_floor_raw,max_amount_per_period
 FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, prepared.TargetID).Scan(&admissionActive, &floor, &maximum); err != nil {
			return fmt.Errorf("lock autodeposit pull controls: %w", err)
		}
		pullAllowed := prepared.OperationKind != OperationPull || (admissionActive && floor != nil && prepared.ProtectionFloorRaw != nil &&
			*floor == *prepared.ProtectionFloorRaw && prepared.SourcePreBalanceRaw-prepared.AmountRaw >= *floor &&
			(maximum == nil || prepared.AmountRaw <= *maximum))
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
    AND attempt_state IN ('prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous')
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
    AND ($4 <> 'pull' OR NOT EXISTS (
      SELECT 1 FROM loyal_yield.balance_sweep_destination_setup_attempts setup
      WHERE setup.claim_token=$1 AND setup.attempt_state IN ('prepared','submitted','unknown','ambiguous')
    ))
    AND ($4 <> 'pull' OR NOT EXISTS (
      SELECT 1
      FROM loyal_yield.balance_sweep_targets AS target
      JOIN loyal_yield.managed_vaults AS vault
        ON vault.settings = target.settings
       AND vault.vault_index = target.vault_index
       AND vault.vault_pubkey = target.vault_pubkey
      JOIN loyal_yield.rebalance_decisions AS fleet_decision
        ON fleet_decision.vault_id = vault.id
       AND fleet_decision.liquidity_mint = target.token_mint
       AND fleet_decision.status::text IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
       AND fleet_decision.execution_plan ->> 'kind' = 'idle_vault_deposit'
      WHERE target.id = $2
    ))
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
			prepared.RecentBlockhash, prepared.LastValidBlockHeight, pullAllowed)
		if err != nil {
			return fmt.Errorf("persist prepared autodeposit %s attempt: %w", prepared.OperationKind, err)
		}
		defer rows.Close()
		attempt, err := scanAttempt(rows)
		if err != nil {
			return err
		}
		if attempt == nil {
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

// RecordAttemptBroadcast marks the persisted wire as submitted. The immutable
// identity columns must still match what was persisted, or ownership of the
// operation has changed hands.
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
  AND attempt_state IN ('prepared', 'submitted', 'unknown')
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

// RecordAttemptObservation stores one chain reading of the persisted signature.
// When the update finds nothing to change, the attempt is re-read: another
// executor may have recorded the same signature first, which is the same truth.
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
  AND attempt_state IN ('prepared', 'submitted', 'unknown', 'ambiguous')
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

// PullRecoveryContext is a claim whose pull already holds custody: the durable
// attempt plus the frozen plan that says where the funds must land.
type PullRecoveryContext struct {
	Attempt DurableAttempt
	Plan    DepositPlan
}

// LoadPullRecoveryContext resolves an existing claim's confirmed-or-pending
// pull and its immutable deposit plan. It refuses half-identified recovery: a
// confirmed pull without a slot, or a plan that disagrees with the attempt
// amount, is an integrity fault an operator must look at, not a retry.
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
  AND attempt.attempt_state IN ('prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous')
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

// LoadConfirmedSiblingTopUpsSince sums confirmed top-ups on the same target that
// were recorded after the given attempt row, so a persisted vault snapshot can
// be discounted instead of misread as a lost deposit.
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
