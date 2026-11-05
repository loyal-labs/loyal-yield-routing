package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (b *Bot) ensureUser(ctx context.Context, telegramID int64, username *string, displayName string) (string, error) {
	var id string
	err := b.db.QueryRow(ctx, `SELECT id::text FROM users WHERE telegram_id = $1`, telegramID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = b.db.QueryRow(ctx, `
			INSERT INTO users (telegram_id, username, display_name) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
			RETURNING id::text`, telegramID, username, displayName).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = b.db.QueryRow(ctx, `SELECT id::text FROM users WHERE telegram_id = $1`, telegramID).Scan(&id)
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve telegram user: %w", err)
	}
	if _, err := b.db.Exec(ctx, `INSERT INTO user_settings (user_id) VALUES ($1::uuid) ON CONFLICT DO NOTHING`, id); err != nil {
		return "", fmt.Errorf("ensure user settings: %w", err)
	}
	return id, nil
}

func (b *Bot) disableNotifications(ctx context.Context, from User) error {
	name := from.displayName()
	switch {
	case strings.TrimSpace(name) != "":
	case strings.TrimSpace(from.Username) != "":
		name = from.Username
	default:
		name = fmt.Sprintf("Telegram User %d", from.ID)
	}
	userID, err := b.ensureUser(ctx, from.ID, from.username(), name)
	if err != nil {
		return err
	}
	_, err = b.db.Exec(ctx, `
		INSERT INTO user_settings (user_id, notifications, updated_at) VALUES ($1::uuid, false, $2)
		ON CONFLICT (user_id) DO UPDATE SET notifications = false, updated_at = EXCLUDED.updated_at`,
		userID, time.Now())
	return err
}

func (b *Bot) notificationsEnabled(ctx context.Context, userID string) (bool, error) {
	var enabled bool
	err := b.db.QueryRow(ctx, `SELECT notifications FROM user_settings WHERE user_id = $1::uuid`, userID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return enabled, err
}
