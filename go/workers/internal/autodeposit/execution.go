package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// The balance_sweep_executions row is the legacy accounted-recovery contract:
// one execution per confirmed pull, keyed by the pull signature, carrying the
// exact wallet/custody balances the pull receipt proved. The Go executor writes
// the same columns the TypeScript executor writes, so the recovery script and
// this package agree on every byte of evidence.

// ErrOwnershipLost is declared in claims.go; finalization maps the database's
// lease error onto it so callers treat it exactly like any other lost lease.

// CreatePullExecution inserts the execution row a confirmed pull owns and
// leaves the pull's immutable execution_id NULL. A top-up is prepared with
// this returned id. Replay must match every recorded effect and identity.
func (s *Store) CreatePullExecution(ctx context.Context, claimToken string, targetID, scheduledSlotID int64, leaseToken string, plan DepositPlan, pull DurableAttempt, walletPreRaw, walletPostRaw, custodyPreRaw, custodyPostRaw int64) (int64, error) {
	if pull.State != AttemptConfirmed || pull.ConfirmedSlot == nil {
		return 0, fmt.Errorf("autodeposit pull %s is not confirmed; no execution row may exist", pull.Signature)
	}
	if pull.AmountRaw != plan.AmountRaw {
		return 0, fmt.Errorf("confirmed pull amount %d does not match the frozen plan %d", pull.AmountRaw, plan.AmountRaw)
	}
	if plan.AmountRaw <= 0 || walletPreRaw < plan.AmountRaw || walletPostRaw < 0 || walletPreRaw-walletPostRaw != plan.AmountRaw || custodyPreRaw < 0 || custodyPostRaw < custodyPreRaw || custodyPostRaw-custodyPreRaw != plan.AmountRaw || *pull.ConfirmedSlot <= 0 {
		return 0, &EffectAmbiguousError{Detail: "pull execution balances do not prove exact wallet-to-custody movement"}
	}
	rawEvidence, err := json.Marshal(map[string]string{
		"source":     "autodeposit-workers-v2-executor",
		"claimToken": claimToken,
	})
	if err != nil {
		return 0, err
	}
	decodedEvidence, err := json.Marshal(map[string]string{
		"sequence": "subscription_pull_then_mandatory_kamino_deposit",
	})
	if err != nil {
		return 0, err
	}
	dedupeKey := fmt.Sprintf("%d:autodeposit-pull:%s", targetID, pull.Signature)
	var executionID int64
	err = WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// The claim lease is still the fence: a lost claim never mints accounting.
		var ownedClaim string
		if err := tx.QueryRow(ctx, `
SELECT claim_token FROM loyal_yield.balance_sweep_lot_claims
  WHERE claim_token = $1 AND target_id = $2 AND status = 'selected'
    AND autodeposit_executor_lease_token = $3
    AND autodeposit_executor_lease_expires_at > now()
FOR UPDATE`, claimToken, targetID, leaseToken).Scan(&ownedClaim); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: claim %s cannot account execution under this lease", ErrOwnershipLost, claimToken)
			}
			return fmt.Errorf("check autodeposit claim lease for execution: %w", err)
		}
		if err := tx.QueryRow(ctx, `
WITH inserted AS (
  INSERT INTO loyal_yield.balance_sweep_executions (
    target_id, signature, slot, source_wallet_ata, destination_vault_ata,
    token_mint, source_token_ata, destination_token_ata, amount_raw,
    source_pre_balance_raw, source_post_balance_raw,
    destination_pre_balance_raw, destination_post_balance_raw,
    source_commitment, raw_evidence, decoded_evidence, received_at, decoded_at, dedupe_key
  )
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'confirmed',
          $14::jsonb, $15::jsonb, now(), now(), $16)
  ON CONFLICT (dedupe_key) DO NOTHING
  RETURNING id
), existing AS (
  SELECT id FROM loyal_yield.balance_sweep_executions WHERE dedupe_key = $16
    AND target_id=$1 AND signature=$2 AND slot=$3
    AND source_wallet_ata=$4 AND destination_vault_ata=$5 AND token_mint=$6
    AND source_token_ata=$7 AND destination_token_ata=$8 AND amount_raw=$9
    AND source_pre_balance_raw=$10 AND source_post_balance_raw=$11
    AND destination_pre_balance_raw=$12 AND destination_post_balance_raw=$13
    AND source_commitment='confirmed'
)
SELECT id FROM inserted
UNION ALL
SELECT id FROM existing
LIMIT 1`,
			targetID, pull.Signature, *pull.ConfirmedSlot,
			plan.Target.WalletUsdcAta, plan.Target.VaultUsdcAta,
			plan.Target.TokenMint, plan.Target.WalletTokenAta, plan.Target.VaultTokenAta,
			plan.AmountRaw, walletPreRaw, walletPostRaw, custodyPreRaw, custodyPostRaw,
			rawEvidence, decodedEvidence, dedupeKey,
		).Scan(&executionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &EffectAmbiguousError{Detail: "existing pull execution contradicts immutable receipt identity"}
			}
			return fmt.Errorf("insert autodeposit pull execution: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return executionID, nil
}

// FinalizeConfirmedAutodeposit publishes the confirmed top-up into yield
// accounting through the shared atomic function: deposits, holding events,
// execution completion, lot linkage, claim and slot completion all commit or
// roll back together. The database's lease error is surfaced as a lost claim.
func (s *Store) FinalizeConfirmedAutodeposit(ctx context.Context, claimToken string, executionID, scheduledSlotID int64, leaseToken string, postConfirmPositionAmountRaw, postConfirmObservedSlot int64) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `
SELECT loyal_yield.finalize_confirmed_autodeposit($1, $2, $3, $4, $5, $6)`,
		claimToken, executionID, scheduledSlotID, leaseToken,
		postConfirmPositionAmountRaw, postConfirmObservedSlot).Scan(&status)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return "", fmt.Errorf("%w: claim %s lost its lease during finalization", ErrOwnershipLost, claimToken)
		}
		return "", fmt.Errorf("finalize autodeposit claim %s: %w", claimToken, err)
	}
	if status != "completed" && status != "already_completed" {
		return "", fmt.Errorf("autodeposit claim %s finalization returned %q", claimToken, status)
	}
	return status, nil
}
