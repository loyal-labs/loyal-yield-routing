package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ClaimWindow  = 7 * 24 * time.Hour
	queryTimeout = 10 * time.Second
	codeAttempts = 4
)

type Store struct {
	yield  *pgxpool.Pool
	app    *pgxpool.Pool
	lookup LookupIdentity
}

func NewStore(yield, app *pgxpool.Pool, lookup LookupIdentity) *Store {
	return &Store{yield: yield, app: app, lookup: lookup}
}

type User struct {
	ID         string
	Wallet     string
	SignedUpAt time.Time
}

func (u User) ClaimClosesAt() time.Time { return u.SignedUpAt.Add(ClaimWindow) }

var ErrNoUser = errors.New("referral: the wallet has no app user")

func (s *Store) User(ctx context.Context, wallet string) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	user := User{Wallet: wallet}
	err := s.app.QueryRow(ctx, `SELECT id::text, created_at FROM app_users WHERE provider = 'solana' AND subject_address = $1`, wallet).
		Scan(&user.ID, &user.SignedUpAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoUser
	}
	return user, err
}

func (s *Store) EnsureCode(ctx context.Context, user string) (Code, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	for range codeAttempts {
		code, found, err := s.code(ctx, user)
		if err != nil || found {
			return code, err
		}
		if _, err := s.yield.Exec(ctx, `INSERT INTO loyal_yield.referral_codes (app_user_id, code) VALUES ($1, $2) ON CONFLICT DO NOTHING`, user, string(newCode())); err != nil {
			return "", err
		}
	}
	code, found, err := s.code(ctx, user)
	if err != nil || found {
		return code, err
	}
	return "", fmt.Errorf("referral: no code for %s after %d attempts", user, codeAttempts)
}

func (s *Store) code(ctx context.Context, user string) (Code, bool, error) {
	var code string
	err := s.yield.QueryRow(ctx, `SELECT code FROM loyal_yield.referral_codes WHERE app_user_id = $1`, user).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return Code(code), err == nil, err
}

func (s *Store) CodeExists(ctx context.Context, code Code) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var exists bool
	err := s.yield.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.referral_codes WHERE code = $1)`, string(code)).Scan(&exists)
	return exists, err
}

// ClaimWindowFor uses known aliases when rendering the existing dashboard.
func (s *Store) ClaimWindowFor(ctx context.Context, user User) (bool, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	id, err := knownIdentity(ctx, s.yield, user.Wallet)
	if errors.Is(err, ErrIdentityConflict) {
		return false, user.ClaimClosesAt(), nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	at, _, err := s.members(ctx, id)
	if err != nil {
		return false, time.Time{}, err
	}
	var bound bool
	err = s.yield.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.referral_bindings
WHERE referee_app_user_id = $1 OR referee_wallet = ANY($2::text[]) OR referee_privy_subject = NULLIF($3,''))`,
		user.ID, id.Wallets, id.Subject).Scan(&bound)
	closes := at.Add(ClaimWindow)
	return !bound && !time.Now().After(closes), closes, err
}
