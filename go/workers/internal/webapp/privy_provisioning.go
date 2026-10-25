package webapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
)

// privyGetOrCreateUser commits the four statements in one short transaction
// so a crash cannot leave a user row without its wallet row; a re-run writes
// the same rows either way.
func (h earnRoutes) privyGetOrCreateUser(ctx context.Context, wallet solana.PublicKey, email string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := h.appDB.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin privy user upsert: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	address := wallet.String()
	userID, err := privyFindUser(ctx, tx, address)
	if err != nil {
		return "", err
	}
	if userID == "" {
		userID, err = privyCreateUser(ctx, tx, address, email)
		if err != nil {
			return "", err
		}
	}
	if err := privyTouchUser(ctx, tx, userID, email); err != nil {
		return "", err
	}
	if err := privyUpsertUserWallet(ctx, tx, userID, address); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit privy user upsert: %w", err)
	}
	return userID, nil
}

func privyFindUser(ctx context.Context, tx pgx.Tx, address string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM app_users WHERE provider = 'solana' AND subject_address = $1`,
		address).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read app user by principal: %w", err)
	}
	return id, nil
}

func privyCreateUser(ctx context.Context, tx pgx.Tx, address, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO app_users (provider, subject_address, email, created_at, updated_at)
VALUES ('solana', $1, $2, now(), now())
ON CONFLICT (provider, subject_address) DO NOTHING
RETURNING id::text`,
		address, privyOptionalText(email)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		raced, findErr := privyFindUser(ctx, tx, address)
		if findErr != nil {
			return "", findErr
		}
		if raced == "" {
			return "", errors.New("failed to provision app user")
		}
		return raced, nil
	}
	if err != nil {
		return "", fmt.Errorf("insert app user: %w", err)
	}
	return id, nil
}

func privyTouchUser(ctx context.Context, tx pgx.Tx, userID, email string) error {
	if email == "" {
		_, err := tx.Exec(ctx, `UPDATE app_users SET updated_at = now() WHERE id = $1`, userID)
		if err != nil {
			return fmt.Errorf("touch app user metadata: %w", err)
		}
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE app_users SET updated_at = now(), email = $2 WHERE id = $1`, userID, email)
	if err != nil {
		return fmt.Errorf("update app user metadata: %w", err)
	}
	return nil
}

func privyUpsertUserWallet(ctx context.Context, tx pgx.Tx, userID, address string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO app_user_wallets (user_id, wallet_address, verified_at, last_used_at, created_at, updated_at)
VALUES ($1, $2, now(), now(), now(), now())
ON CONFLICT (wallet_address) DO UPDATE
SET user_id = EXCLUDED.user_id,
    verified_at = EXCLUDED.verified_at,
    last_used_at = EXCLUDED.last_used_at,
    updated_at = EXCLUDED.updated_at`,
		userID, address)
	if err != nil {
		return fmt.Errorf("upsert app user wallet: %w", err)
	}
	return nil
}

func privyOptionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
