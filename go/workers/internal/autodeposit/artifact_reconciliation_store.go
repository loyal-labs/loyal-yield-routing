package autodeposit

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const artifactTargetColumns = controlTargetColumns + `,authority,period_length_seconds,recurring_delegation_expiry_timestamp,policy_signature,recurring_delegation_signature,policy_confirmed_slot,recurring_delegation_confirmed_slot`

func scanArtifactTarget(row pgx.Row) (ArtifactTarget, error) {
	var t ArtifactTarget
	err := row.Scan(&t.TargetID, &t.SetupGeneration, &t.PolicySeed, &t.Settings, &t.Wallet, &t.WalletTokenATA, &t.Vault, &t.VaultTokenATA, &t.Mint, &t.Policy, &t.SubscriptionAuthority, &t.RecurringDelegation, &t.Nonce, &t.MaxAmountPerPeriod, &t.StartTimestamp, &t.Cluster, &t.RootAuthority, &t.PeriodLength, &t.ExpiryTimestamp, &t.PolicySignature, &t.DelegationSignature, &t.PolicyConfirmedSlot, &t.DelegationConfirmedSlot)
	return t, err
}
func (s *Store) LoadArtifactTarget(ctx context.Context, targetID int64) (*ArtifactTarget, error) {
	t, err := scanArtifactTarget(s.pool.QueryRow(ctx, `SELECT `+artifactTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE id=$1 AND cluster='mainnet-beta' AND subscription_authority IS NOT NULL AND recurring_delegation IS NOT NULL AND recurring_delegation_nonce IS NOT NULL AND max_amount_per_period>0 AND period_length_seconds>0 AND start_timestamp IS NOT NULL AND recurring_delegation_expiry_timestamp IS NOT NULL`, targetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func sameArtifactIdentity(a, b ArtifactTarget) bool {
	return reflect.DeepEqual(a.ControlTarget, b.ControlTarget) && a.RootAuthority == b.RootAuthority && reflect.DeepEqual(a.PeriodLength, b.PeriodLength) && reflect.DeepEqual(a.ExpiryTimestamp, b.ExpiryTimestamp)
}

// Only verified creator receipts may fill the existing 0059 signature/slot
// columns. Recorded pairs stay authoritative; partial contradictory pairs are
// held rather than completed with unrelated history. No lifecycle or desired
// control fields are changed.
func (s *Store) BackfillArtifactCreationProof(ctx context.Context, request ReconciliationRequest, owner string, proof VerifiedArtifactCreationProof) error {
	if proof.target.TargetID != request.TargetID || proof.signature == "" || proof.slot <= 0 || !sha256HexPattern.MatchString(proof.instructionSHA256) {
		return errors.New("artifact creator proof identity incomplete")
	}
	if proof.role != ArtifactPolicy && proof.role != ArtifactDelegation {
		return errors.New("artifact creator proof role invalid")
	}
	expected := proof.target.Policy
	if proof.role == ArtifactDelegation {
		expected = proof.target.RecurringDelegation
	}
	if proof.account != expected {
		return errors.New("artifact creator proof names another account")
	}
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		target, err := scanArtifactTarget(tx.QueryRow(ctx, `SELECT `+artifactTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, request.TargetID))
		if err != nil {
			return err
		}
		var owned int64
		if err = tx.QueryRow(ctx, `SELECT target_id FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now() FOR UPDATE`, request.TargetID, owner).Scan(&owned); errors.Is(err, pgx.ErrNoRows) {
			return ErrOwnershipLost
		} else if err != nil {
			return err
		}
		if target.Cluster != mainnetCluster {
			return ErrChainNamespace
		}
		if !sameArtifactIdentity(target, proof.target) {
			return errors.New("artifact target identity or generation changed before backfill")
		}
		var closed bool
		if err = tx.QueryRow(ctx, `SELECT chain_status='closed' FROM loyal_yield.balance_sweep_targets WHERE id=$1`, request.TargetID).Scan(&closed); err != nil {
			return err
		}
		if closed {
			return ErrArtifactCreationProofPending
		}
		signature, slot := target.PolicySignature, target.PolicyConfirmedSlot
		if proof.role == ArtifactDelegation {
			signature, slot = target.DelegationSignature, target.DelegationConfirmedSlot
		}
		if signature != nil && *signature != proof.signature || slot != nil && *slot != proof.slot {
			return errors.New("artifact creator proof contradicts retained stage evidence")
		}
		query := `UPDATE loyal_yield.balance_sweep_targets SET policy_signature=COALESCE(policy_signature,$2),policy_confirmed_slot=COALESCE(policy_confirmed_slot,$3),last_seen_at=now()WHERE id=$1`
		if proof.role == ArtifactDelegation {
			query = `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation_signature=COALESCE(recurring_delegation_signature,$2),recurring_delegation_confirmed_slot=COALESCE(recurring_delegation_confirmed_slot,$3),last_seen_at=now()WHERE id=$1`
		}
		_, err = tx.Exec(ctx, query, request.TargetID, proof.signature, proof.slot)
		return err
	})
}
