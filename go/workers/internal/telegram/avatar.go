package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const avatarCaptureTimeout = 2500 * time.Millisecond

func imageExtension(contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "jpg"
}

func (b *Bot) captureAvatar(ctx context.Context, telegramID int64) (string, error) {
	fileID, err := b.api.FirstProfilePhotoFileID(ctx, telegramID)
	if err != nil || fileID == "" {
		return "", err
	}
	body, contentType, err := b.api.DownloadFile(ctx, fileID)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(telegramID, 10)))
	key := b.r2.Key(fmt.Sprintf("telegram/users/%s/profile.%s", hex.EncodeToString(sum[:]), imageExtension(contentType)))
	if err := b.r2.Put(ctx, key, body, contentType); err != nil {
		return "", err
	}
	return b.r2.PublicURL(key), nil
}

func (b *Bot) backfillAvatar(ctx context.Context, telegramID int64, userID string) error {
	if b.r2 == nil {
		return nil
	}
	captureCtx, cancel := context.WithTimeout(ctx, avatarCaptureTimeout)
	avatarURL, err := b.captureAvatar(captureCtx, telegramID)
	cancel()
	if err != nil {
		slog.Warn("telegram profile photo capture failed", "telegram_user_id", telegramID, "error", err)
		return nil
	}
	if avatarURL == "" {
		return nil
	}
	if _, err := b.db.Exec(ctx, `UPDATE users SET avatar_url = $1, updated_at = $2 WHERE id = $3::uuid`, avatarURL, time.Now(), userID); err != nil {
		return fmt.Errorf("save avatar: %w", err)
	}
	return nil
}
