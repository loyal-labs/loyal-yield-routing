package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	groupOnlyText         = "This command can only be used in group chats."
	settingsLoadErrorText = "Unable to load your settings right now. Please try again."
	sunsetText            = "The Loyal Telegram app is sunset. Loyal now lives at askloyal.com.\n\nIf you have a wallet in the Telegram app, open the app to export your private key."
	miniAppLink           = "https://t.me/askloyal_tgbot/app"
)

func redeliverable(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return true
	}
	return api.Code >= 500 || api.Code == 429
}

func (b *Bot) sendFailed(update *Update, what string, err error) error {
	slog.Warn("telegram command reply failed", "update_id", update.UpdateID, "command", what, "error", err)
	if redeliverable(err) {
		return err
	}
	return nil
}

func (b *Bot) reply(ctx context.Context, msg *Message, text string, opts SendOptions) (int64, error) {
	if opts.ThreadID == 0 && msg.IsTopicMessage {
		opts.ThreadID = msg.MessageThreadID
	}
	return b.api.Send(ctx, msg.Chat.ID, text, opts)
}

func (b *Bot) replyWithAutoCleanup(ctx context.Context, msg *Message, text string, opts SendOptions) error {
	messageID, err := b.reply(ctx, msg, text, opts)
	if err != nil {
		return err
	}
	if msg.Chat.community() {
		b.scheduleHelperDeletion(ctx, msg.Chat.ID, messageID, helperMessageTTL)
	}
	return nil
}

func (b *Bot) suppress(command string, msg *Message, reason string, err error) {
	args := []any{"command", command, "reason", reason, "telegram_chat_id", msg.Chat.ID, "telegram_chat_type", msg.Chat.Type}
	if msg.From != nil {
		args = append(args, "telegram_user_id", msg.From.ID)
	}
	if err != nil {
		slog.Error("suppressed community command reply", append(args, "error", err)...)
		return
	}
	slog.Warn("suppressed community command reply", args...)
}

