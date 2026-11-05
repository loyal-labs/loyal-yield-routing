package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	notificationSettingsText     = "Notification settings for this community\n\n⏱ <b>Time trigger</b>: Off | 24h | 48h\n🔔 <b>Master switch</b>: Off | On\n\nOnly whitelisted admins can change these settings."
	unauthorizedNotificationText = "Only authorized users can do this. Text @spacesymmetry if you want to change something."
	outdatedNotificationText     = "This settings panel is outdated. Run /notifications again."
	unauthorizedUserSettingsText = "This settings panel belongs to a different user account."
	outdatedUserSettingsText     = "This settings panel is outdated. Run /settings again."
	notificationsUpdatedText     = "Notification settings updated"
	notificationsFailedText      = "Unable to update notification settings right now."
	userSettingsUpdatedText      = "Settings updated"
	userSettingsFailedText       = "Unable to update settings right now."
)

func buttonLabel(label string, active bool) string {
	if active {
		return "✅ " + label
	}
	return label
}

type communityPanel struct {
	id      string
	enabled bool
	hours   *int
}

func (p *communityPanel) keyboard() *Keyboard {
	data := func(dimension, value string) string {
		return "notif:settings:" + p.id + ":" + dimension + ":" + value
	}
	return &Keyboard{Rows: [][]Button{
		{
			{Text: buttonLabel("⏱ Off", p.hours == nil), CallbackData: data("time", "off")},
			{Text: buttonLabel("⏱ 24h", p.hours != nil && *p.hours == 24), CallbackData: data("time", "24")},
			{Text: buttonLabel("⏱ 48h", p.hours != nil && *p.hours == 48), CallbackData: data("time", "48")},
		},
		{
			{Text: buttonLabel("🔕 Off", !p.enabled), CallbackData: data("master", "off")},
			{Text: buttonLabel("🔔 On", p.enabled), CallbackData: data("master", "on")},
		},
	}}
}

