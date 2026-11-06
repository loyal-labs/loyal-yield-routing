package earnmax

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func withdrawalAttemptFixture(t *testing.T) (WithdrawalConsent, validatedWithdrawalAttempt, solana.PrivateKey) {
	t.Helper()
	wallet := solana.NewWallet()
	settings := solana.NewWallet().PublicKey()
	authority, err := AuthorityOf(settings)
	if err != nil {
		t.Fatal(err)
	}
	c := WithdrawalConsent{WithdrawalGeneration: WithdrawalGeneration{RequestSignature: solana.Signature{1}.String(), RequestInstruction: 2}, Version: "1", PolicySeed: "0", Owner: wallet.PublicKey().String(), Cluster: Cluster, Program: voltr.ProgramID.String(), Vault: Vault.String(), Settings: settings.String(), Authority: authority.Key().String(), Destination: wallet.PublicKey().String(), LPRaw: "100", ReferenceUSDC: "1000", ReferenceSlot: "123", FeeBasisHash: strings.Repeat("a", 64), MinClaimUSDC: "990", MaxUserFeeLamports: "5000", ExpiresAt: fmt.Sprint(time.Now().Add(time.Hour).Unix()), Nonce: strings.Repeat("b", 64)}
	message, _ := c.CanonicalMessage()
	c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(wallet.PrivateKey), message))
	a := withdrawalAttemptSignedWire(t, wallet.PrivateKey, 1)
	return c, a, wallet.PrivateKey
}

func withdrawalAttemptSignedWire(t *testing.T, key solana.PrivateKey, hash byte) validatedWithdrawalAttempt {
	t.Helper()
	ix := solana.NewInstruction(solana.SystemProgramID, solana.AccountMetaSlice{solana.Meta(key.PublicKey()).SIGNER().WRITE()}, []byte{0})
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, solana.Hash{hash}, solana.TransactionPayer(key.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Sign(func(pub solana.PublicKey) *solana.PrivateKey {
		if pub == key.PublicKey() {
			return &key
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return validatedWithdrawalAttempt{wire, tx.Signatures[0].String(), tx.Message.RecentBlockhash.String(), 100, 5000}
}

func TestWithdrawalAttemptWireMustMatchSignedOwnerExactMessageAndFee(t *testing.T) {
	c, a, _ := withdrawalAttemptFixture(t)
	if _, err := validateWithdrawalAttemptWire(c, a.wire, a.signature, a.blockhash, a.height, a.fee); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*validatedWithdrawalAttempt){"wire": func(a *validatedWithdrawalAttempt) { a.wire = append(append([]byte(nil), a.wire...), 0) }, "fee": func(a *validatedWithdrawalAttempt) { a.fee = 0 }, "signature": func(a *validatedWithdrawalAttempt) { a.signature = solana.Signature{2}.String() }, "blockhash": func(a *validatedWithdrawalAttempt) { a.blockhash = solana.Hash{2}.String() }} {
		t.Run(name, func(t *testing.T) {
			changed := a
			change(&changed)
			if _, err := validateWithdrawalAttemptWire(c, changed.wire, changed.signature, changed.blockhash, changed.height, changed.fee); err == nil {
				t.Fatal("client-modified signed attempt accepted")
			}
		})
	}
	if err := NewWithdrawalAttemptStore(nil).qualifyWithdrawalClaim(context.Background(), c, &a); err == nil {
		t.Fatal("arbitrary owner-signed wire treated as protected claim")
	}
}

func TestWithdrawalAttemptDatabaseFeeReservationSurvivesWideningAndRetries(t *testing.T) {
	dsn := os.Getenv("EARNMAX_WITHDRAWAL_TEST_DSN")
	if dsn == "" {
		t.Skip("explicit disposable withdrawal attempt database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "vault_cost_test") {
		t.Fatal("attempt database must be explicitly disposable localhost vault_cost_test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE loyal_yield.backyard_withdrawal_cost_reservations,loyal_yield.backyard_withdrawal_cost_operations,loyal_yield.earn_max_withdrawal_claim_attempts,loyal_yield.earn_max_withdrawal_consents,loyal_yield.backyard_confirmed_nav_publications,loyal_yield.multiply_operations,loyal_yield.vault_release_controls`); err != nil {
		t.Fatal(err)
	}
	c, a, key := withdrawalAttemptFixture(t)
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.vault_release_controls(vault,action,state,version,reason,operator) VALUES($1,'claim_attempt','open',1,'test','test')`, c.Vault); err != nil {
		t.Fatal(err)
	}
	store := NewWithdrawalAttemptStore(pool)
	insert := func(c WithdrawalConsent) {
		id, _ := c.Digest()
		payload, _ := json.Marshal(c)
		_, err := pool.Exec(ctx, `INSERT INTO loyal_yield.earn_max_withdrawal_consents(consent_id,authority,vault,request_signature,request_instruction,version,owner,nonce,expires_at,canonical_message,owner_signature,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'message',$10,$11)`, id, c.Authority, c.Vault, c.RequestSignature, c.RequestInstruction, c.Version, c.Owner, c.Nonce, ConsentExpiry(c), make([]byte, 64), payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(c)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, candidate := range []validatedWithdrawalAttempt{a, withdrawalAttemptSignedWire(t, key, 2)} {
		wg.Add(1)
		go func(candidate validatedWithdrawalAttempt) {
			defer wg.Done()
			_, err := store.reserveValidated(ctx, c, candidate)
			results <- err
		}(candidate)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("fee budget admitted %d attempts", accepted)
	}
	var wire []byte
	var id string
	if err := pool.QueryRow(ctx, `SELECT attempt_id,signed_wire FROM loyal_yield.earn_max_withdrawal_claim_attempts`).Scan(&id, &wire); err != nil {
		t.Fatal(err)
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	retryID, err := store.Reserve(ctx, c, wire, tx.Signatures[0].String(), tx.Message.RecentBlockhash.String(), 100, 5000)
	if err != nil || retryID != id {
		t.Fatalf("exact persisted wire retry failed: %s %v", retryID, err)
	}
	c.Version = "2"
	c.MaxUserFeeLamports = "15000"
	c.Nonce = strings.Repeat("c", 64)
	message, _ := c.CanonicalMessage()
	c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(key), message))
	insert(c)
	if _, err := store.reserveValidated(ctx, c, withdrawalAttemptSignedWire(t, key, 3)); err == nil || err.Error() != "withdrawal_claim_pending" {
		t.Fatalf("widening allowed another wire while prior was ambiguous: %v", err)
	}
	var retained []byte
	if err := pool.QueryRow(ctx, `SELECT signed_wire FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, id).Scan(&retained); err != nil || !bytes.Equal(retained, wire) {
		t.Fatal("signed wire changed across consent update")
	}
	sum := sha256.Sum256(retained)
	if id != hex.EncodeToString(sum[:]) {
		t.Fatal("attempt ID detached from exact wire")
	}
}

// A claim pays at the published total value, so a stale-high price would pay
// the claimant out of the remaining holders.
func TestClaimWaitsWhilePublishedValueExceedsCertifiedNAV(t *testing.T) {
	now := time.Now()
	nav := &certifiedVaultNAV{totalRaw: 10_000_000_000, certifiedSlot: 100, maxSlot: 132, validUntil: now.Add(30 * time.Second)}
	if err := claimPricePublished(nav, now, 110, 10_000_000_000+15_000_000); err != ErrClaimWaitingNAVPublication {
		t.Fatalf("claim admitted 15 bps above certified NAV: %v", err)
	}
}
