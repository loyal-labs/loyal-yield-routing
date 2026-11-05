package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"
)

const (
	helperMessageTTL      = time.Minute
	cleanupInterval       = time.Minute
	cleanupBatchLimit     = 200
	cleanupClaimLease     = 2 * time.Minute
	cleanupRetryDelay     = time.Minute
	cleanupMaxRetryWindow = 24 * time.Hour
)

var terminalDeleteDescription = regexp.MustCompile(`(?i)message to delete not found|message can't be deleted|chat not found|bot was kicked|forbidden`)

type CleanupStats struct {
	Claimed               int `json:"claimed"`
	DroppedExpired        int `json:"droppedExpired"`
	Due                   int `json:"due"`
	QueueDeleted          int `json:"queueDeleted"`
	RetryScheduled        int `json:"retryScheduled"`
	SkippedAlreadyClaimed int `json:"skippedAlreadyClaimed"`
	SuccessfulDeletes     int `json:"successfulDeletes"`
	TerminalErrors        int `json:"terminalErrors"`
	TransientErrors       int `json:"transientErrors"`
}

func (b *Bot) scheduleHelperDeletion(ctx context.Context, chatID, messageID int64, ttl time.Duration) {
	if _, err := b.db.Exec(ctx, `
		INSERT INTO telegram_helper_message_cleanup (chat_id, message_id, delete_after) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, chatID, messageID, time.Now().Add(max(ttl, 0))); err != nil {
		slog.Error("schedule helper message cleanup failed", "chat_id", chatID, "message_id", messageID, "error", err)
	}
}

func (b *Bot) RunHelperCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		stats, err := b.CleanHelperMessages(ctx, time.Now())
		switch {
		case err != nil:
			slog.Error("telegram helper cleanup failed", "error", err)
		case stats.Due > 0:
			slog.Info("telegram helper cleanup", "stats", stats)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) CleanHelperMessages(ctx context.Context, now time.Time) (CleanupStats, error) {
	var stats CleanupStats
	type item struct {
		id        string
		chatID    int64
		messageID int64
		createdAt time.Time
	}
	rows, err := b.db.Query(ctx, `
		SELECT id::text, chat_id, message_id, created_at FROM telegram_helper_message_cleanup
		WHERE delete_after <= $1 ORDER BY delete_after LIMIT $2`, now, cleanupBatchLimit)
	if err != nil {
		return stats, fmt.Errorf("due helper messages: %w", err)
	}
	var due []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.chatID, &it.messageID, &it.createdAt); err != nil {
			rows.Close()
			return stats, fmt.Errorf("due helper messages: %w", err)
		}
		due = append(due, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("due helper messages: %w", err)
	}
	stats.Due = len(due)

	var finished []string
	for _, it := range due {
		claim, err := b.db.Exec(ctx, `
			UPDATE telegram_helper_message_cleanup SET delete_after = $3
			WHERE id = $1::uuid AND delete_after <= $2`, it.id, now, now.Add(cleanupClaimLease))
		if err != nil {
			return stats, fmt.Errorf("claim helper message: %w", err)
		}
		if claim.RowsAffected() == 0 {
			stats.SkippedAlreadyClaimed++
			continue
		}
		stats.Claimed++

		err = b.api.DeleteMessage(ctx, it.chatID, it.messageID)
		log := slog.With("queue_id", it.id, "chat_id", it.chatID, "message_id", it.messageID)
		switch {
		case err == nil:
			stats.SuccessfulDeletes++
			finished = append(finished, it.id)
			continue
		case !transientDeleteError(err):
			stats.TerminalErrors++
			log.Warn("helper message delete failed for good", "error", err)
			finished = append(finished, it.id)
			continue
		}
		stats.TransientErrors++
		log.Warn("helper message delete failed, will retry", "error", err)
		if now.Sub(it.createdAt) > cleanupMaxRetryWindow {
			stats.DroppedExpired++
			finished = append(finished, it.id)
			continue
		}
		if _, err := b.db.Exec(ctx, `UPDATE telegram_helper_message_cleanup SET delete_after = $2 WHERE id = $1::uuid`,
			it.id, now.Add(cleanupRetryDelay)); err != nil {
			return stats, fmt.Errorf("reschedule helper message: %w", err)
		}
		stats.RetryScheduled++
	}

	if len(finished) > 0 {
		if _, err := b.db.Exec(ctx, `DELETE FROM telegram_helper_message_cleanup WHERE id = ANY($1::text[]::uuid[])`, finished); err != nil {
			return stats, fmt.Errorf("delete finished helper messages: %w", err)
		}
	}
	stats.QueueDeleted = len(finished)
	return stats, nil
}

func transientDeleteError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	switch {
	case apiErr.Code == 429 || apiErr.Code >= 500:
		return true
	case apiErr.Code == 400 || apiErr.Code == 403:
		return false
	}
	return !terminalDeleteDescription.MatchString(apiErr.Description)
}
