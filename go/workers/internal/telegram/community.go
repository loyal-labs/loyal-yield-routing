package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/jackc/pgx/v5"
)

var positiveReactions = []string{"❤", "🔥", "🎉", "🤩", "🙏", "😍", "❤‍🔥", "💯", "⚡"}

func (b *Bot) communityText(ctx context.Context, update *Update) error {
	message := update.Message
	if err := b.ingest(ctx, message); err != nil {
		return fmt.Errorf("community ingest chat %d message %d: %w", message.Chat.ID, message.MessageID, err)
	}
	b.reactToGLoyal(ctx, update)
	return nil
}

func (b *Bot) ingest(ctx context.Context, message *Message) error {
	if !message.Chat.community() || message.From == nil {
		return nil
	}
	var communityID string
	err := b.db.QueryRow(ctx, `
		SELECT id::text FROM communities
		WHERE chat_id = $1 AND is_active AND parser_type = 'bot'
		LIMIT 1`, message.Chat.ID).Scan(&communityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("active community: %w", err)
	}
	userID, err := b.ensureUser(ctx, message.From.ID, message.From.username(), message.From.displayName())
	if err != nil {
		return err
	}
	if _, err := b.db.Exec(ctx, `
		INSERT INTO community_members (community_id, user_id) VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING`, communityID, userID); err != nil {
		return fmt.Errorf("community member: %w", err)
	}
	if _, err := b.db.Exec(ctx, `
		INSERT INTO messages (community_id, user_id, telegram_message_id, content) VALUES ($1::uuid, $2::uuid, $3, $4)
		ON CONFLICT DO NOTHING`, communityID, userID, message.MessageID, message.Text); err != nil {
		return fmt.Errorf("message: %w", err)
	}
	return nil
}

func (b *Bot) reactToGLoyal(ctx context.Context, update *Update) {
	message := update.Message
	if !strings.Contains(strings.ToLower(message.Text), "gloyal") || !message.Chat.group() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sideEffectTimeout)
	defer cancel()
	emoji := positiveReactions[rand.IntN(len(positiveReactions))]
	if err := b.api.SetMessageReaction(ctx, message.Chat.ID, message.MessageID, emoji); err != nil {
		slog.Warn("gLoyal reaction failed", "update_id", update.UpdateID, "chat_id", message.Chat.ID, "message_id", message.MessageID, "error", err)
	}
}
