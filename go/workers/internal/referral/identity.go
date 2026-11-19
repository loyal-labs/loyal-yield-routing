package referral

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Identity contains only Solana wallet links verified by this app's Privy
// service. A wallet-only account has no Subject.
type Identity struct {
	Subject string
	Wallets []string
}

type LookupIdentity func(context.Context, string) (Identity, error)

var ErrIdentityConflict = errors.New("referral: conflicting verified wallet identities")

// Persist positive evidence only. A lookup miss must not erase a relationship
// that was known when processing an earlier referral.
func (s *Store) observe(ctx context.Context, wallet string) error {
	if s.lookup == nil {
		return nil
	}
	proof, err := s.lookup(ctx, wallet)
	if err != nil || proof.Subject == "" {
		return err
	}
	if !slices.Contains(proof.Wallets, wallet) {
		return errors.New("referral: identity does not contain the requested wallet")
	}
	// App and Yield have independent databases. Remember only the app-user IDs
	// belonging to verified addresses, including later signups under a known wallet.
	var users []string
	err = s.app.QueryRow(ctx, `SELECT ARRAY(SELECT COALESCE(u.id::text, '')
FROM unnest($1::text[]) WITH ORDINALITY w(address, n)
LEFT JOIN app_users u ON u.provider = 'solana' AND u.subject_address = w.address ORDER BY w.n)`, proof.Wallets).Scan(&users)
	if err != nil {
		return err
	}
	tx, err := s.yield.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Reward writers take the shared form before checking the conflict view.
	// A newly discovered alias can affect a binding on a different wallet.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('referral-identity-evidence', 0))"); err != nil {
		return err
	}
	if err := lockIdentities(ctx, tx, proof); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_wallet_identities (wallet, privy_subject, app_user_id)
SELECT wallet, $2, NULLIF(id,'')::uuid FROM unnest($1::text[], $3::text[]) w(wallet,id)
ON CONFLICT DO NOTHING`, proof.Wallets, proof.Subject, users)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Wallet-only and linked-wallet claims contend on every shared address.
// Subject locks cover two observations with disjoint linked-wallet lists.
func lockIdentities(ctx context.Context, tx pgx.Tx, ids ...Identity) error {
	var keys []string
	for _, id := range ids {
		if id.Subject != "" {
			keys = append(keys, "referral-subject:"+id.Subject)
		}
		for _, wallet := range id.Wallets {
			keys = append(keys, "referral-wallet:"+wallet)
		}
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(key) FROM (
 SELECT DISTINCT hashtextextended(value, 0) AS key FROM unnest($1::text[]) value ORDER BY key
) locks`, keys)
	return err
}

type identityReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func knownIdentity(ctx context.Context, db identityReader, wallet string) (Identity, error) {
	var id Identity
	var subjects int
	err := db.QueryRow(ctx, `WITH subjects AS (
  SELECT privy_subject FROM loyal_yield.referral_wallet_identities WHERE wallet = $1
), wallets AS (
  SELECT DISTINCT wallet FROM loyal_yield.referral_wallet_identities WHERE privy_subject IN (SELECT privy_subject FROM subjects)
)
SELECT COALESCE(min(privy_subject), ''), count(DISTINCT privy_subject), ARRAY(SELECT wallet FROM wallets ORDER BY wallet)
FROM loyal_yield.referral_wallet_identities WHERE wallet IN (SELECT wallet FROM wallets)`, wallet).
		Scan(&id.Subject, &subjects, &id.Wallets)
	if err != nil {
		return Identity{}, err
	}
	if subjects > 1 {
		return Identity{}, ErrIdentityConflict
	}
	if len(id.Wallets) == 0 {
		id.Wallets = []string{wallet}
	}
	return id, nil
}

func (s *Store) members(ctx context.Context, id Identity) (time.Time, []string, error) {
	var at time.Time
	var users []string
	err := s.app.QueryRow(ctx, `SELECT min(created_at), array_agg(id::text) FROM app_users
WHERE provider = 'solana' AND subject_address = ANY($1::text[]) HAVING count(*) > 0`, id.Wallets).Scan(&at, &users)
	return at, users, err
}
