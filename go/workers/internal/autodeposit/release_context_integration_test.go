package autodeposit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
)

func releaseContextStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("AUTODEPOSIT_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("AUTODEPOSIT_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/workers_v2_autodeposit" || u.User == nil || u.User.Username() != "workers_v2" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("release fault injection requires the disposable loopback workers_v2 Autodeposit database")
	}
	if _, supplied := u.User.Password(); supplied {
		t.Fatal("disposable Autodeposit tests exclude password credentials")
	}
	return integrationStore(t)
}

func selectedReleaseClaim(t *testing.T, store *Store, suffix string, firstCap ...int64) (integrationTarget, string, int64) {
	t.Helper()
	ctx := context.Background()
	var previousOffset int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(last_event_id), 0) FROM loyal_yield.projection_offsets WHERE consumer_name = $1`, ConsumerName).Scan(&previousOffset); err != nil {
		t.Fatal(err)
	}
	// Release tests run serially and restore their fixture cursor so they
	// cannot hide another test's lower, pinned event IDs on a repeated run.
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `UPDATE loyal_yield.projection_offsets SET last_event_id = $2 WHERE consumer_name = $1`, ConsumerName, previousOffset)
	})
	seeded := seedIntegrationTarget(t, store, suffix)
	seedProjectedSurplus(t, store, seeded, previousOffset+1, 9_000_000)
	var slot int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id = $1`, seeded.TargetID).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	claim := "release-" + suffix
	cap := int64(100_000_000)
	if len(firstCap) != 0 {
		cap = firstCap[0]
	}
	outcome, err := store.ClaimEligibleLotsOnce(ctx, seeded.TargetID, claim, &slot, 9_000_000, 4_000_000, &cap, &cap)
	if err != nil || outcome.Status != ClaimSelected {
		t.Fatalf("claim: %+v %v", outcome, err)
	}
	if acquired, err := store.AcquireClaimLease(ctx, claim, seeded.TargetID, "lease-current"); err != nil || !acquired {
		t.Fatalf("lease: %v %v", acquired, err)
	}
	return seeded, claim, slot
}

func assertReleaseCustodyUnchanged(t *testing.T, store *Store, claim string) {
	t.Helper()
	var status string
	var remaining int64
	if err := store.pool.QueryRow(context.Background(), `
SELECT claim.status::text, COALESCE(sum(lot.remaining_amount_raw), 0)::bigint
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_lot_claim_items AS item ON item.claim_token = claim.claim_token
JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.id = item.lot_id
WHERE claim.claim_token = $1 GROUP BY claim.status`, claim).Scan(&status, &remaining); err != nil {
		t.Fatal(err)
	}
	if status != "selected" || remaining != 0 {
		t.Fatalf("rejected release changed custody: status=%s remaining=%d", status, remaining)
	}
}

func TestReleaseRequiresLiveLeaseAndUnspentPull(t *testing.T) {
	for _, kind := range []string{"displaced", "expired", "holding-pull"} {
		t.Run(kind, func(t *testing.T) {
			store := releaseContextStore(t)
			ctx := context.Background()
			seeded, claim, slot := selectedReleaseClaim(t, store, "release-"+kind)
			lease := "lease-current"
			want := ErrOwnershipLost
			switch kind {
			case "displaced":
				lease = "lease-displaced"
			case "expired":
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET autodeposit_executor_lease_expires_at = now() - interval '1 second' WHERE claim_token = $1`, claim); err != nil {
					t.Fatal(err)
				}
			case "holding-pull":
				// This test owns SQL custody, not ABI proof: an opaque durable
				// packet is enough to show that prepared intent blocks release.
				wire := []byte("release-test-durable-packet")
				hash := sha256.Sum256(wire)
				_, err := store.PersistPreparedAttempt(ctx, PreparedAttempt{
					ClaimToken: claim, TargetID: seeded.TargetID, ScheduledSlotID: slot,
					OperationKind: OperationPull, AmountRaw: 5_000_000, SourcePreBalanceRaw: 9_000_000,
					ProtectionFloorRaw: ptrInt64(4_000_000),
					Signature:          claim + "-signature", SignedTransactionBase64: base64.StdEncoding.EncodeToString(wire),
					SignedTransactionSHA256: hex.EncodeToString(hash[:]), RecentBlockhash: "release-test-hash", LastValidBlockHeight: 500,
				}, lease)
				if err != nil {
					t.Fatal(err)
				}
				want = ErrClaimCustodyHeld
			}
			if _, err := store.ReleaseClaimOnce(ctx, claim, lease); !errors.Is(err, want) {
				t.Fatalf("release error %v, want %v", err, want)
			}
			assertReleaseCustodyUnchanged(t, store, claim)
			if kind == "holding-pull" {
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET updated_at = now() - interval '1 hour', autodeposit_executor_lease_expires_at = now() - interval '1 second' WHERE claim_token = $1`, claim); err != nil {
					t.Fatal(err)
				}
				if count, err := store.ReleaseStaleSelectedClaims(ctx, 60, 100); err != nil || count != 0 {
					t.Fatalf("stale release discarded signed intent: %d %v", count, err)
				}
				assertReleaseCustodyUnchanged(t, store, claim)
			}
		})
	}
}

