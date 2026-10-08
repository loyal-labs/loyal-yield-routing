package earn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Earn reconciliation job queue and mutation application, ported from
// loyal-yield-store store.rs (claim/retry/dead_letter/complete
// earn_reconciliation_job and apply_earn_*). Row states are unchanged.

// Job is one claimed earn_reconciliation_jobs row.
type Job struct {
	ID           int64
	Consumer     string
	EventKey     string
	DurableSlot  uint64
	EventPayload json.RawMessage
	VaultPayload json.RawMessage
	AttemptCount int32
}

// claimableJob is the readiness predicate over loyal_yield.earn_reconciliation_jobs
// for consumer $1: due, unleased, and not behind an earlier pending job of the
// same vault. ClaimJob and the observer's backlog check share it, so a vault
// blocked behind a failing head job is backlog for neither.
const claimableJob = `
consumer_name = $1
                  AND completed_at IS NULL
                  AND next_attempt_at <= NOW()
                  AND (claim_expires_at IS NULL OR claim_expires_at <= NOW())
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.earn_reconciliation_jobs earlier
                      WHERE earlier.consumer_name = earn_reconciliation_jobs.consumer_name
                        AND earlier.settings = earn_reconciliation_jobs.settings
                        AND earlier.vault_index = earn_reconciliation_jobs.vault_index
                        AND earlier.vault_pubkey = earn_reconciliation_jobs.vault_pubkey
                        AND earlier.completed_at IS NULL
                        AND (earlier.durable_slot, earlier.id)
                            < (earn_reconciliation_jobs.durable_slot, earn_reconciliation_jobs.id)
                        AND NOT (
                            earlier.durable_slot = earn_reconciliation_jobs.durable_slot
                            AND earlier.event_payload->>'signature'
                                = earn_reconciliation_jobs.event_payload->>'signature'
                            AND earlier.next_attempt_at > NOW()
                            AND earlier.claim_owner IS NULL
                            AND earlier.claim_expires_at IS NULL
                        )
                  )`

// ClaimJob leases the oldest ready job whose vault has no earlier pending job.
func (s *Store) ClaimJob(ctx context.Context, consumer, owner string, leaseSeconds int64) (*Job, error) {
	var job Job
	var slot int64
	err := s.pool.QueryRow(ctx, `
            WITH candidate AS (
                SELECT id
                FROM loyal_yield.earn_reconciliation_jobs
                WHERE `+claimableJob+`
                ORDER BY durable_slot, id
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.earn_reconciliation_jobs job
            SET claim_owner = $2,
                claim_expires_at = NOW() + make_interval(secs => $3::double precision),
                attempt_count = attempt_count + 1,
                updated_at = NOW()
            FROM candidate
            WHERE job.id = candidate.id
            RETURNING job.id, job.consumer_name, job.event_key, job.durable_slot,
                      job.event_payload, job.vault_payload, job.attempt_count`, consumer, owner, leaseSeconds).
		Scan(&job.ID, &job.Consumer, &job.EventKey, &slot, &job.EventPayload, &job.VaultPayload, &job.AttemptCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if slot < 0 {
		return nil, errors.New("Earn job has negative durable slot")
	}
	job.DurableSlot = uint64(slot)
	return &job, nil
}

func (s *Store) RetryJob(ctx context.Context, jobID int64, owner, message string, retryAfterSeconds int64) error {
	tag, err := s.pool.Exec(ctx, `
            UPDATE loyal_yield.earn_reconciliation_jobs
            SET claim_owner = NULL,
                claim_expires_at = NULL,
                next_attempt_at = NOW() + make_interval(secs => $3::double precision),
                last_error = $4,
                updated_at = NOW()
            WHERE id = $1 AND claim_owner = $2 AND completed_at IS NULL`, jobID, owner, retryAfterSeconds, message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("Earn reconciliation job %d lost claim before retry", jobID)
	}
	return nil
}

// DeadLetterJob parks a terminally failing job so the vault queue advances;
// clearing completed_at requeues it.
func (s *Store) DeadLetterJob(ctx context.Context, jobID int64, owner, message string) error {
	tag, err := s.pool.Exec(ctx, `
            UPDATE loyal_yield.earn_reconciliation_jobs
            SET completed_at = NOW(), claim_owner = NULL, claim_expires_at = NULL,
                last_error = 'dead_letter: ' || $3, updated_at = NOW()
            WHERE id = $1 AND claim_owner = $2 AND completed_at IS NULL`, jobID, owner, message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("Earn reconciliation job %d lost claim before dead-letter", jobID)
	}
	return nil
}

// ReconciliationContext is the vault's projected route and setup policy.
type ReconciliationContext struct {
	RoutePolicy, SetupPolicy *PolicyMatchInput
}

func (s *Store) LoadContext(ctx context.Context, settings string, vaultIndex uint8, vaultPubkey string) (ReconciliationContext, error) {
	var out ReconciliationContext
	var ready bool
	if err := s.pool.QueryRow(ctx, `
            SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_withdrawals') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL`).Scan(&ready); err != nil {
		return out, err
	}
	if !ready {
		return out, errors.New("canonical Loyal App Earn tables are required for direct reconciliation")
	}
	rows, err := s.pool.Query(ctx, `
            SELECT policy.settings, policy.authority, policy.policy_seed,
                   policy.policy_account, policy.vault_index, policy.vault_pubkey,
                   policy.delegated_signers, policy.threshold, policy.route_modes,
                   policy.stable_mints, policy.kamino_markets,
                   policy.kamino_liquidity_mints, policy.universe_preset,
                   policy.risk_profile, policy.swap_lanes, policy.last_seen_slot,
                   policy.last_seen_signature, policy.cluster,
                   policy.source_commitment,
                   CASE
                     WHEN policy.id = vault.setup_policy_id
                     THEN 'setup'
                     ELSE 'route'
                   END AS role
            FROM loyal_yield.route_policies policy
            LEFT JOIN loyal_yield.managed_vaults vault
              ON vault.settings = policy.settings
             AND vault.vault_index = policy.vault_index
             AND vault.vault_pubkey = policy.vault_pubkey
            WHERE policy.settings = $1 AND policy.vault_index = $2
              AND policy.vault_pubkey = $3
              AND policy.id IN (vault.active_policy_id, vault.setup_policy_id)`, settings, int16(vaultIndex), vaultPubkey)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var policy PolicyMatchInput
		var seed, slot int64
		var index int16
		var threshold int32
		var role string
		if err := rows.Scan(&policy.Settings, &policy.Authority, &seed, &policy.PolicyAccount, &index, &policy.VaultPubkey,
			&policy.DelegatedSigners, &threshold, &policy.RouteModes, &policy.StableMints, &policy.KaminoMarkets,
			&policy.KaminoLiquidityMints, &policy.UniversePreset, &policy.RiskProfile, &policy.SwapLanes, &slot,
			&policy.Signature, &policy.Cluster, &policy.SourceCommitment, &role); err != nil {
			return out, err
		}
		if seed < 0 || slot < 0 || index < 0 || index > 255 || threshold < 0 || threshold > 65535 {
			return out, errors.New("projected policy identity is out of range")
		}
		policy.PolicySeed, policy.Slot, policy.VaultIndex, policy.Threshold = uint64(seed), uint64(slot), uint8(index), uint16(threshold)
		if role == "setup" {
			out.SetupPolicy = &policy
		} else {
			out.RoutePolicy = &policy
		}
	}
	return out, rows.Err()
}