func (b *Bot) isAdmin(ctx context.Context, telegramID int64) (bool, error) {
	var found bool
	err := b.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM admins WHERE telegram_id = $1)`, telegramID).Scan(&found)
	return found, err
}

type community struct {
	id     string
	active bool
	public bool
}

func (b *Bot) communityByChat(ctx context.Context, chatID int64) (*community, error) {
	var c community
	err := b.db.QueryRow(ctx, `SELECT id::text, is_active, is_public FROM communities WHERE chat_id = $1`, chatID).Scan(&c.id, &c.active, &c.public)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (b *Bot) start(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	keyboard := &Keyboard{Rows: [][]Button{
		{{Text: "Open askloyal.com", URL: "https://askloyal.com"}},
		{{Text: "Export wallet key", URL: miniAppLink}},
	}}
	if _, err := b.reply(ctx, msg, sunsetText, SendOptions{Markup: keyboard, ThreadID: msg.MessageThreadID}); err != nil {
		return b.sendFailed(update, "/start", err)
	}
	slog.Info("telegram bot command", "command", "/start", "chat_id", msg.Chat.ID, "chat_type", msg.Chat.Type)
	return nil
}

type adminCommand struct {
	name string
	run  func(ctx context.Context, b *Bot, msg *Message) (reason string, err error)
}

func (b *Bot) adminCommand(c adminCommand) Handler {
	return func(ctx context.Context, update *Update) error {
		msg := commandMessage(update)
		if msg.From == nil {
			return nil
		}
		if err := b.api.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID); err != nil {
			slog.Warn("command message delete failed", "command", c.name, "error", err)
		}
		if !msg.Chat.community() {
			if err := b.replyWithAutoCleanup(ctx, msg, groupOnlyText, SendOptions{}); err != nil {
				return b.sendFailed(update, c.name, err)
			}
			return nil
		}
		admin, err := b.isAdmin(ctx, msg.From.ID)
		if err != nil {
			b.suppress(c.name, msg, "internal_error", err)
			return nil
		}
		if !admin {
			b.suppress(c.name, msg, "unauthorized", nil)
			return nil
		}
		reason, err := c.run(ctx, b, msg)
		if err != nil {
			b.suppress(c.name, msg, "internal_error", err)
		} else if reason != "" {
			b.suppress(c.name, msg, reason, nil)
		}
		return nil
	}
}

func chatTitle(chat Chat) string {
	if chat.Title == "" {
		return "Untitled"
	}
	return chat.Title
}

func activateCommunity(ctx context.Context, b *Bot, msg *Message) (string, error) {
	existing, err := b.communityByChat(ctx, msg.Chat.ID)
	if err != nil {
		return "", err
	}
	if existing == nil {
		name := msg.From.displayName()
		if _, err := b.ensureUser(ctx, msg.From.ID, msg.From.username(), name); err != nil {
			return "", err
		}
		tag, err := b.db.Exec(ctx, `
			INSERT INTO communities (chat_id, chat_title, activated_by, is_public, settings)
			VALUES ($1, $2, $3, false, '{}'::jsonb)
			ON CONFLICT DO NOTHING`, msg.Chat.ID, chatTitle(msg.Chat), msg.From.ID)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() > 0 {
			return "", b.replyWithAutoCleanup(ctx, msg, "Community activated for message tracking!", SendOptions{})
		}
		if existing, err = b.communityByChat(ctx, msg.Chat.ID); err != nil {
			return "", err
		}
		if existing == nil {
			return "insert_conflict_community_missing", nil
		}
	}
	if existing.active {
		if _, err := b.db.Exec(ctx, `UPDATE communities SET chat_title = $2, updated_at = $3 WHERE id = $1::uuid`, existing.id, chatTitle(msg.Chat), time.Now()); err != nil {
			return "", err
		}
		return "", b.replyWithAutoCleanup(ctx, msg, "Community is already activated. Data updated!", SendOptions{})
	}
	if _, err := b.db.Exec(ctx, `UPDATE communities SET is_active = true, chat_title = $2, updated_at = $3 WHERE id = $1::uuid`, existing.id, chatTitle(msg.Chat), time.Now()); err != nil {
		return "", err
	}
	return "", b.replyWithAutoCleanup(ctx, msg, "Community reactivated for message tracking!", SendOptions{})
}

func deactivateCommunity(ctx context.Context, b *Bot, msg *Message) (string, error) {
	existing, err := b.communityByChat(ctx, msg.Chat.ID)
	if err != nil {
		return "", err
	}
	if existing == nil {
		return "not_activated", nil
	}
	if !existing.active {
		return "already_deactivated", nil
	}
	if _, err := b.db.Exec(ctx, `UPDATE communities SET is_active = false, updated_at = $2 WHERE id = $1::uuid`, existing.id, time.Now()); err != nil {
		return "", err
	}
	return "", b.replyWithAutoCleanup(ctx, msg, "Community deactivated. Message tracking has been disabled.", SendOptions{})
}

func setCommunityVisibility(public bool) func(ctx context.Context, b *Bot, msg *Message) (string, error) {
	already, done := "already_hidden", "Community hidden from public summaries."
	if public {
		already, done = "already_visible", "Community is now visible in public summaries."
	}
	return func(ctx context.Context, b *Bot, msg *Message) (string, error) {
		existing, err := b.communityByChat(ctx, msg.Chat.ID)
		if err != nil {
			return "", err
		}
		if existing == nil || !existing.active {
			return "not_activated", nil
		}
		if existing.public == public {
			return already, nil
		}
		if _, err := b.db.Exec(ctx, `UPDATE communities SET is_public = $2, updated_at = $3 WHERE id = $1::uuid`, existing.id, public, time.Now()); err != nil {
			return "", err
		}
		return "", b.replyWithAutoCleanup(ctx, msg, done, SendOptions{})
	}
}

func (b *Bot) notifications(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	if !msg.Chat.community() {
		if err := b.replyWithAutoCleanup(ctx, msg, groupOnlyText, SendOptions{}); err != nil {
			return b.sendFailed(update, "/notifications", err)
		}
		return nil
	}
	if msg.From == nil {
		b.suppress("/notifications", msg, "missing_user_context", nil)
		return nil
	}
	admin, err := b.isAdmin(ctx, msg.From.ID)
	if err != nil {
		b.suppress("/notifications", msg, "internal_error", err)
		return nil
	}
	if !admin {
		b.suppress("/notifications", msg, "unauthorized", nil)
		return nil
	}
	panel, err := b.communityPanel(ctx, "WHERE chat_id = $1 AND is_active", msg.Chat.ID)
	if err != nil {
		b.suppress("/notifications", msg, "internal_error", err)
		return nil
	}
	if panel == nil {
		b.suppress("/notifications", msg, "not_activated", nil)
		return nil
	}
	if err := b.replyWithAutoCleanup(ctx, msg, notificationSettingsText, SendOptions{ParseMode: "HTML", Markup: panel.keyboard()}); err != nil {
		b.suppress("/notifications", msg, "internal_error", err)
	}
	return nil
}

func (b *Bot) settings(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	if msg.From == nil || !msg.Chat.private() {
		return nil
	}
	if err := b.sendUserSettings(ctx, msg); err != nil {
		slog.Error("user settings send failed", "update_id", update.UpdateID, "error", err)
		if _, err := b.reply(ctx, msg, settingsLoadErrorText, SendOptions{}); err != nil {
			return b.sendFailed(update, "/settings", err)
		}
	}
	return nil
}

func (b *Bot) sendUserSettings(ctx context.Context, msg *Message) error {
	userID, err := b.ensureUser(ctx, msg.From.ID, msg.From.username(), msg.From.displayName())
	if err != nil {
		return err
	}
	if _, err := b.db.Exec(ctx, `INSERT INTO user_settings (user_id) VALUES ($1::uuid) ON CONFLICT DO NOTHING`, userID); err != nil {
		return err
	}
	panel, err := b.userPanel(ctx, userID)
	if err != nil {
		return err
	}
	if panel == nil {
		return pgx.ErrNoRows
	}
	_, err = b.reply(ctx, msg, panel.text(), SendOptions{ParseMode: "HTML", Markup: panel.keyboard()})
	return err
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
