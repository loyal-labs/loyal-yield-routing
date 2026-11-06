package earnmax

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

func withdrawalConsentTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VAULT_CONSENT_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("requires explicitly disposable local PostgreSQL")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || config.ConnConfig.Port != 55439 || config.ConnConfig.User != "vault_test" || config.ConnConfig.Database != "postgres" {
		t.Fatal("test requires the authorized disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("vault_consent_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	dbConfig := config.Copy()
	dbConfig.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "CREATE SCHEMA loyal_yield"); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../migrations/yield/0139_earn_max_withdrawal_consent.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWithdrawalConsentStoreConcurrentImmutableVersions(t *testing.T) {
	db := withdrawalConsentTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, settings, now := signedConsent(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	owner := solana.PublicKeyFromBytes(key.Public().(ed25519.PublicKey))
	c.Owner = owner.String()
	c.Destination = owner.String()
	sign := func(c *WithdrawalConsent) {
		message, err := c.CanonicalMessage()
		if err != nil {
			t.Fatal(err)
		}
		c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, message))
	}
	sign(&c)
	if err := c.Validate(owner, settings, now); err != nil {
		t.Fatal(err)
	}
	store := NewWithdrawalConsentStore(db)
	wantID, _ := c.Digest()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.Accept(ctx, c)
			if err != nil {
				errs <- err
			} else if id != wantID {
				errs <- fmt.Errorf("duplicate changed identity")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM loyal_yield.earn_max_withdrawal_consents").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate approvals: %d %v", count, err)
	}
	c2 := c
	c2.Version = "2"
	c2.Nonce = strings.Repeat("c", 64)
	c2.MinClaimUSDC = "980"
	sign(&c2)
	if _, err := store.Accept(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if id, err := store.Accept(ctx, c); err != nil || id != wantID {
		t.Fatalf("older exact duplicate must not revive or fail: %s %v", id, err)
	}
	restarted := NewWithdrawalConsentStore(db)
	latest, err := restarted.Latest(ctx, c.Authority, c.WithdrawalGeneration)
	if err != nil || latest == nil || latest.Version != "2" || latest.MinClaimUSDC != "980" {
		t.Fatalf("restart lost latest approval: %#v %v", latest, err)
	}
	conflict := c2
	conflict.MinClaimUSDC = "970"
	conflict.Nonce = strings.Repeat("d", 64)
	sign(&conflict)
	if _, err := store.Accept(ctx, conflict); err == nil {
		t.Fatal("same version replaced immutable approval")
	}
	reuse := c
	reuse.RequestSignature = solana.Signature{2}.String()
	sign(&reuse)
	if _, err := store.Accept(ctx, reuse); err == nil {
		t.Fatal("nonce reused across generations")
	}
	if err := db.QueryRow(ctx, "SELECT count(*) FROM loyal_yield.earn_max_withdrawal_consents").Scan(&count); err != nil || count != 2 {
		t.Fatalf("conflict mutated rows: %d %v", count, err)
	}
}