// CompleteJob applies one mutation and completes its job atomically. The
// chain mutation identity makes replays a no-op; refund rent accounting and
// position cleanup have independent identities.
func (s *Store) CompleteJob(ctx context.Context, jobID int64, owner string, mutation EarnMutation) (int, error) {
	applied := 0
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
            UPDATE loyal_yield.earn_reconciliation_jobs
            SET completed_at = NOW(), claim_owner = NULL, claim_expires_at = NULL,
                last_error = NULL, updated_at = NOW()
            WHERE id = $1 AND claim_owner = $2 AND completed_at IS NULL
              AND claim_expires_at > NOW()`, jobID, owner)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("Earn reconciliation job %d lost claim before completion", jobID)
		}
		type identity struct {
			kind, signature, settings string
			vaultIndex                uint8
			vault                     string
			slot                      uint64
		}
		var id *identity
		switch {
		case mutation.Deposit != nil:
			d := mutation.Deposit
			id = &identity{"deposit", d.DepositSignature, d.RoutePolicy.Settings, d.RoutePolicy.VaultIndex, d.RoutePolicy.VaultPubkey, d.DepositSlot}
		case mutation.Withdrawal != nil:
			w := mutation.Withdrawal
			id = &identity{"withdrawal", w.WithdrawalSignature, w.RoutePolicy.Settings, w.RoutePolicy.VaultIndex, w.VaultPubkey, w.ConfirmedSlot}
		case mutation.Cleanup != nil:
			c := mutation.Cleanup
			id = &identity{"cleanup", c.CleanupSignature, c.Settings, c.VaultIndex, c.VaultPubkey, c.ConfirmedSlot}
		case mutation.Refund != nil:
			r := mutation.Refund
			id = &identity{"refund", r.RefundSignature, r.Settings, r.VaultIndex, r.VaultPubkey, r.ConfirmedSlot}
		}
		claim := func(kind, signature, settings string, vaultIndex uint8, vault string, slot uint64) (bool, error) {
			value, err := slotBigint(slot)
			if err != nil {
				return false, err
			}
			tag, err := tx.Exec(ctx, `
                INSERT INTO loyal_yield.earn_chain_mutations (
                    mutation_kind, chain_signature, settings, vault_index,
                    vault_pubkey, confirmed_slot, created_at
                ) VALUES ($1, $2, $3, $4, $5, $6, now())
                ON CONFLICT (mutation_kind, chain_signature, vault_pubkey) DO NOTHING`, kind, signature, settings, int16(vaultIndex), vault, value)
			if err != nil {
				return false, err
			}
			return tag.RowsAffected() != 0, nil
		}
		primary := true
		if id != nil {
			if primary, err = claim(id.kind, id.signature, id.settings, id.vaultIndex, id.vault, id.slot); err != nil {
				return err
			}
			if !primary && (mutation.Refund == nil || !mutation.Refund.FullCleanup) {
				return nil
			}
		}
		switch {
		case mutation.PolicyOnly != nil:
			applied = 1
			return applyPolicyOnly(ctx, tx, *mutation.PolicyOnly)
		case mutation.Deposit != nil:
			applied = 1
			return applyDeposit(ctx, tx, *mutation.Deposit)
		case mutation.Withdrawal != nil:
			applied = 1
			return applyWithdrawal(ctx, tx, *mutation.Withdrawal)
		case mutation.Cleanup != nil:
			applied = 1
			return applyCleanup(ctx, tx, *mutation.Cleanup)
		case mutation.Refund != nil:
			r := *mutation.Refund
			if primary {
				if err := applyRefund(ctx, tx, r); err != nil {
					return err
				}
			}
			cleanup := false
			if r.FullCleanup {
				if cleanup, err = claim("cleanup", r.RefundSignature, r.Settings, r.VaultIndex, r.VaultPubkey, r.ConfirmedSlot); err != nil {
					return err
				}
			}
			if cleanup {
				if err := applyCleanup(ctx, tx, EarnCleanupMutation{Settings: r.Settings, VaultIndex: r.VaultIndex, VaultPubkey: r.VaultPubkey, CleanupSignature: r.RefundSignature, ConfirmedSlot: r.ConfirmedSlot, ObservedAt: r.ObservedAt}); err != nil {
					return err
				}
			}
			if primary || cleanup {
				applied = 1
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

func observedOrNow(value *time.Time) time.Time {
	if value != nil {
		return *value
	}
	return time.Now().UTC()
}

func applyPolicyOnly(ctx context.Context, tx pgx.Tx, m EarnPolicyOnlyMutation) error {
	route, err := upsertPolicy(ctx, tx, m.RoutePolicy)
	if err != nil {
		return err
	}
	setup, err := upsertPolicy(ctx, tx, m.SetupPolicy)
	if err != nil {
		return err
	}
	if _, err = upsertVaultWithSetup(ctx, tx, route, setup, m.RoutePolicy); err != nil {
		return err
	}
	routeSlot, err := slotBigint(m.RoutePolicy.Slot)
	if err != nil {
		return err
	}
	setupSeed, err := policySeedBigint(m.SetupPolicy.PolicySeed)
	if err != nil {
		return err
	}
	setupSlot, err := slotBigint(m.SetupPolicy.Slot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
        UPDATE loyal_yield.earn_deposit_onboarding_attempts
        SET route_policy_db_id = $1,
            route_policy_signature = $2,
            route_policy_confirmed_slot = $3,
            setup_policy_id = $4,
            setup_policy_account = $5,
            setup_policy_seed = $6,
            setup_policy_db_id = $7,
            setup_policy_signature = $8,
            setup_policy_confirmed_slot = $9,
            status = 'setup_policy_confirmed',
            last_error_code = NULL,
            updated_at = now()
        WHERE settings = $10 AND vault_index = $11 AND vault_pubkey = $12
          AND status <> 'complete'`, route, m.RoutePolicy.Signature, routeSlot, setupSeed, m.SetupPolicy.PolicyAccount, setupSeed, setup,
		m.SetupPolicy.Signature, setupSlot, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.RoutePolicy.VaultPubkey)
	return err
}

