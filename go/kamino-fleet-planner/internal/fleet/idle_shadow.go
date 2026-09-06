package fleet

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// loadIdleShadowSources shares the reserve loader's read-only repeatable-read
// transaction and capacity frontier. Mirror the retained Rust idle-source
// ownership exclusions, including subscription pulls awaiting their top-up.
func loadIdleShadowSources(ctx context.Context, tx pgx.Tx, cluster, signer string, epoch ImmutableMarketEpoch) ([]FleetVault, error) {
	if signer == "" {
		return nil, fmt.Errorf("idle shadow requires a delegated signer")
	}
	mints := []string{}
	for _, coverage := range epoch.MintCoverage {
		mints = append(mints, coverage.Mint)
	}
	rows, err := tx.Query(ctx, `
SELECT v.id,v.settings,v.vault_index,v.vault_pubkey,r.id,r.policy_account,
       r.kamino_markets,i.mint,i.token_account,i.amount_raw,i.observed_slot,i.observed_at
FROM loyal_yield.managed_vaults v
JOIN loyal_yield.route_policies r ON r.id=v.active_policy_id
JOIN loyal_yield.vault_idle_token_balances_current i ON i.vault_id=v.id
WHERE v.active AND r.active AND r.cluster=$1
 AND r.source_commitment='finalized' AND r.finalized_eligible
 AND $2=ANY(r.delegated_signers) AND 'same_mint_kamino'=ANY(r.route_modes)
 AND cardinality(r.kamino_markets)>0
 AND i.amount_raw>0 AND i.mint=ANY($3::text[])
 AND i.mint=ANY(r.stable_mints) AND i.mint=ANY(r.kamino_liquidity_mints)
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities o
  WHERE o.cluster=$1 AND o.vault_id=v.id
   AND o.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created'))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=v.id
  AND d.status::text IN ('planned','simulating','ready','submitted','confirming'))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=v.id
  AND d.status::text='confirmed' AND d.source_reserve IS NULL AND d.liquidity_mint=i.mint
  AND d.updated_at>=transaction_timestamp()-interval '5 minutes')
 AND NOT EXISTS (
  SELECT 1 FROM loyal_yield.balance_sweep_lot_claims c
  JOIN loyal_yield.balance_sweep_targets t ON t.id=c.target_id AND t.token_mint=i.mint
  JOIN loyal_yield.managed_vaults owner ON owner.settings=t.settings
   AND owner.vault_index=t.vault_index AND owner.vault_pubkey=t.vault_pubkey AND owner.id=i.vault_id
  JOIN loyal_yield.balance_sweep_transaction_attempts pull ON pull.claim_token=c.claim_token
   AND pull.operation_kind='pull' AND pull.attempt_state IN ('prepared','submitted','confirmed','unknown','ambiguous')
  LEFT JOIN loyal_yield.balance_sweep_transaction_attempts topup ON topup.claim_token=c.claim_token
   AND topup.operation_kind='top_up' AND topup.attempt_state='confirmed'
  WHERE c.status='selected' AND topup.id IS NULL)
ORDER BY v.id,i.mint,i.token_account`, cluster, signer, mints)
	if err != nil {
		return nil, fmt.Errorf("load idle shadow sources: %w", err)
	}
	defer rows.Close()
	var out []FleetVault
	for rows.Next() {
		var v FleetVault
		var markets []string
		p := &v.Position
		if err := rows.Scan(&p.VaultID, &p.Settings, &p.VaultIndex, &p.VaultPubkey, &p.PolicyID, &p.PolicyAccount, &markets, &p.Mint, &v.IdleTokenAccount, &p.AmountRaw, &p.ObservedSlot, &p.ObservedAt); err != nil {
			return nil, err
		}
		if v.IdleTokenAccount == "" {
			return nil, fmt.Errorf("idle shadow source lacks token account")
		}
		for _, r := range epoch.Reserves {
			if r.TargetEligible && r.LiquidityMint == p.Mint && r.Market != nil && contains(markets, *r.Market) {
				v.AllowedTargets = append(v.AllowedTargets, r.Reserve)
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// idleShadowChecks reports independent candidate evaluations, NOT selected
// fleet moves. It does not allocate capacity or compete with reserve moves.
// Publishing/executing idle routes needs a separate retained-route integration.
func idleShadowChecks(snapshot MarketSnapshot, all []FleetVault) ([]FleetVault, map[string]any) {
	reserves := []FleetVault{}
	reserveIDs := map[int64]bool{}
	idleIDs := map[int64]bool{}
	for _, v := range all {
		if v.IdleTokenAccount == "" {
			reserves = append(reserves, v)
			reserveIDs[v.Position.VaultID] = true
		}
	}
	reasons := map[string]int{}
	sources, eligibleSources, candidates := 0, 0, 0
	for _, v := range all {
		if v.IdleTokenAccount == "" {
			continue
		}
		sources++
		idleIDs[v.Position.VaultID] = true
		eligible := false
		seen := map[string]bool{}
		if len(v.AllowedTargets) == 0 {
			seen["no_policy_eligible_target"] = true
		}
		for _, target := range v.AllowedTargets {
			p := v.Position
			p.TargetCommittedInflowUSDMicros = v.CommittedInflows[target]
			p.TargetCommittedOutflowUSDMicros = v.CommittedOutflows[target]
			d := planIdleDeposit(snapshot, p, target)
			seen[d.Reason] = true
			if d.Eligible {
				eligible = true
				candidates++
			}
		}
		if eligible {
			eligibleSources++
		}
		// Counts are per source/reason and may overlap across target alternatives.
		for reason := range seen {
			reasons[reason]++
		}
	}
	idleOnly := 0
	for id := range idleIDs {
		if !reserveIDs[id] {
			idleOnly++
		}
	}
	return reserves, map[string]any{"event": "kamino_fleet_idle_shadow_checks", "mode": ModeShadow, "idleSourceCount": sources, "idleVaultCount": len(idleIDs), "idleOnlyVaultCount": idleOnly, "combinedSourceVaultCount": len(reserveIDs) + idleOnly, "eligibleIdleSourceCount": eligibleSources, "eligibleIdleCandidateCount": candidates, "sourceReasonCounts": reasons, "publishedCount": 0, "candidateChecksOnly": true}
}
