package referral

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type harvestRPCFixture struct {
	height       uint64
	finalSlot    uint64
	state        chain.SignatureState
	cursorHeight uint64
	hashValid    bool
}

func (f harvestRPCFixture) SendWire(context.Context, []byte, bool) error {
	panic("recovering a harvest must never resend it")
}
func (f harvestRPCFixture) FinalizedBlockHeight(context.Context) (uint64, uint64, error) {
	return f.height, f.finalSlot, nil
}
func (f harvestRPCFixture) SignatureState(context.Context, string) (chain.SignatureState, error) {
	return f.state, nil
}
func (f harvestRPCFixture) FinalizedBlockHeightAt(context.Context, uint64) (uint64, error) {
	return f.cursorHeight, nil
}
func (f harvestRPCFixture) BlockhashValid(context.Context, solana.Hash, rpc.CommitmentType, uint64) (bool, uint64, error) {
	return f.hashValid, f.finalSlot, nil
}

func signedHarvestFixture(t *testing.T, slot uint64) (*solana.Wallet, harvestAttempt) {
	t.Helper()
	wallet := solana.NewWallet()
	addrs, err := harvestAccounts()
	if err != nil {
		t.Fatal(err)
	}
	quote := HarvestQuote{payer: wallet.PublicKey(), addresses: addrs, missing: [3]bool{true, true, true}}
	w, err := quote.wire(solana.Hash{2}, 10, slot)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := wallet.PrivateKey.Sign(w.Message)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := w.Signed(sig)
	if err != nil {
		t.Fatal(err)
	}
	a := harvestAttempt{Wire: wire, Signature: sig, Blockhash: w.Blockhash, LastValidBlockHeight: w.LastValidBlockHeight, QuoteSlot: slot}
	return wallet, a
}