type managedVault struct{ id, activePolicyID int64 }

func applyDeposit(ctx context.Context, tx pgx.Tx, m EarnDepositMutation) error {
	route, err := upsertPolicy(ctx, tx, m.RoutePolicy)
	if err != nil {
		return err
	}
	var vaultID int64
	if m.SetupPolicy != nil {
		setup, err := upsertPolicy(ctx, tx, *m.SetupPolicy)
		if err != nil {
			return err
		}
		vaultID, err = upsertVaultWithSetup(ctx, tx, route, setup, m.RoutePolicy)
		if err != nil {
			return err
		}
	} else if vaultID, err = upsertVault(ctx, tx, route, m.RoutePolicy); err != nil {
		return err
	}
	var vault managedVault
	if err := tx.QueryRow(ctx, `SELECT id, active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1`, vaultID).Scan(&vault.id, &vault.activePolicyID); err != nil {
		return err
	}
	confirmedSlot, err := slotBigint(m.DepositSlot)
	if err != nil {
		return err
	}
	observedSlot, err := slotBigint(m.ObservedSlot)
	if err != nil {
		return err
	}
	amount, err := amountBigint(m.PrincipalAmountRaw)
	if err != nil {
		return err
	}
	var current uint64
	for _, reserve := range m.ReserveState {
		if reserve.LiquidityMint == m.LiquidityMint {
			if current+reserve.AmountRaw < current {
				return errors.New("Earn holding amount overflow")
			}
			current += reserve.AmountRaw
		}
	}
	for _, idle := range m.IdleState {
		if idle.Mint == m.LiquidityMint {
			if current+idle.AmountRaw < current {
				return errors.New("Earn holding amount overflow")
			}
			current += idle.AmountRaw
		}
	}
	currentAmount, err := amountBigint(current)
	if err != nil {
		return err
	}
	seed, err := policySeedBigint(m.RoutePolicy.PolicySeed)
	if err != nil {
		return err
	}
	observedAt := observedOrNow(m.ObservedAt)
	var depositID int64
	err = tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.user_yield_position_deposits (
            deposit_signature, policy_signature, confirmed_slot, wallet_address,
            smart_account_address, settings, vault_index, vault_pubkey, policy_id,
            policy_account, policy_seed, target_reserve, market, liquidity_mint,
            target_supply_apy_bps, deposit_mint, principal_amount_raw, confirmed_at,
            created_at
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
            $15, $16, $17, $18, now()
        )
        ON CONFLICT (deposit_signature) DO NOTHING
        RETURNING id`, m.DepositSignature, m.RoutePolicy.Signature, confirmedSlot, m.Wallet, m.SmartAccountAddress, m.RoutePolicy.Settings,
		int16(m.RoutePolicy.VaultIndex), m.RoutePolicy.VaultPubkey, route, m.RoutePolicy.PolicyAccount, seed, m.TargetReserve, m.Market,
		m.LiquidityMint, m.TargetSupplyAPYBPS, m.DepositMint, amount, observedAt).Scan(&depositID)
	if errors.Is(err, pgx.ErrNoRows) {
		// This exact deposit was already committed: do not republish its old
		// account snapshot and risk replacing newer vault state.
		return nil
	}
	if err != nil {
		return err
	}
	// The position identity is unique across repeated Earn lifecycles; a
	// closed row is reactivated rather than colliding with the initial index.
	var existing struct {
		id                   int64
		reserve, mint        string
		market               *string
		amount, observedSlot int64
		observedAt           time.Time
	}
	err = tx.QueryRow(ctx, `
            SELECT id, current_reserve, current_market, current_liquidity_mint,
                   current_amount_raw, current_observed_slot, current_observed_at
            FROM loyal_yield.user_yield_positions
            WHERE settings = $1 AND vault_index = $2
              AND wallet_address = $3 AND vault_pubkey = $4
            ORDER BY updated_at DESC, id DESC
            LIMIT 1
            FOR UPDATE`, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.Wallet, m.RoutePolicy.VaultPubkey).
		Scan(&existing.id, &existing.reserve, &existing.market, &existing.mint, &existing.amount, &existing.observedSlot, &existing.observedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var positionID, resulting, holdingDelta, eventSlot int64
	var eventType, eventReserve, eventMint string
	var eventMarket *string
	var eventAt time.Time
	if err == nil {
		current := observedSlot >= existing.observedSlot
		eventReserve, eventMarket, eventMint, resulting, eventSlot, eventAt = existing.reserve, existing.market, existing.mint, existing.amount, existing.observedSlot, existing.observedAt
		if current {
			eventReserve, eventMarket, eventMint, resulting, eventSlot, eventAt = m.TargetReserve, m.Market, m.LiquidityMint, currentAmount, observedSlot, observedAt
		}
		holdingDelta = resulting - existing.amount
		if _, err := tx.Exec(ctx, `
                UPDATE loyal_yield.user_yield_positions
                SET wallet_address = $2,
                    smart_account_address = $3,
                    vault_pubkey = $4,
                    policy_id = $5,
                    policy_account = $6,
                    policy_seed = $7,
                    principal_amount_raw = principal_amount_raw + $8,
                    last_deposit_signature = $9,
                    last_confirmed_slot = GREATEST(last_confirmed_slot, $10),
                    status = 'active'::loyal_yield.yield_position_status,
                    current_reserve = $11,
                    current_market = $12,
                    current_liquidity_mint = $13,
                    current_amount_raw = $14,
                    current_observed_slot = $15,
                    current_observed_at = $16,
                    updated_at = now()
                WHERE id = $1`, existing.id, m.Wallet, m.SmartAccountAddress, m.RoutePolicy.VaultPubkey, route, m.RoutePolicy.PolicyAccount, seed,
			amount, m.DepositSignature, confirmedSlot, eventReserve, eventMarket, eventMint, resulting, eventSlot, eventAt); err != nil {
			return err
		}
		positionID, eventType = existing.id, "deposit_top_up"
	} else {
		if err := tx.QueryRow(ctx, `
                INSERT INTO loyal_yield.user_yield_positions (
                    wallet_address, smart_account_address, settings, vault_index,
                    vault_pubkey, policy_id, policy_account, policy_seed,
                    initial_reserve, initial_market, initial_liquidity_mint,
                    initial_supply_apy_bps, deposit_mint, principal_amount_raw,
                    first_deposit_signature, last_deposit_signature,
                    last_confirmed_slot, status, created_at, updated_at,
                    current_reserve, current_market, current_liquidity_mint,
                    current_amount_raw, current_observed_slot, current_observed_at
                ) VALUES (
                    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                    $14, $15, $15, $16, 'active'::loyal_yield.yield_position_status,
                    now(), now(), $9, $10, $11, $18, $19, $17
                ) RETURNING id`, m.Wallet, m.SmartAccountAddress, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.RoutePolicy.VaultPubkey,
			route, m.RoutePolicy.PolicyAccount, seed, m.TargetReserve, m.Market, m.LiquidityMint, m.TargetSupplyAPYBPS, m.DepositMint, amount,
			m.DepositSignature, confirmedSlot, observedAt, currentAmount, observedSlot).Scan(&positionID); err != nil {
			return err
		}
		eventType, eventReserve, eventMarket, eventMint = "deposit_initialized", m.TargetReserve, m.Market, m.LiquidityMint
		resulting, holdingDelta, eventSlot, eventAt = currentAmount, currentAmount, observedSlot, observedAt
	}
	var holdingID int64
	if err := tx.QueryRow(ctx, `
            INSERT INTO loyal_yield.user_yield_position_holding_events (
                position_id, event_type, reserve, market, liquidity_mint,
                amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
                observed_at, source_signature, source_deposit_id, created_at
            ) VALUES (
                $1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4,
                $5, $6, $7, $8, $9, $10, $11, $12, now()
            ) RETURNING id`, positionID, eventType, eventReserve, eventMarket, eventMint, resulting, amount, holdingDelta, eventSlot, eventAt,
		m.DepositSignature, depositID).Scan(&holdingID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.user_yield_positions SET last_holding_event_id = $2 WHERE id = $1`, positionID, holdingID); err != nil {
		return err
	}
	return applyObservedBalances(ctx, tx, vault, m.ReserveState, m.IdleState, observedSlot, observedAt)
}