func (b *Bot) communityPanel(ctx context.Context, where string, arg any) (*communityPanel, error) {
	var p communityPanel
	err := b.db.QueryRow(ctx, `
		SELECT id::text, summary_notifications_enabled, summary_notification_time_hours
		FROM communities `+where, arg).Scan(&p.id, &p.enabled, &p.hours)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

type userPanel struct {
	userID        string
	model         string
	notifications bool
}

func (p *userPanel) text() string {
	return "Your notification settings\n\nToggle sound notifications for this bot in your private chat.\n\nCurrent model: <code>" + htmlEscaper.Replace(p.model) + "</code>"
}

func (p *userPanel) keyboard() *Keyboard {
	data := func(value string) string { return "usr:settings:" + p.userID + ":notif:" + value }
	return &Keyboard{Rows: [][]Button{{
		{Text: buttonLabel("🔕 Off", !p.notifications), CallbackData: data("off")},
		{Text: buttonLabel("🔔 On", p.notifications), CallbackData: data("on")},
	}}}
}

func (b *Bot) userPanel(ctx context.Context, userID string) (*userPanel, error) {
	p := userPanel{userID: userID}
	err := b.db.QueryRow(ctx, `SELECT model, notifications FROM user_settings WHERE user_id = $1::uuid`, userID).Scan(&p.model, &p.notifications)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (b *Bot) answer(ctx context.Context, update *Update, text string, alert bool) {
	var err error
	if text == "" {
		err = b.api.AnswerCallbackQuery(ctx, update.CallbackQuery.ID)
	} else {
		err = b.api.AnswerCallback(ctx, update.CallbackQuery.ID, text, alert, 0)
	}
	if err != nil {
		slog.Warn("telegram callback answer failed", "update_id", update.UpdateID, "error", err)
	}
}

func messageNotModified(err error) bool {
	var api *APIError
	return errors.As(err, &api) && strings.Contains(api.Description, "message is not modified")
}

func (b *Bot) editPanel(ctx context.Context, message *CallbackMessage, text string, keyboard *Keyboard) error {
	err := b.api.EditMessageText(ctx, message.Chat.ID, message.MessageID, text, SendOptions{ParseMode: "HTML", Markup: keyboard})
	if err != nil && !messageNotModified(err) {
		return err
	}
	return nil
}

func parseNotificationSettings(data string) (communityID, dimension, value string, ok bool) {
	m := notificationSettingsButton.FindStringSubmatch(data)
	if m == nil {
		return "", "", "", false
	}
	communityID, dimension, value = m[1], m[2], m[3]
	switch dimension {
	case "time":
		ok = value == "off" || value == "24" || value == "48"
	case "msg":
		ok = value == "off" || value == "500" || value == "1000"
	case "master":
		ok = value == "off" || value == "on"
	}
	return communityID, dimension, value, ok
}

var notificationUpdates = map[string]string{
	"time":   `UPDATE communities SET summary_notification_time_hours = $2::int, updated_at = $3 WHERE id = $1::uuid`,
	"msg":    `UPDATE communities SET summary_notification_message_count = $2::int, updated_at = $3 WHERE id = $1::uuid`,
	"master": `UPDATE communities SET summary_notifications_enabled = $2::boolean, updated_at = $3 WHERE id = $1::uuid`,
}

func (b *Bot) notificationSettingsCallback(ctx context.Context, update *Update) error {
	query := update.CallbackQuery
	communityID, dimension, value, ok := parseNotificationSettings(query.Data)
	if !ok || query.From == nil {
		b.answer(ctx, update, "", false)
		return nil
	}
	if err := b.applyNotificationSettings(ctx, update, communityID, dimension, value); err != nil {
		slog.Error("notification settings update failed", "update_id", update.UpdateID, "error", err)
		b.answer(ctx, update, notificationsFailedText, true)
	}
	return nil
}

func (b *Bot) applyNotificationSettings(ctx context.Context, update *Update, communityID, dimension, value string) error {
	query := update.CallbackQuery
	admin, err := b.isAdmin(ctx, query.From.ID)
	if err != nil {
		return err
	}
	if !admin {
		b.answer(ctx, update, unauthorizedNotificationText, true)
		return nil
	}
	var chatID int64
	err = b.db.QueryRow(ctx, `SELECT chat_id FROM communities WHERE id = $1::uuid`, communityID).Scan(&chatID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || query.Message == nil || chatID != query.Message.Chat.ID {
		b.answer(ctx, update, outdatedNotificationText, true)
		return nil
	}
	var arg any
	switch {
	case dimension == "master":
		arg = value == "on"
	case value == "off":
		arg = nil
	default:
		arg, _ = strconv.Atoi(value)
	}
	if _, err := b.db.Exec(ctx, notificationUpdates[dimension], communityID, arg, time.Now()); err != nil {
		return err
	}
	panel, err := b.communityPanel(ctx, "WHERE id = $1::uuid", communityID)
	if err != nil {
		return err
	}
	if panel == nil {
		b.answer(ctx, update, outdatedNotificationText, true)
		return nil
	}
	if err := b.editPanel(ctx, query.Message, notificationSettingsText, panel.keyboard()); err != nil {
		return err
	}
	b.answer(ctx, update, notificationsUpdatedText, false)
	return nil
}

func (b *Bot) userSettingsCallback(ctx context.Context, update *Update) error {
	query := update.CallbackQuery
	m := userSettingsButton.FindStringSubmatch(query.Data)
	if m == nil || query.From == nil {
		b.answer(ctx, update, "", false)
		return nil
	}
	if err := b.applyUserSettings(ctx, update, m[1], m[3] == "on"); err != nil {
		slog.Error("user settings update failed", "update_id", update.UpdateID, "error", err)
		b.answer(ctx, update, userSettingsFailedText, true)
	}
	return nil
}

func (b *Bot) applyUserSettings(ctx context.Context, update *Update, userID string, notifications bool) error {
	query := update.CallbackQuery
	var actorID string
	err := b.db.QueryRow(ctx, `SELECT id::text FROM users WHERE telegram_id = $1`, query.From.ID).Scan(&actorID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || actorID != userID {
		b.answer(ctx, update, unauthorizedUserSettingsText, true)
		return nil
	}
	if query.Message == nil || query.Message.Chat.ID != query.From.ID {
		b.answer(ctx, update, outdatedUserSettingsText, true)
		return nil
	}
	current, err := b.userPanel(ctx, userID)
	if err != nil {
		return err
	}
	if current == nil {
		b.answer(ctx, update, outdatedUserSettingsText, true)
		return nil
	}
	if _, err := b.db.Exec(ctx, `UPDATE user_settings SET notifications = $2, updated_at = $3 WHERE user_id = $1::uuid`, userID, notifications, time.Now()); err != nil {
		return err
	}
	updated, err := b.userPanel(ctx, userID)
	if err != nil {
		return err
	}
	if updated == nil {
		b.answer(ctx, update, outdatedUserSettingsText, true)
		return nil
	}
	if err := b.editPanel(ctx, query.Message, updated.text(), updated.keyboard()); err != nil {
		return err
	}
	b.answer(ctx, update, userSettingsUpdatedText, false)
	return nil
}
