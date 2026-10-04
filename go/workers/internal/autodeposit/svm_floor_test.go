package autodeposit

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
)

// This exercises the durable publication boundary, not controller bootstrap:
// canonical authorization creation and its signed proof execute in the paired
// SVM test. A mutable protection floor is deliberately a database control; the
// wallet-authorized canonical Squads policy does not bake it into its ABI.
func TestSVMCurrentFloorMutationRejectsActualSignedPullBeforeBroadcast(t *testing.T) {
	f := loadSVMAutodepositFixture(t)
	svm := startAutodepositSVM(t, f)
	chain, err := NewRPCChain(svm.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := base64.StdEncoding.DecodeString(f.ExecutorSeedBase64)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := NewSweepWireBuilder(setupProxy(t), ed25519.NewKeyFromSeed(seed), chain.ReadAccountsWithOptional)
	if err != nil {
		t.Fatal(err)
	}
	store := integrationStore(t)
	seeded, claim, slot := selectedReleaseClaim(t, store, "svm-floor", f.AmountRaw)
	ctx := t.Context()
	// Bind the DB admission identity to the actual source-produced accounts.
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, seeded.ManagedVaultID, f.Settings, f.Vault); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET settings=$2,authority=$3,wallet=$3,vault_pubkey=$4,policy_seed=$5,policy_account=$6,wallet_usdc_ata=$7,wallet_token_ata=$7,vault_usdc_ata=$8,vault_token_ata=$8,wallet_balance_floor_raw=400000 WHERE id=$1`, seeded.TargetID, f.Settings, f.Wallet, f.Vault, f.PolicySeed, f.Policy, f.WalletATA, f.VaultATA); err != nil {
		t.Fatal(err)
	}
	// Keep the active route policy in the same admission namespace as its vault
	// and target so this fixture reaches the protection-floor boundary.
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,vault_pubkey=$4 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, seeded.ManagedVaultID, f.Settings, f.Wallet, f.Vault); err != nil {
		t.Fatal(err)
	}
	plan := svmPullPlan(f)
	plan.Target.ID, plan.Target.ManagedVaultID = seeded.TargetID, seeded.ManagedVaultID
	if _, err = store.FreezeDepositPlan(ctx, claim, "lease-current", plan); err != nil {
		t.Fatal(err)
	}
	if _, err = builder.ConfirmTopUpRoute(ctx, plan); err != nil {
		t.Fatal(err)
	}
	wire, err := builder.BuildPull(ctx, PullWireRequest{Plan: plan, RecurringDelegation: f.RecurringDelegation, RecentBlockhash: svm.blockhash, LastValidBlockHeight: 1150})
	if err != nil {
		t.Fatal(err)
	}
	before, err := chain.ConfirmedTokenBalanceRaw(ctx, f.WalletATA, f.Wallet)
	if err != nil || before != f.WalletBeforeRaw {
		t.Fatalf("actual source balance=%d err=%v", before, err)
	}
	prepared := PreparedAttempt{ClaimToken: claim, TargetID: seeded.TargetID, ScheduledSlotID: slot, OperationKind: OperationPull, AmountRaw: f.AmountRaw, SourcePreBalanceRaw: before, ProtectionFloorRaw: ptrInt64(400_000), Signature: wire.Signature, SignedTransactionBase64: wire.SignedTransactionBase64, SignedTransactionSHA256: wire.SignedTransactionSHA256, RecentBlockhash: wire.RecentBlockhash, LastValidBlockHeight: wire.LastValidBlockHeight}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=$2 WHERE id=$1`, seeded.TargetID, before-f.AmountRaw+1); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PersistPreparedAttempt(ctx, prepared, "lease-current"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("changed live floor admitted old signed intent: %v", err)
	}
	prepared.ProtectionFloorRaw = ptrInt64(before - f.AmountRaw + 1)
	if _, err = store.PersistPreparedAttempt(ctx, prepared, "lease-current"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("matching new floor admitted below-floor spend: %v", err)
	}
	var count int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token=$1`, claim).Scan(&count); err != nil || count != 0 {
		t.Fatalf("floor refusal published %d attempts: %v", count, err)
	}
	for _, want := range []struct {
		account, owner string
		amount         int64
	}{{f.WalletATA, f.Wallet, before}, {f.VaultATA, f.Vault, 0}} {
		if balance, e := chain.ConfirmedTokenBalanceRaw(ctx, want.account, want.owner); e != nil || balance != want.amount {
			t.Fatalf("floor refusal moved real funds: %d %v", balance, e)
		}
	}
	// Prove the same wire and lease are otherwise admissible at the exact
	// inclusive boundary. It is persisted, deliberately never broadcast here.
	prepared.ProtectionFloorRaw = ptrInt64(before - f.AmountRaw)
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=$2 WHERE id=$1`, seeded.TargetID, *prepared.ProtectionFloorRaw); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.PersistPreparedAttempt(ctx, prepared, "lease-current")
	if err != nil || attempt.Signature != wire.Signature || attempt.BroadcastCount != 0 {
		t.Fatalf("valid inclusive floor refused exact intent: %+v %v", attempt, err)
	}
}