func applyObservedBalances(ctx context.Context, tx pgx.Tx, vault managedVault, reserves []EarnReserveMutation, idles []EarnIdleTokenMutation, observedSlot int64, observedAt time.Time) error {
	var current int64
	err := tx.QueryRow(ctx, `SELECT observed_slot FROM loyal_yield.vault_position_snapshots WHERE vault_id = $1 AND is_current FOR UPDATE`, vault.id).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && current > observedSlot {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.vault_position_snapshots SET is_current = FALSE WHERE vault_id = $1 AND is_current`, vault.id); err != nil {
		return err
	}
	var snapshotID int64
	if err := tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.vault_position_snapshots
            (vault_id, policy_id, observed_slot, observed_at, chain_slot, context)
        VALUES ($1, $2, $3, $4, $3, jsonb_build_object('kind', 'earn_laserstream_direct'))
        RETURNING id`, vault.id, vault.activePolicyID, observedSlot, observedAt).Scan(&snapshotID); err != nil {
		return err
	}
	observedReserves := make([]string, 0, len(reserves))
	for _, reserve := range reserves {
		observedReserves = append(observedReserves, reserve.Reserve)
	}
	if _, err := tx.Exec(ctx, `
        DELETE FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = $1
          AND observed_slot <= $3
          AND NOT (reserve = ANY($2::text[]))`, vault.id, observedReserves, observedSlot); err != nil {
		return err
	}
	idleMints := make([]string, 0, len(idles))
	for _, idle := range idles {
		idleMints = append(idleMints, idle.Mint)
	}
	if _, err := tx.Exec(ctx, `
        DELETE FROM loyal_yield.vault_idle_token_balances_current
        WHERE vault_id = $1
          AND observed_slot <= $3
          AND NOT (mint = ANY($2::text[]))`, vault.id, idleMints, observedSlot); err != nil {
		return err
	}
	for _, reserve := range reserves {
		amount, err := amountBigint(reserve.AmountRaw)
		if err != nil {
			return err
		}
		metadata := []byte(reserve.PlanningMetadata)
		if len(metadata) == 0 {
			metadata = []byte("null")
		}
		// Zero rows stay out of history; see reconcile_vault_transaction_guarded.
		if amount > 0 {
			if _, err := tx.Exec(ctx, `
                INSERT INTO loyal_yield.vault_position_snapshot_positions
                    (snapshot_id, reserve, market, liquidity_mint, amount_raw,
                     supply_apy_bps, borrow_apy_bps, has_value, planning_metadata)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, snapshotID, reserve.Reserve, reserve.Market, reserve.LiquidityMint, amount,
				reserve.SupplyAPYBPS, reserve.BorrowAPYBPS, reserve.HasValue, metadata); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO loyal_yield.vault_reserve_positions_current
                (vault_id, reserve, market, liquidity_mint, amount_raw, has_value,
                 supply_apy_bps, borrow_apy_bps, snapshot_id, observed_slot,
                 observed_at, planning_metadata)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
            ON CONFLICT (vault_id, reserve) DO UPDATE SET
                market = EXCLUDED.market,
                liquidity_mint = EXCLUDED.liquidity_mint,
                amount_raw = EXCLUDED.amount_raw,
                has_value = EXCLUDED.has_value,
                supply_apy_bps = EXCLUDED.supply_apy_bps,
                borrow_apy_bps = EXCLUDED.borrow_apy_bps,
                snapshot_id = EXCLUDED.snapshot_id,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                planning_metadata = EXCLUDED.planning_metadata`, vault.id, reserve.Reserve, reserve.Market, reserve.LiquidityMint, amount,
			reserve.HasValue, reserve.SupplyAPYBPS, reserve.BorrowAPYBPS, snapshotID, observedSlot, observedAt, metadata); err != nil {
			return err
		}
	}
	for _, idle := range idles {
		amount, err := amountBigint(idle.AmountRaw)
		if err != nil {
			return err
		}
		slot, err := slotBigint(idle.ObservedSlot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO loyal_yield.vault_idle_token_balances_current
                (vault_id, mint, amount_raw, owner, token_account, observed_slot,
                 observed_at, source_commitment, updated_at)
            VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), $8, now())
            ON CONFLICT (vault_id, mint) DO UPDATE SET
                amount_raw = EXCLUDED.amount_raw,
                owner = EXCLUDED.owner,
                token_account = EXCLUDED.token_account,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                source_commitment = EXCLUDED.source_commitment,
                updated_at = now()`, vault.id, idle.Mint, amount, idle.Owner, idle.TokenAccount, slot, idle.ObservedAt, idle.SourceCommitment); err != nil {
			return err
		}
	}
	return nil
}

func applyWithdrawal(ctx context.Context, tx pgx.Tx, m EarnWithdrawalMutation) error {
	// Withdrawals must not reactivate a retired policy during historical replay.
	confirmedSlot, err := slotBigint(m.ConfirmedSlot)
	if err != nil {
		return err
	}
	observedSlot, err := slotBigint(m.ObservedSlot)
	if err != nil {
		return err
	}
	withdrawn, err := amountBigint(m.WithdrawnAmountRaw)
	if err != nil {
		return err
	}
	remaining, err := amountBigint(m.RemainingAmountRaw)
	if err != nil {
		return err
	}
	observedAt := observedOrNow(m.ObservedAt)
	var position struct {
		id, principal, amount, observedSlot, policyID, policySeed int64
		status, policyAccount                                     string
		settled, laterDeposits                                    bool
	}
	err = tx.QueryRow(ctx, `
        SELECT position.id, position.principal_amount_raw, position.current_amount_raw,
               position.current_observed_slot, position.status::text AS status,
               owning_deposit.policy_id, owning_deposit.policy_account, owning_deposit.policy_seed,
               EXISTS (
                   SELECT 1 FROM loyal_yield.user_yield_position_holding_events boundary
                   WHERE boundary.position_id = position.id
                     AND boundary.observed_slot >= $5 AND boundary.amount_raw = 0
                     AND boundary.event_type::text IN ('snapshot_reconciled', 'withdrawal_full')
               ) AS historically_settled,
               EXISTS (
                   SELECT 1 FROM loyal_yield.user_yield_position_deposits later_deposit
                   WHERE later_deposit.settings = position.settings
                     AND later_deposit.vault_index = position.vault_index
                     AND later_deposit.vault_pubkey = position.vault_pubkey
                     AND later_deposit.wallet_address = position.wallet_address
                     AND later_deposit.confirmed_slot >= $5
               ) AS has_later_deposits
        FROM loyal_yield.user_yield_positions position
        JOIN loyal_yield.user_yield_position_deposits first_deposit
          ON first_deposit.deposit_signature = position.first_deposit_signature
         AND first_deposit.settings = position.settings
         AND first_deposit.vault_index = position.vault_index
         AND first_deposit.vault_pubkey = position.vault_pubkey
         AND first_deposit.wallet_address = position.wallet_address
        JOIN LATERAL (
            SELECT deposit.policy_id, deposit.policy_account, deposit.policy_seed
            FROM loyal_yield.user_yield_position_deposits deposit
            WHERE deposit.settings = position.settings
              AND deposit.vault_index = position.vault_index
              AND deposit.vault_pubkey = position.vault_pubkey
              AND deposit.wallet_address = position.wallet_address
              AND deposit.confirmed_slot <= $5
            ORDER BY deposit.confirmed_slot DESC, deposit.id DESC
            LIMIT 1
        ) owning_deposit ON TRUE
        WHERE position.settings = $1 AND position.vault_index = $2
          AND position.wallet_address = $3 AND position.vault_pubkey = $4
          AND first_deposit.confirmed_slot <= $5
        ORDER BY first_deposit.confirmed_slot DESC, position.id DESC
        LIMIT 1
        FOR UPDATE OF position`, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.Wallet, m.VaultPubkey, confirmedSlot).
		Scan(&position.id, &position.principal, &position.amount, &position.observedSlot, &position.status, &position.policyID,
			&position.policyAccount, &position.policySeed, &position.settled, &position.laterDeposits)
	if errors.Is(err, pgx.ErrNoRows) {
		var replayed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_withdrawals WHERE withdrawal_signature = $1)`, m.WithdrawalSignature).Scan(&replayed); err != nil {
			return err
		}
		if replayed {
			return nil
		}
		return fmt.Errorf("confirmed Earn withdrawal %s has no projected lifecycle at its slot", m.WithdrawalSignature)
	}
	if err != nil {
		return err
	}
	historyOnly := position.status != "active" || position.settled
	if !historyOnly && position.laterDeposits {
		return fmt.Errorf("historical Earn withdrawal %s has later deposits without a proven settlement boundary", m.WithdrawalSignature)
	}
	mode := "partial"
	if remaining == 0 {
		mode = "full"
	}
	var withdrawalID int64
	err = tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.user_yield_position_withdrawals (
            withdrawal_signature, confirmed_slot, wallet_address,
            smart_account_address, settings, vault_index, vault_pubkey, policy_id,
            policy_account, policy_seed, target_reserve, market, liquidity_mint,
            withdrawn_amount_raw, source_type, source_id, source_metadata,
            reserve_withdrawals, mode, confirmed_at, created_at
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
            'chain_snapshot', $11, jsonb_build_object('kind', 'earn_laserstream_confirmed'),
            '[]'::jsonb, $15, $16, now()
        )
        ON CONFLICT (withdrawal_signature) DO NOTHING
        RETURNING id`, m.WithdrawalSignature, confirmedSlot, m.Wallet, m.VaultPubkey, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex),
		m.VaultPubkey, position.policyID, position.policyAccount, position.policySeed, m.TargetReserve, m.Market, m.LiquidityMint, withdrawn, mode, observedAt).Scan(&withdrawalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The withdrawal landing in the wallet is no Autodeposit inflow.
	if err := autodeposit.SuppressWithdrawalLots(ctx, tx, m.WithdrawalSignature, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.VaultPubkey); err != nil {
		return err
	}
	// A reused position row whose later zero-balance boundary already settled
	// the old principal gets history only.
	if historyOnly {
		return nil
	}
	var vault managedVault
	if err := tx.QueryRow(ctx, `SELECT id, active_policy_id FROM loyal_yield.managed_vaults WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3`, m.RoutePolicy.Settings, int16(m.RoutePolicy.VaultIndex), m.VaultPubkey).Scan(&vault.id, &vault.activePolicyID); err != nil {
		return err
	}
	principalDelta := -min(position.principal, withdrawn)
	nextPrincipal := position.principal - withdrawn
	if position.principal < withdrawn && nextPrincipal > position.principal {
		nextPrincipal = -1 << 63 // saturating_sub at the BIGINT floor
	}
	current := observedSlot >= position.observedSlot
	resulting, eventSlot := position.amount, position.observedSlot
	if current {
		resulting, eventSlot = remaining, observedSlot
	}
	holdingDelta := resulting - position.amount
	eventType := "withdrawal_partial"
	if mode == "full" {
		eventType = "withdrawal_full"
	}
	var holdingID int64
	if err := tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id, event_type, reserve, market, liquidity_mint,
            amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
            observed_at, source_signature, source_withdrawal_id, created_at
        ) VALUES (
            $1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4, $5,
            $6, $7, $8, $9, $10, $11, $12, now()
        ) RETURNING id`, position.id, eventType, m.TargetReserve, m.Market, m.LiquidityMint, resulting, principalDelta, holdingDelta, eventSlot,
		observedAt, m.WithdrawalSignature, withdrawalID).Scan(&holdingID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE loyal_yield.user_yield_positions
        SET principal_amount_raw = $2,
            current_reserve = CASE WHEN $3 THEN $4 ELSE current_reserve END,
            current_market = CASE WHEN $3 THEN $5 ELSE current_market END,
            current_liquidity_mint = CASE WHEN $3 THEN $6 ELSE current_liquidity_mint END,
            current_amount_raw = $7,
            current_observed_slot = $8,
            current_observed_at = $9,
            last_confirmed_slot = GREATEST(last_confirmed_slot, $10),
            last_holding_event_id = $11,
            updated_at = now()
        WHERE id = $1`, position.id, nextPrincipal, current, m.TargetReserve, m.Market, m.LiquidityMint, resulting, eventSlot, observedAt,
		confirmedSlot, holdingID); err != nil {
		return err
	}
	return applyObservedBalances(ctx, tx, vault, m.ReserveState, m.IdleState, observedSlot, observedAt)
}

func applyRefund(ctx context.Context, tx pgx.Tx, m EarnRefundMutation) error {
	cluster := ""
	switch m.Cluster {
	case "mainnet", "mainnet-beta":
		cluster = "mainnet-beta"
	case "devnet":
		cluster = "devnet"
	default:
		return fmt.Errorf("unsupported Earn refund cluster %q", m.Cluster)
	}
	slot, err := slotBigint(m.ConfirmedSlot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO loyal_yield.earn_chain_refund_events (
            cluster, settings, vault_index, vault_pubkey, wallet_address,
            refund_signature, confirmed_slot, refund_kind, confirmed_at, created_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
        ON CONFLICT (refund_signature) DO NOTHING`, cluster, m.Settings, int16(m.VaultIndex), m.VaultPubkey, m.Wallet, m.RefundSignature, slot,
		m.RefundKind, observedOrNow(m.ObservedAt))
	return err
}

