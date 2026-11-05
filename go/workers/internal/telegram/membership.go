package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

const onboardingMessage = "Thanks for adding me. Run /activate_community to enable summaries for this community.\nAfter activation, summaries are available in this chat and in the app.\nUse /notifications to set notification cycles.\nUse /hide or /unhide to control public visibility."

func statusIn(status string, set ...string) bool {
	for _, s := range set {
		if status == s {
			return true
		}
	}
	return false
}

func (b *Bot) myChatMember(ctx context.Context, update *Update) error {
	change := update.MyChatMember
	before, after := change.OldChatMember.Status, change.NewChatMember.Status
	log := slog.With("update_id", update.UpdateID, "chat_id", change.Chat.ID)

	if change.Chat.private() {
		blocked := statusIn(before, "member", "restricted") && after == "kicked"
		unblocked := before == "kicked" && statusIn(after, "member", "restricted")
		if blocked {
			if err := b.disableNotifications(ctx, change.From); err != nil {
				log.Error("disable notifications after block failed", "error", err)
			}
		}
		if blocked || unblocked {
			log.Info("telegram bot private block transition", "blocked", blocked, "old_status", before, "new_status", after)
		}
		return nil
	}
	if !change.Chat.community() {
		return nil
	}

	joined := statusIn(before, "left", "kicked") && statusIn(after, "member", "administrator")
	removed := statusIn(before, "member", "administrator", "creator", "restricted") && statusIn(after, "left", "kicked")
	if !joined && !removed {
		return nil
	}
	title := change.Chat.Title
	if title == "" {
		title = "Untitled"
	}
	var err error
	if joined {
		_, err = b.db.Exec(ctx, `
			INSERT INTO communities (activated_by, chat_id, chat_title, is_active, is_public, updated_at)
			VALUES ($1, $2, $3, false, false, $4)
			ON CONFLICT (chat_id) DO UPDATE SET
				chat_title = EXCLUDED.chat_title, is_active = false, is_public = false, updated_at = EXCLUDED.updated_at`,
			change.From.ID, change.Chat.ID, title, time.Now())
	} else {
		_, err = b.db.Exec(ctx, `
			UPDATE communities SET chat_title = $2, is_active = false, is_public = false, updated_at = $3
			WHERE chat_id = $1`, change.Chat.ID, title, time.Now())
	}
	if err != nil {
		log.Error("community state from my_chat_member failed", "error", err)
		return nil
	}
	log.Info("telegram bot group membership", "joined", joined, "old_status", before, "new_status", after)
	if joined {
		b.sendWithAutoCleanup(ctx, change.Chat, onboardingMessage)
	}
	return nil
}

func (b *Bot) sendWithAutoCleanup(ctx context.Context, chat Chat, text string) {
	sendCtx, cancel := context.WithTimeout(ctx, sideEffectTimeout)
	defer cancel()
	messageID, err := b.api.SendMessage(sendCtx, chat.ID, text)
	if err != nil {
		slog.Warn("telegram helper message send failed", "chat_id", chat.ID, "error", err)
		return
	}
	if chat.community() {
		b.scheduleHelperDeletion(ctx, chat.ID, messageID, helperMessageTTL)
	}
}

func (b *Bot) businessConnection(ctx context.Context, update *Update) error {
	connection := update.BusinessConnection
	log := slog.With("update_id", update.UpdateID, "telegram_user_id", connection.User.ID)
	userID, err := b.ensureUser(ctx, connection.User.ID, connection.User.username(), connection.User.displayName())
	if err != nil {
		log.Error("business connection user failed", "error", err)
		return nil
	}
	rights := connection.Rights
	if len(rights) == 0 || string(rights) == "null" {
		rights = json.RawMessage(`{}`)
	}
	if _, err := b.db.Exec(ctx, `
		INSERT INTO business_connections (user_id, business_connection_id, user_chat_id, is_enabled, rights, connected_at)
		VALUES ($1::uuid, $2, $3, $4, $5::text::jsonb, $6)
		ON CONFLICT (user_id) DO UPDATE SET
			business_connection_id = EXCLUDED.business_connection_id, user_chat_id = EXCLUDED.user_chat_id,
			is_enabled = EXCLUDED.is_enabled, rights = EXCLUDED.rights, connected_at = EXCLUDED.connected_at,
			updated_at = $7`,
		userID, connection.ID, connection.UserChatID, connection.IsEnabled, string(rights), time.Unix(connection.Date, 0), time.Now()); err != nil {
		log.Error("business connection upsert failed", "error", err)
		return nil
	}
	enabled, err := b.notificationsEnabled(ctx, userID)
	if err != nil {
		log.Error("business connection notification setting failed", "error", err)
		return nil
	}
	if !enabled {
		return nil
	}
	text := "You've disconnected Loyal from messages."
	if connection.IsEnabled {
		text = "You've connected Loyal to messages."
	}
	sendCtx, cancel := context.WithTimeout(ctx, sideEffectTimeout)
	defer cancel()
	if _, err := b.api.SendMessage(sendCtx, connection.UserChatID, text); err != nil {
		log.Warn("business connection notification failed", "error", err)
	}
	return nil
}
