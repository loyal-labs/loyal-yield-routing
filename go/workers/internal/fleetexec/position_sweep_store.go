package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// loadPositionSweepCohort is Rust load_fleet_position_sweep_vaults. The
// never-observed and oldest-observed vaults go first, so a restart does not
// replay low database ids forever; the order is frozen for one sweep.
func (s *Store) loadPositionSweepCohort(ctx context.Context, signer string, enabledMints []string) ([]positionSweepVault, error) {
	rows, err := s.pool.Query(ctx, `
SELECT v.id, v.settings, v.vault_index
FROM loyal_yield.managed_vaults v
JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
LEFT JOIN (
    SELECT position.vault_id, max(position.observed_at) AS last_observed_at
    FROM loyal_yield.vault_reserve_positions_current position
    GROUP BY position.vault_id
) observed_position ON observed_position.vault_id = v.id
WHERE v.active = TRUE
  AND p.active = TRUE
  AND $1 = ANY(p.delegated_signers)
  AND p.route_modes && $2::TEXT[]
  AND p.stable_mints && $3::TEXT[]
  AND p.kamino_liquidity_mints && $3::TEXT[]
  AND cardinality(p.kamino_markets) > 0
ORDER BY observed_position.last_observed_at ASC NULLS FIRST, v.id`, signer, []string{positionSweepSameMintMode, positionSweepFixedKaminoMode}, enabledMints)
	if err != nil {
		return nil, fmt.Errorf("load position sweep cohort: %w", err)
	}
	defer rows.Close()
	var vaults []positionSweepVault
	for rows.Next() {
		var vault positionSweepVault
		if err := rows.Scan(&vault.id, &vault.settings, &vault.vaultIndex); err != nil {
			return nil, err
		}
		vaults = append(vaults, vault)
	}
	return vaults, rows.Err()
}

type positionSweepCatalogAddress struct {
	address, semanticClass, accountRole string
	writable                            bool
}

type positionSweepCatalogHead struct {
	revisionID                         int64
	sourceSlot                         *int64
	readiness, enabledMintsHash        string
	activeGeneration, targetGeneration *int32
	addresses                          []positionSweepCatalogAddress
}

