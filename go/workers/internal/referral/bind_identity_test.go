package referral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBindVerifiedWallets(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("REFERRAL_IDENTITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a fresh disposable referral_identity_*_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host, name := config.ConnConfig.Host, config.ConnConfig.Database
	if !(host == "localhost" || host == "127.0.0.1" || strings.HasPrefix(host, "/")) ||
		!strings.HasPrefix(name, "referral_identity_") || !strings.HasSuffix(name, "_test") {
		t.Fatal("requires a disposable local referral_identity_*_test database")
	}
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(ctx, `CREATE SCHEMA loyal_yield;
CREATE TABLE app_users (id uuid PRIMARY KEY, provider text NOT NULL, subject_address text UNIQUE NOT NULL, created_at timestamptz NOT NULL)`); err != nil {
		t.Fatalf("database must be fresh: %v", err)
	}
	defer func() {
		if _, err := db.Exec(ctx, "DROP SCHEMA loyal_yield CASCADE; DROP TABLE app_users"); err != nil {
			t.Error(err)
		}
	}()
	for _, name := range []string{"0129_referrals", "0146_referral_wallet_identities"} {
		migration, err := os.ReadFile("../../../../migrations/yield/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]Identity{}
	store := NewStore(db, db, func(_ context.Context, wallet string) (Identity, error) { return links[wallet], nil })
	add := func(wallet string, age time.Duration) (User, Code) {
		t.Helper()
		var count int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM app_users").Scan(&count); err != nil {
			t.Fatal(err)
		}
		user := User{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", count+1), Wallet: wallet, SignedUpAt: time.Now().Add(-age)}
		if _, err := db.Exec(ctx, "INSERT INTO app_users VALUES ($1,'solana',$2,$3)", user.ID, wallet, user.SignedUpAt); err != nil {
			t.Fatal(err)
		}
		code, err := store.EnsureCode(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return user, code
	}
	link := func(subject string, users ...User) {
		t.Helper()
		id := Identity{Subject: subject}
		for _, user := range users {
			id.Wallets = append(id.Wallets, user.Wallet)
		}
		for _, wallet := range id.Wallets {
			links[wallet] = id
		}
	}
	check := func(user User, code Code, expected Result) {
		t.Helper()
		got, err := store.Bind(ctx, user, code, Typed)
		if err != nil || got != expected {
			t.Fatalf("claim %s expected %s, got %s, err=%v", user.Wallet, expected, got, err)
		}
	}

	inviter1, code1 := add("inviter1", 40*24*time.Hour)
	_, code2 := add("inviter2", 30*24*time.Hour)
	a, selfCode := add("friend-a", 6*time.Hour)
	b, _ := add("friend-b", 2*time.Hour)
	link("did:privy:friend", a, b)
	check(b, selfCode, OwnCode)

	type attempt struct {
		user   User
		code   Code
		result Result
		err    error
	}
	start, results := make(chan struct{}), make(chan attempt, 2)
	for _, x := range []attempt{{user: a, code: code1}, {user: b, code: code2}} {
		go func() {
			<-start
			x.result, x.err = store.Bind(ctx, x.user, x.code, Link)
			results <- x
		}()
	}
	close(start)
	winner, other := <-results, <-results
	if winner.result != Bound {
		winner, other = other, winner
	}
	if winner.err != nil || other.err != nil || winner.result != Bound || other.result != AlreadyBound {
		t.Fatalf("competing mixed-wallet claims: %+v %+v", winner, other)
	}
	check(other.user, winner.code, Bound)
	var rows int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM loyal_yield.referral_bindings").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("expected single immutable binding, rows=%d err=%v", rows, err)
	}
	can, _, err := store.ClaimWindowFor(ctx, other.user)
	if err != nil || can {
		t.Fatalf("linked wallet can claim again: %v, %v", can, err)
	}
	// A later lookup miss preserves previously verified evidence and retries.
	delete(links, other.user.Wallet)
	check(other.user, other.code, AlreadyBound)

	old, _ := add("old-wallet", 9*24*time.Hour)
	newWallet, _ := add("new-wallet", 2*time.Hour)
	link("did:privy:old", old, newWallet)
	check(newWallet, code1, WindowClosed)
	native, _ := add("native", 5*time.Hour)
	if got, err := store.Bind(ctx, native, code1, InstallReferrer); err != nil || got != Bound {
		t.Fatalf("native wallet installation claim = %s, %v", got, err)
	}
	check(native, code1, Bound)
	var firstSource string
	if err := db.QueryRow(ctx, "SELECT source FROM loyal_yield.referral_bindings WHERE referee_app_user_id=$1", native.ID).Scan(&firstSource); err != nil || firstSource != string(InstallReferrer) {
		t.Fatalf("install source after retry = %q, %v", firstSource, err)
	}
	second, _ := add("another-native", 4*time.Hour)
	check(second, code2, Bound)
	// Both bindings were valid independently. Keep both immutable and require
	// resolution when their later verified linkage exposes a conflict.
	link("did:privy:later", native, second)
	if _, err := store.Bind(ctx, native, code1, Link); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("expected unresolved historical overlap, got %v", err)
	}
	var holds int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.referral_attribution_conflicts
WHERE referee_app_user_id::text = ANY($1::text[]) AND reason = 'multiple_bindings'`, []string{native.ID, second.ID}).Scan(&holds); err != nil || holds != 2 {
		t.Fatalf("expected both later-conflicting referrals to be held: rows=%d err=%v", holds, err)
	}
	var original int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.referral_bindings b
JOIN loyal_yield.referral_codes c ON b.referrer_app_user_id = c.app_user_id
WHERE (b.referee_wallet = $1 AND c.code = $2) OR (b.referee_wallet = $3 AND c.code = $4)`,
		native.Wallet, string(code1), second.Wallet, string(code2)).Scan(&original); err != nil || original != 2 {
		t.Fatalf("original bindings no longer intact: rows=%d err=%v", original, err)
	}
	lateSelf, _ := add("native-later-linked-to-inviter", time.Hour)
	check(lateSelf, code1, Bound)
	link("did:privy:inviter", lateSelf, inviter1)
	check(lateSelf, code1, OwnCode)
	var held bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.referral_attribution_conflicts
WHERE referee_app_user_id=$1 AND reason='self_referral')`, lateSelf.ID).Scan(&held); err != nil || !held {
		t.Fatalf("later verified self referral must be held: held=%v err=%v", held, err)
	}

	link("did:privy:changed", other.user)
	if _, err := store.Bind(ctx, other.user, other.code, Link); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("expected wallet reassignment conflict; got %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.referral_attribution_conflicts
WHERE referee_app_user_id=$1 AND reason='wallet_reassigned')`, winner.user.ID).Scan(&held); err != nil || !held {
		t.Fatalf("reassigned linked wallet must hold binding: held=%v err=%v", held, err)
	}

}
