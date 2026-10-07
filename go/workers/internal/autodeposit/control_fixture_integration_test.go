package autodeposit

import (
	"context"
	"testing"
)

type countingControlReader struct {
	reader ControlReader
	calls  int
}

func (r *countingControlReader) ObserveControl(ctx context.Context, t ControlTarget, slot int64) (ControlObservation, error) {
	r.calls++
	return r.reader.ObserveControl(ctx, t, slot)
}

type fixedCreatorHistory struct {
	entries  map[string]ArtifactHistoryEntry
	receipts map[string]ArtifactReceipt
}

func (h fixedCreatorHistory) ArtifactHistory(_ context.Context, address string, _ int) ([]ArtifactHistoryEntry, error) {
	return []ArtifactHistoryEntry{h.entries[address]}, nil
}
func (h fixedCreatorHistory) ArtifactReceipt(_ context.Context, signature string) (ArtifactReceipt, error) {
	receipt, ok := h.receipts[signature]
	if !ok {
		return receipt, ErrArtifactCreationProofPending
	}
	return receipt, nil
}

// seedControlRuntime seeds one mainnet target whose policy and delegation
// match the golden artifact fixture, and a counting control reader over it.
func seedControlRuntime(t *testing.T, s *Store) (int64, *countingControlReader, *ArtifactProofReader) {
	t.Helper()
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	seeded := seedIntegrationTarget(t, s, "control")
	target.TargetID = seeded.TargetID
	seedProjectedSurplus(t, s, seeded, 1, 9_000_000)
	_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET settings=$2,authority=$3,policy_seed=$4,policy_account=$5,wallet=$6,wallet_usdc_ata=$7,wallet_token_ata=$7,vault_pubkey=$8,vault_usdc_ata=$9,vault_token_ata=$9,subscription_authority=$10,recurring_delegation=$11,recurring_delegation_nonce=$12,max_amount_per_period=$13,period_length_seconds=$14,start_timestamp=$15,recurring_delegation_expiry_timestamp=$16,setup_generation=$17 WHERE id=$1`, target.TargetID, target.Settings, target.RootAuthority, target.PolicySeed, target.Policy, target.Wallet, target.WalletTokenATA, target.Vault, target.VaultTokenATA, target.SubscriptionAuthority, target.RecurringDelegation, *target.Nonce, *target.MaxAmountPerPeriod, *target.PeriodLength, *target.StartTimestamp, *target.ExpiryTimestamp, target.SetupGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, seeded.ManagedVaultID, target.Settings, target.Vault); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,vault_pubkey=$4 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, seeded.ManagedVaultID, target.Settings, target.RootAuthority, target.Vault); err != nil {
		t.Fatal(err)
	}
	seedRepairPosition(t, s, target.TargetID, USDCMint)
	policy := goldenCreatorReceipt(t, f)
	f.WireBase64 = f.DelegationWireBase64
	f.Policy = f.RecurringDelegation
	delegation := goldenCreatorReceipt(t, f)
	history := fixedCreatorHistory{entries: map[string]ArtifactHistoryEntry{target.Policy: {Signature: policy.Signature, Slot: policy.Slot}, target.RecurringDelegation: {Signature: delegation.Signature, Slot: delegation.Slot}}, receipts: map[string]ArtifactReceipt{policy.Signature: policy, delegation.Signature: delegation}}
	return target.TargetID, &countingControlReader{reader: b}, &ArtifactProofReader{Wires: b, History: history}
}
