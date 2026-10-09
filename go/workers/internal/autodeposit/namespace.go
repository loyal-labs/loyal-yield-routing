package autodeposit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// This family is composed with the verified mainnet RPC. A missing namespace
// is not evidence that a retained signed transaction belongs to that chain.
const mainnetCluster = "mainnet-beta"

var ErrChainNamespace = errors.New("autodeposit chain namespace is unsupported or unknown")

func (s *Store) requireMainnetTarget(ctx context.Context, id int64) error {
	var matches bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(cluster='mainnet-beta',false) FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return ErrChainNamespace
	}
	return nil
}

func (s *Store) requireMainnetClaim(ctx context.Context, token string) error {
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT target_id FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1`, token).Scan(&id); err != nil {
		return err
	}
	return s.requireMainnetTarget(ctx, id)
}

// Called after locking the target and before inserting a new packet. Existing
// signed packets do not require a current policy, but still require their
// target's known chain before any reconciliation RPC is allowed.
func lockMainnetPolicy(ctx context.Context, tx pgx.Tx, id int64) error {
	var policy int64
	err := tx.QueryRow(ctx, `SELECT rp.id FROM loyal_yield.balance_sweep_targets target
JOIN loyal_yield.managed_vaults mv ON mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey AND mv.active
JOIN loyal_yield.route_policies rp ON rp.id=mv.active_policy_id AND rp.active AND rp.cluster='mainnet-beta'
AND rp.authority=target.authority AND rp.settings=target.settings AND rp.vault_index=target.vault_index AND rp.vault_pubkey=target.vault_pubkey
WHERE target.id=$1 AND target.cluster='mainnet-beta' AND 'same_mint_kamino'=ANY(rp.route_modes)
FOR SHARE OF mv,rp`, id).Scan(&policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChainNamespace
	}
	return err
}