func applyCleanup(ctx context.Context, tx pgx.Tx, m EarnCleanupMutation) error {
	slot, err := slotBigint(m.ConfirmedSlot)
	if err != nil {
		return err
	}
	observedAt := observedOrNow(m.ObservedAt)
	var vaultID, activePolicyID int64
	var setupPolicyID *int64
	err = tx.QueryRow(ctx, `
        SELECT id, active_policy_id, setup_policy_id
        FROM loyal_yield.managed_vaults
        WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3
        FOR UPDATE`, m.Settings, int16(m.VaultIndex), m.VaultPubkey).Scan(&vaultID, &activePolicyID, &setupPolicyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A historical close never closes a replacement policy or later deposit.
	var superseded bool
	if err := tx.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.route_policies
            WHERE (id = $1 OR id = $2) AND last_seen_slot > $3
        ) OR EXISTS (
            SELECT 1 FROM loyal_yield.user_yield_positions
            WHERE settings = $4 AND vault_index = $5 AND vault_pubkey = $6
              AND status::text = 'active'
              AND (last_confirmed_slot > $3
                   OR (current_observed_slot > $3 AND current_amount_raw > 0))
        )`, activePolicyID, setupPolicyID, slot, m.Settings, int16(m.VaultIndex), m.VaultPubkey).Scan(&superseded); err != nil {
		return err
	}
	if superseded {
		return nil
	}
	if _, err := tx.Exec(ctx, `
        UPDATE loyal_yield.route_policies
        SET active = FALSE,
            finalized_eligible = FALSE,
            last_seen_slot = GREATEST(last_seen_slot, $3),
            last_seen_signature = CASE WHEN $3 >= last_seen_slot THEN $4 ELSE last_seen_signature END,
            last_seen_at = now()
        WHERE id = $1 OR id = $2`, activePolicyID, setupPolicyID, slot, m.CleanupSignature); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE loyal_yield.vault_reserve_positions_current
        SET amount_raw = 0, has_value = FALSE, observed_slot = $2, observed_at = $3
        WHERE vault_id = $1 AND observed_slot <= $2`, vaultID, slot, observedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE loyal_yield.vault_idle_token_balances_current
        SET amount_raw = 0, observed_slot = $2, observed_at = $3, updated_at = now()
        WHERE vault_id = $1 AND observed_slot <= $2`, vaultID, slot, observedAt); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
        SELECT id, current_reserve, current_market, current_liquidity_mint,
               principal_amount_raw, current_amount_raw
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3
          AND status::text = 'active'
        FOR UPDATE`, m.Settings, int16(m.VaultIndex), m.VaultPubkey)
	if err != nil {
		return err
	}
	type activePosition struct {
		id, principal, current int64
		reserve, mint          string
		market                 *string
	}
	var positions []activePosition
	for rows.Next() {
		var p activePosition
		if err := rows.Scan(&p.id, &p.reserve, &p.market, &p.mint, &p.principal, &p.current); err != nil {
			rows.Close()
			return err
		}
		positions = append(positions, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range positions {
		var holdingID int64
		if err := tx.QueryRow(ctx, `
            INSERT INTO loyal_yield.user_yield_position_holding_events (
                position_id, event_type, reserve, market, liquidity_mint,
                amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
                observed_at, source_signature, created_at
            ) VALUES (
                $1, 'snapshot_reconciled'::loyal_yield.user_yield_holding_event_type,
                $2, $3, $4, 0, $5, $6, $7, $8, $9, now()
            ) RETURNING id`, p.id, p.reserve, p.market, p.mint, -p.principal, -p.current, slot, observedAt, m.CleanupSignature).Scan(&holdingID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            UPDATE loyal_yield.user_yield_positions
            SET principal_amount_raw = 0,
                current_amount_raw = 0,
                current_observed_slot = $2,
                current_observed_at = $3,
                last_holding_event_id = $4,
                status = 'closed'::loyal_yield.yield_position_status,
                updated_at = now()
            WHERE id = $1`, p.id, slot, observedAt, holdingID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET active = FALSE, last_seen_at = now() WHERE id = $1`, vaultID); err != nil {
		return err
	}
	// The vault's Autodeposit work had only this route as its destination.
	_, err = autodeposit.SkipUnroutedVaultSlots(ctx, tx, m.Settings, int16(m.VaultIndex), m.VaultPubkey)
	return err
}