// loadPositionSweepCatalogHead is Rust shared_market_catalog_head without a
// lock: the active shared-market family's sealed head revision and its
// ordered manifest addresses.
func (s *Store) loadPositionSweepCatalogHead(ctx context.Context, cluster string) (positionSweepCatalogHead, error) {
	var head positionSweepCatalogHead
	rows, err := s.pool.Query(ctx, `
SELECT family.active_generation, head.catalog_revision_id, head.target_generation,
       head.readiness_state::text, revision.manifest_id, revision.enabled_mints_hash,
       revision.address_count, revision.source_slot
FROM loyal_yield.lookup_table_families family
JOIN loyal_yield.lookup_table_shared_market_catalog_heads head ON head.family_id = family.id
JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
  ON revision.id = head.catalog_revision_id AND revision.family_id = family.id
JOIN loyal_yield.lookup_table_manifests manifest
  ON manifest.id = revision.manifest_id AND manifest.family_id = family.id
 AND manifest.subject_kind = 'shared_market' AND manifest.sealed_at IS NOT NULL
WHERE family.cluster = $1 AND family.kind = 'shared_market' AND family.desired_state = 'active'`, cluster)
	if err != nil {
		return head, sweepTransport(fmt.Errorf("load shared-market catalog head: %w", err))
	}
	var manifestID int64
	var addressCount int32
	found := 0
	for rows.Next() {
		found++
		if err := rows.Scan(&head.activeGeneration, &head.revisionID, &head.targetGeneration, &head.readiness, &manifestID, &head.enabledMintsHash, &addressCount, &head.sourceSlot); err != nil {
			rows.Close()
			return head, sweepTransport(err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return head, sweepTransport(err)
	}
	if found > 1 {
		return head, sweepInvariant("cluster %q has multiple active shared-market catalog heads", cluster)
	}
	if found == 0 {
		return head, sweepInvariant("fleet position sweep requires a durable shared-market catalog head")
	}
	addressRows, err := s.pool.Query(ctx, `SELECT address, semantic_class::text, account_role, is_writable FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal`, manifestID)
	if err != nil {
		return head, sweepTransport(err)
	}
	defer addressRows.Close()
	for addressRows.Next() {
		var address positionSweepCatalogAddress
		if err := addressRows.Scan(&address.address, &address.semanticClass, &address.accountRole, &address.writable); err != nil {
			return head, sweepTransport(err)
		}
		head.addresses = append(head.addresses, address)
	}
	if err := addressRows.Err(); err != nil {
		return head, sweepTransport(err)
	}
	if int(addressCount) != len(head.addresses) {
		return head, sweepInvariant("shared-market catalog manifest %d address count drifted", manifestID)
	}
	return head, nil
}

// loadPositionSweepVault is Rust load_active_vault: the vault active under an
// active policy at this settings and index, with its policy cohort fields.
func (s *Store) loadPositionSweepVault(ctx context.Context, settings string, vaultIndex int16) (*positionSweepPolicyVault, error) {
	var vault positionSweepPolicyVault
	err := s.pool.QueryRow(ctx, `
SELECT v.id, v.active_policy_id, v.vault_pubkey, p.delegated_signers, p.route_modes,
       p.stable_mints, p.kamino_markets, p.kamino_liquidity_mints
FROM loyal_yield.managed_vaults v
JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
WHERE v.settings = $1 AND v.vault_index = $2 AND v.active = TRUE AND p.active = TRUE`, settings, vaultIndex).Scan(
		&vault.id, &vault.policyID, &vault.vaultPubkey, &vault.signers, &vault.modes, &vault.stableMints, &vault.markets, &vault.kaminoMints)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &vault, nil
}

// heldPositionReserves are the vault's current reserves that hold value.
func (s *Store) heldPositionReserves(ctx context.Context, vaultID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT reserve FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1 AND (has_value OR amount_raw > 0) ORDER BY reserve`, vaultID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// publishSweptVault is Rust publish_complete_vault (store.rs
// reconcile_vault_transaction_guarded, CompleteProductVault scope) under the
// projector contract publishSameMintPost shares: every writer serializes on
// the managed-vault row and publishes only a strictly newer slot than the
// vault's current snapshot, position and idle rows. An older or equal
// observation changes nothing and is reported stale.
//
// A complete publication replaces the vault's current set: every observed
// reserve is written, zero amounts included (a fresh zero row is the drained
// evidence Autodeposit reads), current rows for unobserved reserves are
// deleted, and the six Earn idle balances replace the idle set.
func (s *Store) publishSweptVault(ctx context.Context, vaultID, policyID int64, vaultPubkey string, o positionSweepObservation) (positionSweepOutcome, string, error) {
	if o.slot <= 0 || len(o.positions) == 0 || len(o.idle) != len(fleet.EarnStableMints()) {
		return "", "", sweepInvariant("complete product vault publication requires positions and the full Earn idle set")
	}
	contextJSON, err := o.contextJSON()
	if err != nil {
		return "", "", sweepInvariant("encode snapshot context: %v", err)
	}
	outcome, reason := positionSweepRefreshed, ""
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var active bool
		var currentPolicy int64
		var currentPubkey string
		err := tx.QueryRow(ctx, `SELECT active, active_policy_id, vault_pubkey FROM loyal_yield.managed_vaults WHERE id = $1 FOR UPDATE`, vaultID).Scan(&active, &currentPolicy, &currentPubkey)
		if err != nil {
			return err
		}
		if !active || currentPolicy != policyID || currentPubkey != vaultPubkey {
			outcome, reason = positionSweepSuperseded, "vault was deactivated or its policy replaced during the refresh"
			return nil
		}
		var frontier int64
		if err := tx.QueryRow(ctx, `SELECT GREATEST(COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_position_snapshots WHERE vault_id=$1 AND is_current),0),COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1),0),COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=$1),0))`, vaultID).Scan(&frontier); err != nil {
			return err
		}
		if frontier >= o.slot {
			outcome = positionSweepStale
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.vault_position_snapshots SET is_current = FALSE WHERE vault_id = $1 AND is_current`, vaultID); err != nil {
			return err
		}
		var snapshotID int64
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,chain_slot,is_current,context) VALUES($1,$2,$3,now(),$3,TRUE,$4) RETURNING id`, vaultID, policyID, o.slot, contextJSON).Scan(&snapshotID); err != nil {
			return err
		}
		observed := make([]string, 0, len(o.positions))
		for _, p := range o.positions {
			observed = append(observed, p.reserve)
			metadata, err := json.Marshal(map[string]any{
				"source": "chain_reconcile_preview", "amount_semantics": positionSweepAmountSemantics,
				"source_collateral_amount_raw": fmt.Sprint(p.amount), "redeemable_source_liquidity_amount_raw": fmt.Sprint(p.redeemable),
				"redeemable_liquidity_amount_raw": fmt.Sprint(p.redeemable), "obligation": p.obligation, "obligation_exists": p.obligationExists,
				"vault_liquidity_ata": p.ata, "vault_liquidity_token_account_exists": p.ataExists,
				"idle_vault_liquidity_amount_raw": fmt.Sprint(p.vaultLiquidity), "vault_liquidity_amount_raw": fmt.Sprint(p.vaultLiquidity),
			})
			if err != nil {
				return err
			}
			// History keeps only funded reserves; the current row records a zero.
			if p.amount > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_position_snapshot_positions(snapshot_id,reserve,market,liquidity_mint,amount_raw,supply_apy_bps,borrow_apy_bps,has_value,planning_metadata) VALUES($1,$2,$3,$4,$5,NULL,NULL,TRUE,$6)`, snapshotID, p.reserve, p.market, p.mint, p.amount, metadata); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,borrow_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata)
