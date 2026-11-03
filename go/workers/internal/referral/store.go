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
	yield *pgxpool.Pool
	app   *pgxpool.Pool
}

func NewStore(yield, app *pgxpool.Pool) *Store {
	return &Store{yield: yield, app: app}
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
		var code string
		err := s.yield.QueryRow(ctx, `SELECT code FROM loyal_yield.referral_codes WHERE app_user_id = $1`, user).Scan(&code)
		if err == nil {
			return Code(code), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if _, err := s.yield.Exec(ctx, `INSERT INTO loyal_yield.referral_codes (app_user_id, code) VALUES ($1, $2) ON CONFLICT DO NOTHING`, user, string(newCode())); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("referral: no code for %s after %d attempts", user, codeAttempts)
}

func (s *Store) CodeExists(ctx context.Context, code Code) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var exists bool
	err := s.yield.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.referral_codes WHERE code = $1)`, string(code)).Scan(&exists)
	return exists, err
}

func (s *Store) Bound(ctx context.Context, user User) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var bound bool
	err := s.yield.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.referral_bindings WHERE referee_app_user_id = $1 OR referee_wallet = $2)`, user.ID, user.Wallet).Scan(&bound)
	return bound, err
}
