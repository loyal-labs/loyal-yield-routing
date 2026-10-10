package earn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Policy projection writers, ported from loyal-yield-store store.rs
// (record_policy_match, record_setup_policy_match,
// record_balance_sweep_policy_match, record_cross_mint_swap_policy_manifest,
// record_policy_removal, record_autodeposit_recurring_delegation). The SQL is
// unchanged so the stopped Rust writer can resume on the same rows.

func strings0(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func policySeedBigint(seed uint64) (int64, error) {
	if seed > math.MaxInt64 {
		return 0, fmt.Errorf("policy seed %d exceeds PostgreSQL BIGINT", seed)
	}
	return int64(seed), nil
}

// upsertPolicy returns the route_policies id.
func upsertPolicy(ctx context.Context, tx pgx.Tx, event PolicyMatchInput) (int64, error) {
	if _, err := commitmentRank(event.SourceCommitment); err != nil {
		return 0, err
	}
	slot, err := slotBigint(event.Slot)
	if err != nil {
		return 0, err
	}
	seed, err := policySeedBigint(event.PolicySeed)
	if err != nil {
		return 0, err
	}
	swapLanes := event.SwapLanes
	if len(swapLanes) == 0 {
		swapLanes = json.RawMessage(`null`)
	}
	var id int64
	err = tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature,
             cluster, source_commitment, finalized_eligible)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, TRUE, $16, $17, $18, $19, $20)
        ON CONFLICT (policy_account) DO UPDATE SET
            settings = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.settings ELSE loyal_yield.route_policies.settings END,
            authority = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.authority ELSE loyal_yield.route_policies.authority END,
            policy_seed = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.policy_seed ELSE loyal_yield.route_policies.policy_seed END,
            vault_index = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.vault_index ELSE loyal_yield.route_policies.vault_index END,
            vault_pubkey = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.vault_pubkey ELSE loyal_yield.route_policies.vault_pubkey END,
            delegated_signers = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.delegated_signers ELSE loyal_yield.route_policies.delegated_signers END,
            threshold = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.threshold ELSE loyal_yield.route_policies.threshold END,
            route_modes = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.route_modes ELSE loyal_yield.route_policies.route_modes END,
            stable_mints = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.stable_mints ELSE loyal_yield.route_policies.stable_mints END,
            kamino_markets = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.kamino_markets ELSE loyal_yield.route_policies.kamino_markets END,
            kamino_liquidity_mints = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.kamino_liquidity_mints ELSE loyal_yield.route_policies.kamino_liquidity_mints END,
            universe_preset = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.universe_preset ELSE loyal_yield.route_policies.universe_preset END,
            risk_profile = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.risk_profile ELSE loyal_yield.route_policies.risk_profile END,
            swap_lanes = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.swap_lanes ELSE loyal_yield.route_policies.swap_lanes END,
            cluster = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.cluster ELSE loyal_yield.route_policies.cluster END,
            source_commitment = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN EXCLUDED.source_commitment
                ELSE loyal_yield.route_policies.source_commitment
            END,
            finalized_eligible = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN EXCLUDED.finalized_eligible
                ELSE loyal_yield.route_policies.finalized_eligible
            END,
            active = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN TRUE
                ELSE loyal_yield.route_policies.active
            END,
            last_seen_at = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN now()
                ELSE loyal_yield.route_policies.last_seen_at
            END,
            last_seen_slot = GREATEST(loyal_yield.route_policies.last_seen_slot, EXCLUDED.last_seen_slot),
            last_seen_signature = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.last_seen_signature ELSE loyal_yield.route_policies.last_seen_signature END
        RETURNING id`,
		event.Settings, event.Authority, seed, event.PolicyAccount, int16(event.VaultIndex), event.VaultPubkey,
		strings0(event.DelegatedSigners), int32(event.Threshold), strings0(event.RouteModes), strings0(event.StableMints),
		strings0(event.KaminoMarkets), strings0(event.KaminoLiquidityMints), event.UniversePreset, event.RiskProfile,
		[]byte(swapLanes), slot, event.Signature, event.Cluster, event.SourceCommitment, event.SourceCommitment == "finalized",
	).Scan(&id)
	return id, err
}

const managedVaultReplace = `
                WHEN (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = EXCLUDED.active_policy_id
                ) > (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = loyal_yield.managed_vaults.active_policy_id
                )`

// upsertVault returns the managed_vaults id.
func upsertVault(ctx context.Context, tx pgx.Tx, policyID int64, event PolicyMatchInput) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id, active)
        VALUES ($1, $2, $3, $4, TRUE)
        ON CONFLICT (settings, vault_index, vault_pubkey) DO UPDATE SET
            active_policy_id = CASE`+managedVaultReplace+`
                THEN EXCLUDED.active_policy_id
                ELSE loyal_yield.managed_vaults.active_policy_id
            END,
            active = CASE`+managedVaultReplace+`
                THEN TRUE
                ELSE loyal_yield.managed_vaults.active
            END,
            last_seen_at = CASE`+managedVaultReplace+`
                THEN now()
                ELSE loyal_yield.managed_vaults.last_seen_at
            END
        RETURNING id`, event.Settings, int16(event.VaultIndex), event.VaultPubkey, policyID).Scan(&id)
	return id, err
}

