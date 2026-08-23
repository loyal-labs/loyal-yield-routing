-- Autoswap starts from confirmed account state. Finalized remains eligible as
-- the stronger commitment. Ordinary Earn route-policy finality is unchanged.
ALTER TABLE loyal_yield.cross_mint_swap_policies
    DROP CONSTRAINT IF EXISTS cross_mint_swap_policies_observation_check;

ALTER TABLE loyal_yield.cross_mint_swap_policies
    ADD CONSTRAINT cross_mint_swap_policies_observation_check CHECK (
        last_mutation IN ('create', 'update', 'remove', 'ambiguous')
        AND source_commitment IN ('processed', 'confirmed', 'finalized')
        AND last_seen_slot >= 0
        AND (
            NOT start_eligible OR (
                active
                AND source_commitment IN ('confirmed', 'finalized')
                AND last_mutation IN ('create', 'update')
            )
        )
    );

UPDATE loyal_yield.cross_mint_swap_policies
SET start_eligible = active
    AND source_commitment IN ('confirmed', 'finalized')
    AND last_mutation IN ('create', 'update')
WHERE start_eligible IS DISTINCT FROM (
    active
    AND source_commitment IN ('confirmed', 'finalized')
    AND last_mutation IN ('create', 'update')
);

INSERT INTO loyal_yield.cross_mint_vault_opt_ins
    (cluster, settings, vault_index, vault_pubkey, enabled)
SELECT policy.cluster, policy.settings, policy.vault_index, policy.vault_pubkey, TRUE
FROM loyal_yield.cross_mint_swap_policies AS policy
WHERE policy.active
  AND policy.start_eligible
  AND policy.source_commitment IN ('confirmed', 'finalized')
  AND policy.last_mutation IN ('create', 'update')
  AND policy.source_shard IN ('classic', 'token_2022')
GROUP BY
    policy.cluster,
    policy.settings,
    policy.vault_index,
    policy.vault_pubkey
HAVING count(*) = 2
   AND count(DISTINCT policy.source_shard) = 2
   AND count(DISTINCT policy.authority) = 1
   AND count(DISTINCT policy.delegated_signer) = 1
   AND count(DISTINCT policy.max_slippage_bps) = 1
   AND count(DISTINCT policy.daily_source_mint_spending_cap) = 1
ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO NOTHING;

COMMENT ON TABLE loyal_yield.cross_mint_swap_policies IS
    'Current decoded Autoswap policy accounts. Confirmed or finalized active canonical pairs are eligible.';