func TestHarvestPersistenceAndReconciliation(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := lockHarvestDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	wallet, a := signedHarvestFixture(t, 50)
	if err = saveHarvest(first, a); err != nil {
		t.Fatal(err)
	}
	if d, err := lockHarvestDir(path); !errors.Is(err, syscall.EWOULDBLOCK) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("second collector acquired directory: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(path, harvestAttemptFile))
	if err != nil {
		t.Fatal(err)
	}
	var recovered harvestAttempt
	if err = json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	if err = recovered.validate(wallet.PublicKey()); err != nil {
		t.Fatal(err)
	}
	wrong := recovered
	wrong.Signature[0] ^= 1
	if err = wrong.validate(wallet.PublicKey()); err == nil {
		t.Fatal("persisted signature substitution accepted")
	}
	if recovered.Signature != a.Signature || string(recovered.Wire) != string(a.Wire) || recovered.LastValidBlockHeight != 10 {
		t.Fatal("persisted attempt changed")
	}
	if err = clearHarvest(nil); err != nil {
		t.Fatal(err)
	} // Preview cannot remove pending work.
	if _, err = os.Stat(filepath.Join(path, harvestAttemptFile)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expired := harvestRPCFixture{height: 11, finalSlot: 60, state: chain.SignatureState{ContextSlot: 60}, cursorHeight: 11}
	landed := expired
	landed.state = chain.SignatureState{Found: true, Slot: 57, Commitment: chain.Finalized, ContextSlot: 60}
	failed := landed
	failed.state.Err = "program failure"
	forkFailure := failed
	forkFailure.state.Commitment = chain.Confirmed
	behind := expired
	behind.state.ContextSlot--
	young := expired
	young.cursorHeight = 10
	for _, tc := range []struct {
		name  string
		chain harvestRPCFixture
		db    harvestCheckpoint
		want  string
	}{
		{"possibly sent, lagging RPC", behind, harvestCheckpoint{Verified: 60}, "pending_finality"},
		{"expiry, ledger behind", young, harvestCheckpoint{Verified: 57}, "awaiting_fee_observer"},
		{"expiry, ledger held", expired, harvestCheckpoint{Verified: 60, Held: true}, "held"},
		{"finalized expiry and independent ledger absence", expired, harvestCheckpoint{Verified: 60}, "expired"},
		{"seen before observer catches up", landed, harvestCheckpoint{Verified: 55}, "awaiting_fee_observer"},
		{"observer missed finalized signature", landed, harvestCheckpoint{Verified: 60}, "held"},
		{"confirmed fork error", forkFailure, harvestCheckpoint{Verified: 55}, "pending_finality"},
		{"finalized failure latches", failed, harvestCheckpoint{Verified: 60}, "held"},
		{"exact observed receipt", expired, harvestCheckpoint{Verified: 60, ReceiptSlot: 57}, "reconciled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := harvestDisposition(ctx, tc.chain, a, tc.db)
			if err != nil || res.Status != tc.want {
				t.Fatalf("got %+v, %v", res, err)
			}
		})
	}
	expired.hashValid = true
	if _, err := harvestDisposition(ctx, expired, a, harvestCheckpoint{Verified: 60}); err == nil {
		t.Fatal("released attempt with live blockhash")
	}
	if err = clearHarvest(first); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(path, harvestAttemptFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal was not cleared: %v", err)
	}
}

func TestHarvestRequiresVerifiedFriendFunding(t *testing.T) {
	dsn := os.Getenv("REFERRAL_HARVEST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a fresh private referral_harvest_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "referral_harvest_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/") {
		t.Fatal("requires a disposable local socket database")
	}
	ctx := context.Background()
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA loyal_yield")
	for _, name := range []string{"0129_referrals", "0146_referral_wallet_identities", "0147_referral_earn_max_capital", "0148_referral_earn_max_fee_lots"} {
		b, err := os.ReadFile("../../../../migrations/yield/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(b))
	}
	s := NewStore(db, nil, nil)
	check := func(want, raw string) {
		t.Helper()
		r, err := s.harvestInventory(ctx)
		if err != nil || r.Status != want || r.EligibleLPRaw != raw {
			t.Fatalf("inventory %+v, %v; want %s %s", r, err, want, raw)
		}
	}
	check("not_launched", "")
	exec("INSERT INTO loyal_yield.referral_earn_max_program(launch_slot,launched_at) VALUES(1,now())")
	check("opening_checkpoint_required", "")
	exec(`INSERT INTO loyal_yield.referral_earn_max_fee_cursor(verified_slot,checked_slot,state) VALUES(20,20,'{"Minted":100,"Accrued":100,"OpeningReserve":100,"Head":"test"}')`)
	check("no_eligible_fees", "0")
	const inviter = "00000000-0000-0000-0000-000000000001"
	const alice = "00000000-0000-0000-0000-000000000002"
	const bob = "00000000-0000-0000-0000-000000000003"
	for _, friend := range []string{alice, bob} {
		exec("INSERT INTO loyal_yield.referral_bindings(referee_app_user_id,referee_wallet,referrer_app_user_id,source,referee_signed_up_at,bound_at) VALUES($1,$2,$3,'link',now(),now())", friend, "wallet"+friend, inviter)
	}
	exec(`INSERT INTO loyal_yield.referral_earn_max_fee_assessments(signature,slot,transaction_index,effective_supply_raw,fee_lp_raw,block_time,block_signatures) VALUES('a',12,0,4,1,now(),ARRAY['a'])`)
	exec(`UPDATE loyal_yield.referral_earn_max_fee_cursor SET state=jsonb_set(state,'{Accrued}','101')`)
	check("no_eligible_fees", "0")
	for _, friend := range []string{alice, bob} {
		exec(`INSERT INTO loyal_yield.referral_earn_max_fee_weights(assessment_signature,referee_app_user_id,referrer_app_user_id,shares_raw) VALUES('a',$1,$2,2)`, friend, inviter)
	}
	// Two friends' half-share contributions cannot be pooled.
	check("no_eligible_fees", "0")
	exec("INSERT INTO loyal_yield.referral_earn_max_fee_remainders(referee_app_user_id,remainder) VALUES($1,'1/2')", alice)
	check("eligible", "1")
	// A previous attempt was finalized, and a different assessment now
	// qualifies. The normal entry point must handle the journal before
	// considering that new work. A nil RPC proves it never builds or sends.
	t.Run("recover observer-certified harvest before new work", func(t *testing.T) {
		key, a := signedHarvestFixture(t, 7)
		treasury, err := feeTreasury()
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO loyal_yield.referral_earn_max_fee_harvests(signature,slot,transaction_index,treasury_lp_account,amount_lp_raw) VALUES($1,10,0,$2,100)", a.Signature.String(), treasury.String())
		exec(`UPDATE loyal_yield.referral_earn_max_fee_cursor SET state=state || '{"Accrued":1,"OpeningReserve":0}'::jsonb`)
		check("eligible", "1")
		path := t.TempDir()
		if err = os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
		dir, err := lockHarvestDir(path)
		if err != nil {
			t.Fatal(err)
		}
		err = saveHarvest(dir, a)
		dir.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, send := range []bool{false, true} {
			var signer solana.PrivateKey
			if send {
				signer = key.PrivateKey
			}
			out, err := s.RunHarvest(ctx, nil, key.PublicKey(), signer, path, send)
			if err != nil || out.Status != "reconciled" || out.Signature != a.Signature.String() {
				t.Fatalf("send=%v: %+v, %v", send, out, err)
			}
			_, err = os.Stat(filepath.Join(path, harvestAttemptFile))
			if (send && !errors.Is(err, os.ErrNotExist)) || (!send && err != nil) {
				t.Fatalf("send=%v: journal stat: %v", send, err)
			}
		}
		check("eligible", "1")
	})

	exec("UPDATE loyal_yield.referral_earn_max_fee_weights SET shares_raw=NULL,hold_reason='unresolved' WHERE referee_app_user_id=$1", alice)
	check("no_eligible_fees", "0")
	exec("UPDATE loyal_yield.referral_earn_max_fee_weights SET shares_raw=2,hold_reason=NULL WHERE referee_app_user_id=$1", alice)
	exec("INSERT INTO loyal_yield.referral_wallet_identities(wallet,privy_subject,app_user_id) VALUES($1,'same',$2),($3,'same',$4)", "wallet"+alice, alice, "wallet"+bob, bob)
	check("no_eligible_fees", "0")
	exec("UPDATE loyal_yield.referral_earn_max_fee_cursor SET hold_reason='history gap',checked_slot=21")
	check("held", "")
}

func TestHarvestBetweenObserverAndQuote(t *testing.T) {
	baseline := feeState{Minted: 90, Accrued: 10, Head: "old"}
	sameAmount := baseline
	got, err := harvestInventoryAvailable(baseline, sameAmount, []feeEvent{{kind: "harvest", minted: 10}, {kind: "strategy", fee: 10}})
	if err != nil || got {
		t.Fatalf("reused assessments harvested by another caller: %t %v", got, err)
	}
	added := feeEvent{kind: "strategy", fee: 3, supplyBefore: 100, supplyAfter: 103}
	now := feeState{Minted: 90, Accrued: 13}
	got, err = harvestInventoryAvailable(baseline, now, []feeEvent{added})
	if err != nil || !got {
		t.Fatalf("existing fees no longer eligible after new accrual: %t %v", got, err)
	}
	now.Accrued++
	if _, err = harvestInventoryAvailable(baseline, now, []feeEvent{added}); err == nil {
		t.Fatal("accepted unexplained accrual")
	}
}