func upsertVaultWithSetup(ctx context.Context, tx pgx.Tx, routePolicyID, setupPolicyID int64, event PolicyMatchInput) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id, setup_policy_id, active)
        VALUES ($1, $2, $3, $4, $5, TRUE)
        ON CONFLICT (settings, vault_index, vault_pubkey) DO UPDATE SET
            active_policy_id = EXCLUDED.active_policy_id,
            setup_policy_id = EXCLUDED.setup_policy_id,
            active = TRUE,
            last_seen_at = now()
        RETURNING id`, event.Settings, int16(event.VaultIndex), event.VaultPubkey, routePolicyID, setupPolicyID).Scan(&id)
	return id, err
}

// RecordPolicyMatch projects one detected yield route policy.
func (s *Store) RecordPolicyMatch(ctx context.Context, event PolicyMatchInput) error {
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, err := upsertPolicy(ctx, tx, event)
		if err != nil {
			return err
		}
		_, err = upsertVault(ctx, tx, id, event)
		return err
	})
}

// RecordSetupPolicyMatch attaches a setup policy to its existing vault.
func (s *Store) RecordSetupPolicyMatch(ctx context.Context, event PolicyMatchInput) error {
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, err := upsertPolicy(ctx, tx, event)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
            UPDATE loyal_yield.managed_vaults
            SET setup_policy_id = $1,
                active = TRUE,
                last_seen_at = now()
            WHERE settings = $2
              AND vault_index = $3
              AND vault_pubkey = $4`, id, event.Settings, int16(event.VaultIndex), event.VaultPubkey)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("setup policy %s arrived before its route policy", event.PolicyAccount)
		}
		return nil
	})
}