func TestStaleReleaseKeepsLiveExecutorAndRecoversCrashBeforePrepare(t *testing.T) {
	store := releaseContextStore(t)
	ctx := context.Background()
	_, claim, _ := selectedReleaseClaim(t, store, "crash-before-prepare")
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET updated_at = now() - interval '1 hour' WHERE claim_token = $1`, claim); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReleaseStaleSelectedClaims(ctx, 60, 100); err != nil || count != 0 {
		t.Fatalf("live executor released: %d %v", count, err)
	}
	assertReleaseCustodyUnchanged(t, store, claim)
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET autodeposit_executor_lease_expires_at = now() - interval '1 second' WHERE claim_token = $1`, claim); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReleaseStaleSelectedClaims(ctx, 60, 100); err != nil || count != 1 {
		t.Fatalf("unspent expired executor was not recovered: %d %v", count, err)
	}
}

func TestSelectedClaimBlocksAnotherClaimTokenOnSameTarget(t *testing.T) {
	store := releaseContextStore(t)
	seeded, firstClaim, _ := selectedReleaseClaim(t, store, "exclusive-target", 1_000_000)
	cap := int64(100_000_000)
	outcome, err := store.ClaimEligibleLotsOnce(context.Background(), seeded.TargetID, "second-independent-claim", nil, 9_000_000, 4_000_000, &cap, &cap)
	if err != nil || outcome.Status != ClaimNoopStatus() || outcome.Reason != "target_has_selected_claim" {
		t.Fatalf("independent claim did not respect selected custody: %+v %v", outcome, err)
	}
	var amount, remaining int64
	if err := store.pool.QueryRow(context.Background(), `
SELECT claim.amount_raw, lot.remaining_amount_raw
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_lot_claim_items AS item ON item.claim_token = claim.claim_token
JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.id = item.lot_id
WHERE claim.claim_token = $1`, firstClaim).Scan(&amount, &remaining); err != nil {
		t.Fatal(err)
	}
	if amount != 1_000_000 || remaining != 4_000_000 {
		t.Fatalf("second executor consumed the partial claim's remaining lots: claim=%d remaining=%d", amount, remaining)
	}
}

func TestClaimRechecksFloorAndCurrentPeriodCapUnderTargetLock(t *testing.T) {
	store := releaseContextStore(t)
	ctx := context.Background()
	seeded, firstClaim, _ := selectedReleaseClaim(t, store, "protection-settings")
	if _, err := store.ReleaseClaimOnce(ctx, firstClaim, "lease-current"); err != nil {
		t.Fatal(err)
	}
	elapseReleaseDelay(t, store, seeded.TargetID)
	callerCap := int64(100_000_000)
	outcome, err := store.ClaimEligibleLotsOnce(ctx, seeded.TargetID, "stale-floor-claim", nil, 9_000_000, 0, &callerCap, &callerCap)
	if err != nil || outcome.Status != ClaimNoopStatus() || outcome.Reason != "wallet_balance_floor_changed" {
		t.Fatalf("stale lower floor authorized a claim: %+v %v", outcome, err)
	}
	var remaining, claims int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(sum(remaining_amount_raw), 0)::bigint FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1`, seeded.TargetID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'stale-floor-claim'`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if remaining != 5_000_000 || claims != 0 {
		t.Fatalf("stale floor changed lots/claims: remaining=%d claims=%d", remaining, claims)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET max_amount_per_period = 1000000 WHERE id = $1`, seeded.TargetID); err != nil {
		t.Fatal(err)
	}
	outcome, err = store.ClaimEligibleLotsOnce(ctx, seeded.TargetID, "current-period-claim", nil, 9_000_000, 4_000_000, &callerCap, &callerCap)
	if err != nil || outcome.Status != ClaimSelected || outcome.AmountRaw != 1_000_000 {
		t.Fatalf("stale high caller cap overrode current target cap: %+v %v", outcome, err)
	}
}

func TestTargetContextRefusesUnknownFloorAndSelectsMatchingMint(t *testing.T) {
	store := releaseContextStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "context-mint")
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw = NULL WHERE id = $1`, seeded.TargetID); err != nil {
		t.Fatal(err)
	}
	if target, err := store.LoadTargetExecutionContext(ctx, seeded.TargetID); err == nil || target != nil {
		t.Fatalf("unknown floor became executable context: %+v %v", target, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw = 0 WHERE id = $1`, seeded.TargetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM loyal_yield.user_yield_positions WHERE settings = 'itest-settings-context-mint'`)
	})
	for _, position := range []struct{ reserve, mint string }{{"context-usdc", USDCMint}, {"context-other-newer", "other-mint"}} {
		if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.user_yield_positions (
 wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
 policy_id, policy_account, policy_seed, initial_reserve, initial_market, initial_liquidity_mint,
 deposit_mint, principal_amount_raw, current_reserve, current_market, current_liquidity_mint,
 current_amount_raw, current_observed_slot, current_observed_at,
 first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at)
SELECT wallet, vault_pubkey, settings, vault_index, vault_pubkey,
 1, policy_account, policy_seed, $2, 'context-market', $3, $3, 1, $2, 'context-market', $3,
 1, 1, now(), $2, $2, 1, 'active', now(), now()
FROM loyal_yield.balance_sweep_targets WHERE id = $1`, seeded.TargetID, position.reserve, position.mint); err != nil {
			t.Fatal(err)
		}
	}
	target, err := store.LoadTargetExecutionContext(ctx, seeded.TargetID)
	if err != nil || target == nil || target.WalletBalanceFloorRaw != 0 || target.CurrentReserve == nil || *target.CurrentReserve != "context-usdc" {
		t.Fatalf("zero floor or matching mint context lost: %+v %v", target, err)
	}
}