SELECT $1,$2,$3,$4,$5::bigint,$5::bigint>0,NULL,NULL,s.id,s.observed_slot,s.observed_at,$7 FROM loyal_yield.vault_position_snapshots s WHERE s.id=$6
ON CONFLICT(vault_id,reserve) DO UPDATE SET market=EXCLUDED.market,liquidity_mint=EXCLUDED.liquidity_mint,amount_raw=EXCLUDED.amount_raw,has_value=EXCLUDED.has_value,supply_apy_bps=EXCLUDED.supply_apy_bps,borrow_apy_bps=EXCLUDED.borrow_apy_bps,snapshot_id=EXCLUDED.snapshot_id,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,planning_metadata=EXCLUDED.planning_metadata`, vaultID, p.reserve, p.market, p.mint, p.amount, snapshotID, metadata); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1 AND NOT (reserve = ANY($2))`, vaultID, observed); err != nil {
			return err
		}
		if o.idleTotal != nil && o.idleTotal.Sign() == 0 {
			if err := closeZeroUserYieldPositions(ctx, tx, vaultID, snapshotID); err != nil {
				return err
			}
		}
		mints := make([]string, 0, len(o.idle))
		for _, idle := range o.idle {
			mints = append(mints, idle.mint)
			if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_idle_token_balances_current(vault_id,mint,amount_raw,owner,token_account,observed_slot,observed_at,source_commitment,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now())
ON CONFLICT(vault_id,mint) DO UPDATE SET amount_raw=EXCLUDED.amount_raw,owner=EXCLUDED.owner,token_account=EXCLUDED.token_account,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,source_commitment=EXCLUDED.source_commitment,updated_at=now()`, vaultID, idle.mint, idle.amount, vaultPubkey, idle.tokenAccount, o.slot, o.observedAt, positionSweepSourceCommitment); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id = $1 AND NOT (mint = ANY($2::TEXT[]))`, vaultID, mints)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return outcome, reason, nil
}

// closeZeroUserYieldPositions is Rust close_zero_user_yield_positions_for_vault
// for a sweep snapshot whose recorded idle total is zero: with no deposited
// collateral and no idle liquidity the funds left the vault, so its active
// app positions observed at or before this slot close. A nonzero idle total
// is a rebalance in flight and closes nothing (the caller skips it).
func closeZeroUserYieldPositions(ctx context.Context, tx pgx.Tx, vaultID, snapshotID int64) error {
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL`).Scan(&ready); err != nil || !ready {
		return err
	}
	var total int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_raw), 0)::bigint FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1`, vaultID).Scan(&total); err != nil {
		return err
	}
	if total > 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
WITH vault AS (
    SELECT v.settings, v.vault_index, v.vault_pubkey, s.observed_slot, s.observed_at
    FROM loyal_yield.managed_vaults v JOIN loyal_yield.vault_position_snapshots s ON s.vault_id = v.id
    WHERE v.id = $1 AND s.id = $2
), active_positions AS (
    SELECT p.id, p.current_reserve, p.current_market, p.current_liquidity_mint, p.current_amount_raw, p.principal_amount_raw
    FROM loyal_yield.user_yield_positions p, vault
    WHERE p.settings = vault.settings AND p.vault_index = vault.vault_index AND p.vault_pubkey = vault.vault_pubkey
      AND p.status::text = 'active' AND p.current_observed_slot <= vault.observed_slot
    FOR UPDATE OF p
), inserted_events AS (
    INSERT INTO loyal_yield.user_yield_position_holding_events (
        position_id, event_type, reserve, market, liquidity_mint, amount_raw,
        principal_delta_raw, holding_delta_raw, observed_slot, observed_at, source_snapshot_id, created_at)
    SELECT a.id, 'snapshot_reconciled'::loyal_yield.user_yield_holding_event_type, a.current_reserve, a.current_market,
           a.current_liquidity_mint, 0, -a.principal_amount_raw, -a.current_amount_raw, vault.observed_slot, vault.observed_at, $2, now()
    FROM active_positions a, vault
    RETURNING id, position_id
)
UPDATE loyal_yield.user_yield_positions p
SET smart_account_address = vault.vault_pubkey, principal_amount_raw = 0, current_amount_raw = 0,
    current_observed_slot = vault.observed_slot, current_observed_at = vault.observed_at,
    last_holding_event_id = inserted_events.id, status = 'closed'::loyal_yield.yield_position_status, updated_at = now()
FROM inserted_events, vault
WHERE p.id = inserted_events.position_id`, vaultID, snapshotID)
	return err
}
