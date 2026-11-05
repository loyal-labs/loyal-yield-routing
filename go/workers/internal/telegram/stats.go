package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	statsDeadline          = 7 * time.Second
	statsCompletionTimeout = 500 * time.Millisecond
	statsMaxAge            = 5 * time.Minute
	statsUnavailableText   = "Loyal stats are unavailable right now. Please try again shortly."
)

type loyalStats struct {
	totalAumRaw             int64
	totalOptimizedVolumeRaw int64
	totalUsers              int32
	refreshedAt             time.Time
}

func usdcRaw(raw int64) (string, error) {
	if raw < 0 {
		return "", errors.New("USDC metric cannot be negative")
	}
	cents := (uint64(raw) + 5_000) / 10_000
	return fmt.Sprintf("$%s.%02d", groupThousands(strconv.FormatUint(cents/100, 10)), cents%100), nil
}

func statsMessage(s loyalStats) (string, error) {
	if s.totalUsers < 0 {
		return "", errors.New("Total user count must be a non-negative safe integer")
	}
	aum, err := usdcRaw(s.totalAumRaw)
	if err != nil {
		return "", err
	}
	volume, err := usdcRaw(s.totalOptimizedVolumeRaw)
	if err != nil {
		return "", err
	}
	return "Loyal Stats\n\nTotal AUM: *" + aum + "*\nTotal Users: *" + groupThousands(strconv.Itoa(int(s.totalUsers))) + "*\nTotal Optimized Volume: *" + volume + "*", nil
}

func (b *Bot) loadStatsSnapshot(ctx context.Context, now time.Time) (loyalStats, error) {
	var s loyalStats
	err := b.db.QueryRow(ctx, `
		SELECT total_aum_raw, total_optimized_volume_raw, total_users, refreshed_at
		FROM loyal_stats_snapshots WHERE snapshot_key = 'current' LIMIT 1`).
		Scan(&s.totalAumRaw, &s.totalOptimizedVolumeRaw, &s.totalUsers, &s.refreshedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, errors.New("Loyal stats snapshot is unavailable")
	}
	if err != nil {
		return s, err
	}
	if max(0, now.Sub(s.refreshedAt)) > statsMaxAge {
		return s, errors.New("Loyal stats snapshot is stale")
	}
	return s, nil
}

func (b *Bot) claimStats(ctx context.Context, chatID int64, from *User, updateID int64) (bool, error) {
	var userID *int64
	if from != nil {
		userID = &from.ID
	}
	tag, err := b.db.Exec(ctx, `
		INSERT INTO telegram_command_receipts (chat_id, command, telegram_update_id, telegram_user_id)
		VALUES ($1, '/stats', $2, $3)
		ON CONFLICT DO NOTHING`, chatID, updateID, userID)
	return tag.RowsAffected() == 1, err
}

func (b *Bot) completeStats(ctx context.Context, updateID int64, status string, messageID *int64) error {
	ctx, cancel := context.WithTimeout(ctx, statsCompletionTimeout)
	defer cancel()
	now := time.Now()
	_, err := b.db.Exec(ctx, `
		UPDATE telegram_command_receipts
		SET completed_at = $2, status = $3, telegram_message_id = COALESCE($4, telegram_message_id), updated_at = $2
		WHERE telegram_update_id = $1`, updateID, now, status, messageID)
	return err
}

func (b *Bot) stats(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	if msg.Chat.ID != loyalCommunityChatID {
		return nil
	}
	if update.UpdateID < 0 {
		slog.Error("/stats command failed", "chat_id", msg.Chat.ID, "stage", "validate", "error", "Telegram update ID is missing or invalid")
		return nil
	}
	startedAt := time.Now()
	ctx, cancel := context.WithDeadline(ctx, startedAt.Add(statsDeadline))
	defer cancel()
	log := slog.With("command", "/stats", "chat_id", msg.Chat.ID, "update_id", update.UpdateID)

	claimed, err := b.claimStats(ctx, msg.Chat.ID, msg.From, update.UpdateID)
	if err != nil {
		log.Error("/stats command failed", "stage", "claim", "error", err)
		return nil
	}
	if !claimed {
		log.Info("/stats command duplicate skipped", "outcome", "duplicate")
		return nil
	}

	stage := "load_snapshot"
	snapshot, err := b.loadStatsSnapshot(ctx, time.Now())
	var messageID int64
	if err == nil {
		stage = "send"
		var text string
		if text, err = statsMessage(snapshot); err == nil {
			messageID, err = b.api.Send(ctx, msg.Chat.ID, text, SendOptions{ParseMode: "Markdown"})
		}
	}
	if err == nil {
		if err := b.completeStats(ctx, update.UpdateID, "completed", &messageID); err != nil {
			log.Error("/stats command failed", "stage", "complete", "error", err)
			return nil
		}
		log.Info("/stats command completed", "outcome", "completed", "message_id", messageID, "snapshot_age_ms", time.Since(snapshot.refreshedAt).Milliseconds())
		return nil
	}

	log.Error("/stats command failed", "stage", stage, "error", err)
	var failureID *int64
	if stage == "load_snapshot" {
		id, sendErr := b.api.Send(ctx, msg.Chat.ID, statsUnavailableText, SendOptions{})
		if sendErr != nil {
			log.Error("/stats failure response send failed", "stage", "send_failure", "error", sendErr)
		} else {
			failureID = &id
		}
	}
	if err := b.completeStats(ctx, update.UpdateID, "failed", failureID); err != nil {
		log.Error("/stats failed receipt completion failed", "stage", "complete_failure", "error", err)
	}
	return nil
}