// RecordBalanceSweepPolicyMatch projects one Autodeposit policy, rolling a
// target whose policy account changed.
func (s *Store) RecordBalanceSweepPolicyMatch(ctx context.Context, event BalanceSweepPolicyMatchInput) error {
	seed, err := policySeedBigint(event.PolicySeed)
	if err != nil {
		return err
	}
	slot, err := slotBigint(event.Slot)
	if err != nil {
		return err
	}
	maxAmount, err := amountBigint(event.MaxAmountPerPeriod)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		lock := fmt.Sprintf("%s|%s|%d|%s", event.Settings, event.Wallet, event.VaultIndex, event.TokenMint)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lock); err != nil {
			return err
		}
		var currentID, observation int64
		var currentPolicy string
		err := tx.QueryRow(ctx, `
            SELECT id, policy_account,
                   GREATEST(last_seen_slot, chain_observation_slot) AS observation_slot
            FROM loyal_yield.balance_sweep_targets
            WHERE settings = $1
              AND wallet = $2
              AND vault_index = $3
              AND token_mint = $4
              AND chain_status <> 'closed'
            FOR UPDATE`, event.Settings, event.Wallet, int16(event.VaultIndex), event.TokenMint).Scan(&currentID, &currentPolicy, &observation)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && currentPolicy != event.PolicyAccount {
			if slot <= observation {
				// The newer target already owns this wallet; the older policy is
				// history.
				return nil
			}
			if _, err := tx.Exec(ctx, `
                    WITH closed_target AS (
                        UPDATE loyal_yield.balance_sweep_targets
                        SET desired_active = FALSE,
                            chain_status = 'closed',
                            chain_observation_slot = GREATEST(chain_observation_slot, $1),
                            closed_at = COALESCE(closed_at, now())
                        WHERE id = $2
                          AND chain_status <> 'closed'
                        RETURNING id
                    ), canceled_slots AS (
                        UPDATE loyal_yield.balance_sweep_scheduled_slots
                        SET status = 'canceled', updated_at = now()
                        WHERE target_id IN (SELECT id FROM closed_target)
                          AND status IN ('scheduled', 'requested')
                    )
                    UPDATE loyal_yield.balance_sweep_surplus_lots
                    SET status = 'suppressed', updated_at = now()
                    WHERE target_id IN (SELECT id FROM closed_target)
                      AND status = 'open'
                      AND remaining_amount_raw > 0`, slot, currentID); err != nil {
				return err
			}
		}
		newer := func(column string) string {
			return column + ` = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.` + column + `
                    ELSE loyal_yield.balance_sweep_targets.` + column + `
                END`
		}
		columns := []string{"settings", "authority", "vault_index", "vault_pubkey", "wallet", "wallet_usdc_ata", "vault_usdc_ata", "token_mint", "wallet_token_ata", "vault_token_ata", "delegated_signers", "threshold", "max_amount_per_period"}
		sets := make([]string, 0, len(columns))
		for _, column := range columns {
			sets = append(sets, newer(column))
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO loyal_yield.balance_sweep_targets
                (cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                 wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata,
                 vault_token_ata, delegated_signers, threshold, max_amount_per_period,
                 desired_active, chain_status, chain_observation_slot,
                 wallet_balance_floor_raw, last_seen_slot, last_seen_signature)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, $13, $14, $15, $16,
                TRUE, 'pending', $17, NULL, $17, $18)
            ON CONFLICT (policy_account) DO UPDATE SET
                cluster = COALESCE(
                    loyal_yield.balance_sweep_targets.cluster,
                    EXCLUDED.cluster
                ),
                `+strings.Join(sets, ",\n                ")+`,
                chain_status = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                         AND loyal_yield.balance_sweep_targets.chain_status <> 'closed'
                    THEN 'pending'
                    ELSE loyal_yield.balance_sweep_targets.chain_status
                END,
                chain_observation_slot = GREATEST(
                    loyal_yield.balance_sweep_targets.chain_observation_slot,
                    EXCLUDED.chain_observation_slot
                ),
                last_seen_at = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN now()
                    ELSE loyal_yield.balance_sweep_targets.last_seen_at
                END,
                last_seen_slot = GREATEST(loyal_yield.balance_sweep_targets.last_seen_slot, EXCLUDED.last_seen_slot),
                last_seen_signature = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.last_seen_signature
                    ELSE loyal_yield.balance_sweep_targets.last_seen_signature
                END`,
			event.Cluster, event.Settings, event.Authority, seed, event.PolicyAccount, int16(event.VaultIndex), event.VaultPubkey,
			event.Wallet, event.WalletUSDCATA, event.VaultUSDCATA, event.TokenMint, event.WalletTokenATA,
			event.VaultTokenATA, strings0(event.DelegatedSigners), int32(event.Threshold), maxAmount, slot, event.Signature)
		return err
	})
}

func validateCrossMintManifest(event CrossMintSwapPolicyManifestInput) error {
	if _, err := commitmentRank(event.SourceCommitment); err != nil {
		return err
	}
	if event.Mutation != "create" && event.Mutation != "update" {
		return fmt.Errorf("cross-mint manifest mutation must be create or update, got %q", event.Mutation)
	}
	if strings.TrimSpace(event.ManifestFingerprint) == "" || event.ManifestFingerprint != strings.TrimSpace(event.ManifestFingerprint) {
		return errors.New("cross-mint manifest fingerprint must be non-empty and trimmed")
	}
	if (event.SourceShard != "classic" && event.SourceShard != "token_2022") || event.MaxSlippageBPS == 0 || event.MaxSlippageBPS > 10_000 || event.DailySourceMintSpendingCap == 0 {
		return errors.New("cross-mint policy has an invalid source shard, slippage, or daily cap")
	}
	for _, value := range []string{event.Signature, event.Cluster, event.Settings, event.Authority, event.PolicyAccount, event.VaultPubkey, event.DelegatedSigner, event.SourceShard} {
		if value == "" {
			return errors.New("cross-mint manifest identity fields must be non-empty")
		}
	}
	return nil
}

type crossMintPolicyRow struct {
	settings, authority, lastMutation, sourceCommitment, lastSeenSignature string
	policySeed                                                             *int64
	vaultIndex                                                             *int16
	vaultPubkey, delegatedSigner, sourceShard, manifestFingerprint         *string
	maxSlippageBPS                                                         *int32
	dailyCap                                                               *int64
	active, startEligible                                                  bool
	lastSeenSlot                                                           int64
}

func fetchCrossMintPolicyForUpdate(ctx context.Context, tx pgx.Tx, cluster, policyAccount string) (*crossMintPolicyRow, error) {
	var row crossMintPolicyRow
	err := tx.QueryRow(ctx, `
        SELECT settings, authority, policy_seed, vault_index, vault_pubkey, delegated_signer,
               source_shard, max_slippage_bps, daily_source_mint_spending_cap,
               manifest_fingerprint, active, start_eligible, last_mutation,
               source_commitment, last_seen_slot, last_seen_signature
        FROM loyal_yield.cross_mint_swap_policies
        WHERE cluster = $1 AND policy_account = $2
        FOR UPDATE`, cluster, policyAccount).Scan(&row.settings, &row.authority, &row.policySeed, &row.vaultIndex, &row.vaultPubkey, &row.delegatedSigner,
		&row.sourceShard, &row.maxSlippageBPS, &row.dailyCap, &row.manifestFingerprint, &row.active, &row.startEligible, &row.lastMutation,
		&row.sourceCommitment, &row.lastSeenSlot, &row.lastSeenSignature)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func markCrossMintPolicyAmbiguous(ctx context.Context, tx pgx.Tx, cluster, policyAccount, commitment string, slot int64, signature string) (bool, error) {
	var startEligible bool
	err := tx.QueryRow(ctx, `
        UPDATE loyal_yield.cross_mint_swap_policies
        SET active = FALSE,
            start_eligible = FALSE,
            last_mutation = 'ambiguous',
            source_commitment = $3,
            last_seen_at = now(),
            last_seen_slot = GREATEST(last_seen_slot, $4),
            last_seen_signature = $5
        WHERE cluster = $1 AND policy_account = $2
        RETURNING start_eligible`, cluster, policyAccount, commitment, slot, signature).Scan(&startEligible)
	return startEligible, err
}

// RecordCrossMintSwapPolicyManifest projects one generalized Jupiter policy.
func (s *Store) RecordCrossMintSwapPolicyManifest(ctx context.Context, event CrossMintSwapPolicyManifestInput) error {
	if err := validateCrossMintManifest(event); err != nil {
		return err
	}
	slot, err := slotBigint(event.Slot)
	if err != nil {
		return err
	}
	var seed *int64
	if event.PolicySeed != nil {
		value, err := policySeedBigint(*event.PolicySeed)
		if err != nil {
			return err
		}
		seed = &value
	}
	dailyCap, err := amountBigint(event.DailySourceMintSpendingCap)
	if err != nil {
		return errors.New("cross-mint daily source-mint spending cap exceeds PostgreSQL BIGINT")
	}
	eventRank, err := commitmentRank(event.SourceCommitment)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0::bigint))`, "cross-mint-swap-policy:"+event.Cluster+":"+event.PolicyAccount); err != nil {
			return err
		}
		current, err := fetchCrossMintPolicyForUpdate(ctx, tx, event.Cluster, event.PolicyAccount)
		if err != nil {
			return err
		}
		startEligible := false
		switch {
		case current == nil:
			err = tx.QueryRow(ctx, `
                INSERT INTO loyal_yield.cross_mint_swap_policies
                    (cluster, settings, authority, policy_seed, policy_account,
                     vault_index, vault_pubkey, delegated_signer, source_shard,
                     max_slippage_bps, daily_source_mint_spending_cap,
                     manifest_fingerprint, active, start_eligible, last_mutation,
                     source_commitment, last_seen_slot, last_seen_signature)
                VALUES
                    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                     TRUE, $13, $14, $15, $16, $17)
                RETURNING start_eligible`, event.Cluster, event.Settings, event.Authority, seed, event.PolicyAccount,
				int16(event.VaultIndex), event.VaultPubkey, event.DelegatedSigner, event.SourceShard, int32(event.MaxSlippageBPS), dailyCap,
				event.ManifestFingerprint, eventRank >= 2, event.Mutation, event.SourceCommitment, slot, event.Signature).Scan(&startEligible)
			if err != nil {
				return err
			}
		case current.lastMutation == "remove":
			return nil
		case slot < current.lastSeenSlot:
			startEligible = current.startEligible
		default:
			same := slot == current.lastSeenSlot && event.Signature == current.lastSeenSignature &&
				eqString(current.manifestFingerprint, event.ManifestFingerprint) && eqString(current.sourceShard, event.SourceShard) &&
				event.Settings == current.settings && event.Authority == current.authority && eqInt64(current.policySeed, seed) &&
				current.vaultIndex != nil && *current.vaultIndex == int16(event.VaultIndex) && eqString(current.vaultPubkey, event.VaultPubkey) &&
				eqString(current.delegatedSigner, event.DelegatedSigner) && current.maxSlippageBPS != nil && *current.maxSlippageBPS == int32(event.MaxSlippageBPS) &&
				current.dailyCap != nil && *current.dailyCap == dailyCap && event.Mutation == current.lastMutation
			upgrade := false
			if same {
				currentRank, err := commitmentRank(current.sourceCommitment)
				if err != nil {
					return err
				}
				upgrade = eventRank > currentRank
			}
			switch {
			case upgrade:
				err = tx.QueryRow(ctx, `
                    UPDATE loyal_yield.cross_mint_swap_policies
                    SET source_commitment = $3,
                        start_eligible = active AND $3 IN ('confirmed', 'finalized')
                            AND last_mutation IN ('create', 'update'),
                        last_seen_at = now()
                    WHERE cluster = $1 AND policy_account = $2
                    RETURNING start_eligible`, event.Cluster, event.PolicyAccount, event.SourceCommitment).Scan(&startEligible)
				if err != nil {
					return err
				}
			case same:
				startEligible = current.startEligible
			default:
				stronger, err := strongerCommitment(event.SourceCommitment, current.sourceCommitment)
				if err != nil {
					return err
				}
				if startEligible, err = markCrossMintPolicyAmbiguous(ctx, tx, event.Cluster, event.PolicyAccount, stronger, max(slot, current.lastSeenSlot), event.Signature); err != nil {
					return err
				}
			}
		}
		if !startEligible {
			return nil
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO loyal_yield.cross_mint_vault_opt_ins
                (cluster, settings, vault_index, vault_pubkey, enabled)
            SELECT $1, $2, $3, $4, TRUE
            WHERE (
                SELECT count(*) = 2
                   AND count(DISTINCT source_shard) = 2
                   AND count(DISTINCT authority) = 1
                   AND count(DISTINCT delegated_signer) = 1
                   AND count(DISTINCT max_slippage_bps) = 1
                   AND count(DISTINCT daily_source_mint_spending_cap) = 1
                FROM loyal_yield.cross_mint_swap_policies
                WHERE cluster = $1 AND settings = $2
                  AND vault_index = $3 AND vault_pubkey = $4
                  AND active AND start_eligible
                  AND source_commitment IN ('confirmed', 'finalized')
                  AND last_mutation IN ('create', 'update')
                  AND source_shard IN ('classic', 'token_2022')
            )
            ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO NOTHING`, event.Cluster, event.Settings, int16(event.VaultIndex), event.VaultPubkey)
		return err
	})
}

func eqString(current *string, value string) bool { return current != nil && *current == value }

func eqInt64(current, value *int64) bool {
	return (current == nil) == (value == nil) && (current == nil || *current == *value)
}

// RecordPolicyRemoval closes a policy account of any family.
func (s *Store) RecordPolicyRemoval(ctx context.Context, event PolicyRemovalInput) error {
	eventRank, err := commitmentRank(event.SourceCommitment)
	if err != nil {
		return err
	}
	slot, err := slotBigint(event.Slot)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0::bigint))`, "cross-mint-swap-policy:"+event.Cluster+":"+event.PolicyAccount); err != nil {
			return err
		}
		if err := deactivateCrossMintSwapPolicy(ctx, tx, event, slot, eventRank); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            DELETE FROM loyal_yield.cross_mint_vault_opt_ins AS opt_in
            WHERE opt_in.cluster = $1
              AND opt_in.settings = $2
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.cross_mint_swap_policies AS active_policy
                  WHERE active_policy.cluster = opt_in.cluster
                    AND active_policy.settings = opt_in.settings
                    AND active_policy.vault_index = opt_in.vault_index
                    AND active_policy.vault_pubkey = opt_in.vault_pubkey
                    AND active_policy.active
              )
              AND 2 = (
                  SELECT count(DISTINCT removed_policy.source_shard)
                  FROM loyal_yield.cross_mint_swap_policies AS removed_policy
                  WHERE removed_policy.cluster = opt_in.cluster
                    AND removed_policy.settings = opt_in.settings
                    AND removed_policy.vault_index = opt_in.vault_index
                    AND removed_policy.vault_pubkey = opt_in.vault_pubkey
                    AND removed_policy.authority = $3
                    AND removed_policy.source_shard IN ('classic', 'token_2022')
                    AND removed_policy.last_mutation = 'remove'
                    AND NOT removed_policy.active
              )`, event.Cluster, event.Settings, event.Authority); err != nil {
			return err
		}
		var routePolicyID int64
		var routeVaultIndex *int16
		var routeVaultPubkey *string
		err := tx.QueryRow(ctx, `
            UPDATE loyal_yield.route_policies
            SET active = FALSE,
                finalized_eligible = FALSE,
                cluster = $1,
                source_commitment = $2,
                last_seen_at = now(),
                last_seen_slot = $3,
                last_seen_signature = $4
            WHERE policy_account = $5
              AND settings = $6
              AND authority = $7
              AND $3 >= last_seen_slot
            RETURNING id, vault_index, vault_pubkey`, event.Cluster, event.SourceCommitment, slot, event.Signature, event.PolicyAccount, event.Settings, event.Authority).Scan(&routePolicyID, &routeVaultIndex, &routeVaultPubkey)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := tx.Exec(ctx, `
                UPDATE loyal_yield.managed_vaults
                SET active = FALSE, last_seen_at = now()
                WHERE active_policy_id = $1 AND active`, routePolicyID); err != nil {
				return err
			}
			// A removed route policy leaves no Autodeposit target with a
			// sweep policy to close, so its vault's work ends here. The
			// targets stay active: the sweep policy itself was not removed.
			if routeVaultIndex != nil && routeVaultPubkey != nil {
				if _, err := autodeposit.SkipUnroutedVaultSlots(ctx, tx, event.Settings, *routeVaultIndex, *routeVaultPubkey); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `
            WITH closed_target AS (
            UPDATE loyal_yield.balance_sweep_targets
            SET chain_status = 'closed',
                chain_observation_slot = GREATEST(chain_observation_slot, $1),
                last_seen_at = now(),
                last_seen_slot = $1,
                last_seen_signature = $2
            WHERE policy_account = $3
              AND settings = $4
              AND authority = $5
              AND $1 >= chain_observation_slot
              AND chain_status <> 'closed'
            RETURNING id
            ), canceled_slots AS (
              UPDATE loyal_yield.balance_sweep_scheduled_slots
              SET status = 'canceled', updated_at = now()
              WHERE target_id IN (SELECT id FROM closed_target)
                AND status IN ('scheduled','requested')
            ), suppressed_lots AS (
              UPDATE loyal_yield.balance_sweep_surplus_lots
              SET status = 'suppressed', updated_at = now()
              WHERE target_id IN (SELECT id FROM closed_target)
                AND status = 'open' AND remaining_amount_raw > 0
            )
            SELECT EXISTS(SELECT 1 FROM closed_target)`, slot, event.Signature, event.PolicyAccount, event.Settings, event.Authority)
		return err
	})
}

func deactivateCrossMintSwapPolicy(ctx context.Context, tx pgx.Tx, event PolicyRemovalInput, slot int64, eventRank int) error {
	current, err := fetchCrossMintPolicyForUpdate(ctx, tx, event.Cluster, event.PolicyAccount)
	if err != nil {
		return err
	}
	if current == nil {
		_, err = tx.Exec(ctx, `
            INSERT INTO loyal_yield.cross_mint_swap_policies
                (cluster, settings, authority, policy_seed, policy_account,
                 vault_index, vault_pubkey, delegated_signer, source_shard,
                 max_slippage_bps, daily_source_mint_spending_cap,
                 manifest_fingerprint, active, start_eligible, last_mutation,
                 source_commitment, last_seen_slot, last_seen_signature)
            VALUES
                ($1, $2, $3, NULL, $4, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
                 FALSE, FALSE, 'remove', $5, $6, $7)`, event.Cluster, event.Settings, event.Authority, event.PolicyAccount, event.SourceCommitment, slot, event.Signature)
		return err
	}
	if slot < current.lastSeenSlot {
		return nil
	}
	if current.settings != event.Settings || current.authority != event.Authority ||
		(slot == current.lastSeenSlot && current.lastSeenSignature != event.Signature && current.lastMutation != "remove") {
		stronger, err := strongerCommitment(event.SourceCommitment, current.sourceCommitment)
		if err != nil {
			return err
		}
		_, err = markCrossMintPolicyAmbiguous(ctx, tx, event.Cluster, event.PolicyAccount, stronger, slot, event.Signature)
		return err
	}
	if current.lastMutation == "remove" && slot == current.lastSeenSlot && current.lastSeenSignature == event.Signature {
		currentRank, err := commitmentRank(current.sourceCommitment)
		if err != nil {
			return err
		}
		if eventRank <= currentRank {
			return nil
		}
	}
	_, err = tx.Exec(ctx, `
        UPDATE loyal_yield.cross_mint_swap_policies
        SET active = FALSE,
            start_eligible = FALSE,
            last_mutation = 'remove',
            source_commitment = $3,
            last_seen_at = now(),
            last_seen_slot = $4,
            last_seen_signature = $5
        WHERE cluster = $1 AND policy_account = $2`, event.Cluster, event.PolicyAccount, event.SourceCommitment, slot, event.Signature)
	return err
}

func upsertAutodepositReconciliationRequest(ctx context.Context, tx pgx.Tx, targetID, requestedSlot int64) error {
	_, err := tx.Exec(ctx, `
        INSERT INTO loyal_yield.autodeposit_reconciliation_requests
            (target_id, requested_slot)
        VALUES ($1, $2)
        ON CONFLICT (target_id) DO UPDATE SET
            requested_slot = EXCLUDED.requested_slot,
            next_attempt_at = LEAST(
                loyal_yield.autodeposit_reconciliation_requests.next_attempt_at,
                NOW()
            ),
            updated_at = NOW()
        WHERE EXCLUDED.requested_slot
            >= loyal_yield.autodeposit_reconciliation_requests.requested_slot`, targetID, requestedSlot)
	return err
}

// RecordRecurringDelegation projects one confirmed Subscriptions recurring
// delegation onto its Autodeposit target and requests reconciliation.
func (s *Store) RecordRecurringDelegation(ctx context.Context, input RecurringDelegationObserved) error {
	slot, err := slotBigint(input.Slot)
	if err != nil {
		return err
	}
	var values [3]int64
	for i, raw := range []uint64{input.Nonce, input.AmountPerPeriod, input.PeriodLengthSeconds} {
		if values[i], err = amountBigint(raw); err != nil {
			return err
		}
	}
	nonce, amount, period := values[0], values[1], values[2]
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, input.Wallet+"|"+input.VaultPubkey); err != nil {
			return err
		}
		// A durable transaction can arrive after its setup lifecycle closed:
		// resolve the immutable delegation owner first so a historical replay
		// cannot attach to the wallet's current target.
		var targetID int64
		var wallet, vault, chainStatus string
		var authority *string
		var existingNonce, existingSlot *int64
		err := tx.QueryRow(ctx, `
            SELECT id, wallet, vault_pubkey, subscription_authority,
                   recurring_delegation_nonce, recurring_delegation_confirmed_slot,
                   chain_status
            FROM loyal_yield.balance_sweep_targets
            WHERE recurring_delegation = $1
            FOR UPDATE`, input.RecurringDelegation).Scan(&targetID, &wallet, &vault, &authority, &existingNonce, &existingSlot, &chainStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if wallet != input.Wallet || vault != input.VaultPubkey || (authority != nil && *authority != input.SubscriptionAuthority) || (existingNonce != nil && *existingNonce != nonce) {
				return fmt.Errorf("recurring delegation %s is assigned to target %d with conflicting immutable ownership", input.RecurringDelegation, targetID)
			}
			if existingSlot == nil || slot >= *existingSlot {
				if err := tx.QueryRow(ctx, `
                    UPDATE loyal_yield.balance_sweep_targets
                    SET subscription_authority = $1,
                        recurring_delegation_nonce = $2,
                        max_amount_per_period = $3,
                        period_length_seconds = $4,
                        start_timestamp = $5,
                        recurring_delegation_expiry_timestamp = $6,
                        recurring_delegation_signature = $7,
                        recurring_delegation_confirmed_slot = $8,
                        chain_status = CASE
                            WHEN chain_status = 'closed' OR $8 <= chain_observation_slot
                            THEN chain_status
                            ELSE 'pending'
                        END,
                        chain_observation_slot = GREATEST(chain_observation_slot, $8),
                        last_seen_at = CASE WHEN $8 > last_seen_slot THEN now() ELSE last_seen_at END,
                        last_seen_slot = GREATEST(last_seen_slot, $8),
                        last_seen_signature = CASE
                            WHEN $8 > last_seen_slot THEN $7 ELSE last_seen_signature
                        END
                    WHERE id = $9
                    RETURNING chain_status`, input.SubscriptionAuthority, nonce, amount, period, input.StartTimestamp, input.ExpiryTimestamp, input.Signature, slot, targetID).Scan(&chainStatus); err != nil {
					return err
				}
			}
			if chainStatus != "closed" {
				return upsertAutodepositReconciliationRequest(ctx, tx, targetID, slot)
			}
			return nil
		}
		var currentSlot *int64
		err = tx.QueryRow(ctx, `
            SELECT id, recurring_delegation_confirmed_slot
            FROM loyal_yield.balance_sweep_targets
            WHERE wallet = $1
              AND vault_pubkey = $2
              AND chain_status <> 'closed'
            FOR UPDATE`, input.Wallet, input.VaultPubkey).Scan(&targetID, &currentSlot)
		if errors.Is(err, pgx.ErrNoRows) {
			// The stream projects in chain order and the app confirms the
			// Autodeposit policy before it builds the delegation, so a wallet
			// and vault without a target is not an Autodeposit delegation.
			return nil
		}
		if err != nil {
			return err
		}
		if currentSlot != nil && slot < *currentSlot {
			return nil
		}
		if _, err := tx.Exec(ctx, `
            UPDATE loyal_yield.balance_sweep_targets
            SET setup_generation = CASE
                    WHEN recurring_delegation IS NOT NULL
                         AND recurring_delegation IS DISTINCT FROM $2
                    THEN setup_generation + 1
                    ELSE setup_generation
                END,
                subscription_authority = $1,
                recurring_delegation = $2,
                recurring_delegation_nonce = $3,
                max_amount_per_period = $4,
                period_length_seconds = $5,
                start_timestamp = $6,
                recurring_delegation_expiry_timestamp = $7,
                recurring_delegation_signature = $8,
                recurring_delegation_confirmed_slot = $9,
                chain_status = CASE WHEN chain_status = 'closed' THEN chain_status ELSE 'pending' END,
                chain_observation_slot = GREATEST(chain_observation_slot, $9),
                last_seen_at = now(),
                last_seen_slot = GREATEST(last_seen_slot, $9),
                last_seen_signature = CASE WHEN $9 >= last_seen_slot THEN $8 ELSE last_seen_signature END
            WHERE id = $10`, input.SubscriptionAuthority, input.RecurringDelegation, nonce, amount, period, input.StartTimestamp, input.ExpiryTimestamp, input.Signature, slot, targetID); err != nil {
			return err
		}
		return upsertAutodepositReconciliationRequest(ctx, tx, targetID, slot)
	})
}
