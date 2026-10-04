package multiply

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func seedManagedCustody(t *testing.T, store *Store, state *RouteState, policy string, vault solana.PublicKey, index uint8) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var policyID, vaultID int64
	if err := store.Pool().QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,last_seen_slot,last_seen_signature) VALUES($1,$2,123,$3,$4,$5,1,500,'custody-fixture') RETURNING id`, state.Settings, fixtureKey(91).String(), policy, index, vault.String()).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id) VALUES($1,$2,$3,$4) RETURNING id`, state.Settings, index, vault.String(), policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Pool().Exec(ctx, "DELETE FROM loyal_yield.managed_vaults WHERE id=$1", vaultID); err != nil {
			t.Error(err)
		}
		if _, err := store.Pool().Exec(ctx, "DELETE FROM loyal_yield.route_policies WHERE id=$1", policyID); err != nil {
			t.Error(err)
		}
	})
	return vaultID, policyID
}

func TestFreshCustodyUsesExactVaultAndLegacyLock(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx := context.Background()
	// A different derived Squads account index coexists with Multiply0.
	otherVault, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), topology.Settings[:], []byte("smart_account"), {1}}, mustKey(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	seedManagedCustody(t, store, state, fixtureKey(93).String(), otherVault, 1)
	ownPolicy := topology.StrategyCatalog()[0].CollateralPolicy.Account.String()
	vaultID, policyID := seedManagedCustody(t, store, state, fixtureKey(94).String(), topology.Vault, topology.VaultIndex)
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "custody-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	op := integrationOperation(state.RouteKey, state.Cycle, lease.Version+1)
	state.Generation++
	state.CurrentOperationID = &op.OperationID
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err == nil || ok {
		t.Fatalf("foreign configuration admitted: %v %v", ok, err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.route_policies SET policy_account=$2 WHERE id=$1", policyID, ownPolicy); err != nil {
		t.Fatal(err)
	}
	// The unchanged Rust fleet holds this exact namespace. Even our own family
	// management row must wait for existing legacy handoff work to complete.
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mint := USDCMint
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0::bigint))", fmt.Sprintf("idle-vault-handoff:%d:%s", vaultID, mint)); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	ok, err := store.PrepareOperation(bounded, lease, state, op)
	cancel()
	if err == nil || ok {
		t.Fatalf("legacy lock was bypassed: %v %v", ok, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err != nil || !ok {
		t.Fatalf("own drained custody or unrelated index rejected: %v %v", ok, err)
	}
}

func TestInactiveSweepTargetRetainsSelectedClaimOwnership(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx := context.Background()
	var targetID int64
	if err := store.Pool().QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,wallet,wallet_token_ata,vault_token_ata,token_mint,threshold,max_amount_per_period,desired_active,chain_status,last_seen_slot,last_seen_signature) VALUES($1,$2,123,$3,$4,$5,$2,$6,$7,$8,1,1000,false,'closed',500,'fixture') RETURNING id`, state.Settings, fixtureKey(95).String(), fixtureKey(96).String(), topology.VaultIndex, topology.Vault.String(), fixtureKey(97).String(), topology.ClaimCustody.String(), USDCMint).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	token := state.RouteKey + ":selected"
	t.Cleanup(func() {
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1", token)
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.balance_sweep_targets WHERE id=$1", targetID)
	})
	if _, err := store.Pool().Exec(ctx, "INSERT INTO loyal_yield.balance_sweep_lot_claims(claim_token,target_id,amount_raw) VALUES($1,$2,100)", token, targetID); err != nil {
		t.Fatal(err)
	}
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "sweep-contention", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	op := integrationOperation(state.RouteKey, state.Cycle, lease.Version+1)
	state.Generation++
	state.CurrentOperationID = &op.OperationID
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err == nil || ok {
		t.Fatalf("inactive unresolved claim admitted: %v %v", ok, err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.balance_sweep_lot_claims SET status='released' WHERE claim_token=$1", token); err != nil {
		t.Fatal(err)
	}
	// A released selection still cannot free an inactive target while its
	// immutable signed attempt is unknown. This fixture proves ownership only;
	// it is not used as a successful financial execution receipt.
	signed := signedWireFixture(t, false)
	var attemptID int64
	if err := store.Pool().QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_transaction_attempts(claim_token,target_id,operation_kind,amount_raw,source_pre_balance_raw,destination_pre_balance_raw,signature,signed_transaction_base64,signed_transaction_sha256,recent_blockhash,last_valid_block_height,attempt_state) VALUES($1,$2,'pull',100,1000,0,$3,$4,$5,$6,$7,'unknown') RETURNING id`, token, targetID, signed.TransactionSignature, base64.StdEncoding.EncodeToString(signed.Wire), signed.WireSHA256, signed.RecentBlockhash, signed.LastValidBlockHeight).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Pool().Exec(ctx, "DELETE FROM loyal_yield.balance_sweep_transaction_attempts WHERE id=$1", attemptID); err != nil {
			t.Error(err)
		}
	})
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err == nil || ok {
		t.Fatalf("inactive durable unknown admitted: %v %v", ok, err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state='expired' WHERE id=$1", attemptID); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err != nil || !ok {
		t.Fatalf("drained inactive target rejected: %v %v", ok, err)
	}
}
