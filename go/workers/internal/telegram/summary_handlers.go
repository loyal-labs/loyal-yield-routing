package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const (
	duplicateVoteCacheSeconds = 300
	voteAlreadyText           = "You've voted already!"
	voteScoreFailedText       = "Unable to load summary score right now."
	voteUpdateFailedText      = "Unable to update summary score right now."
)

func (b *Bot) summarySource(chatID int64) int64 {
	if b.summaryPeer != nil && chatID == b.summaryPeer[0] {
		return b.summaryPeer[1]
	}
	return chatID
}

func (b *Bot) summaryCommand(ctx context.Context, update *Update) error {
	msg := commandMessage(update)
	if !msg.Chat.community() {
		if err := b.replyWithAutoCleanup(ctx, msg, groupOnlyText, SendOptions{}); err != nil {
			return b.sendFailed(update, "/summary", err)
		}
		return nil
	}
	source := b.summarySource(msg.Chat.ID)
	d, err := b.sendLatestSummary(ctx, source, msg.Chat.ID, msg.MessageID)
	switch {
	case err != nil:
		b.suppress("/summary", msg, "internal_error", err)
	case d.reason != "":
		b.suppress("/summary", msg, d.reason, nil)
	default:
		slog.Info("telegram bot command", "command", "/summary", "chat_id", msg.Chat.ID, "chat_type", msg.Chat.Type, "summary_source_chat_id", source)
	}
	return nil
}

type inlineArticle struct {
	Description         string          `json:"description"`
	ID                  string          `json:"id"`
	InputMessageContent inlineHTMLInput `json:"input_message_content"`
	ReplyMarkup         *Keyboard       `json:"reply_markup"`
	Title               string          `json:"title"`
	Type                string          `json:"type"`
}

type inlineHTMLInput struct {
	LinkPreviewOptions linkPreview `json:"link_preview_options"`
	MessageText        string      `json:"message_text"`
	ParseMode          string      `json:"parse_mode"`
}

func (b *Bot) answerInline(ctx context.Context, update *Update, results []inlineArticle, personal bool) error {
	if err := b.api.AnswerInlineQuery(ctx, update.InlineQuery.ID, results, personal); err != nil {
		return b.sendFailed(update, "inline_query", err)
	}
	return nil
}

func (b *Bot) inline(ctx context.Context, update *Update) error {
	return b.answerInline(ctx, update, []inlineArticle{}, false)
}

func (b *Bot) inlineSummary(ctx context.Context, update *Update) error {
	m := summaryInlineQuery.FindStringSubmatch(update.InlineQuery.Query)
	chatID, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return b.answerInline(ctx, update, []inlineArticle{}, true)
	}
	var communityID, title string
	err = b.db.QueryRow(ctx, `SELECT id::text, chat_title FROM communities WHERE chat_id = $1 AND is_active AND is_public LIMIT 1`, chatID).Scan(&communityID, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		return b.answerInline(ctx, update, []inlineArticle{}, true)
	}
	if err != nil {
		return err
	}
	rows, err := b.db.Query(ctx, `SELECT `+summaryColumns+` FROM summaries s WHERE s.community_id = $1::uuid ORDER BY s.created_at DESC LIMIT $2`, communityID, inlineResultsLimit)
	if err != nil {
		return err
	}
	summaries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*storedSummary, error) { return scanSummary(row) })
	if err != nil {
		return err
	}
	results := make([]inlineArticle, 0, len(summaries))
	for _, s := range summaries {
		text, keyboard, err := b.summaryPayload(ctx, s, chatID)
		if err != nil {
			return err
		}
		results = append(results, inlineArticle{
			Description:         s.oneliner,
			ID:                  s.id,
			InputMessageContent: inlineHTMLInput{LinkPreviewOptions: linkPreview{PreferLargeMedia: true}, MessageText: text, ParseMode: "HTML"},
			ReplyMarkup:         keyboard,
			Title:               title + " • " + s.createdAt.UTC().Format("2006-01-02"),
			Type:                "article",
		})
	}
	return b.answerInline(ctx, update, results, true)
}

func (b *Bot) summaryVoteCallback(ctx context.Context, update *Update) error {
	query := update.CallbackQuery
	m := summaryVoteButton.FindStringSubmatch(query.Data)
	action, summaryID, groupChatID := m[1], m[2], canonicalChatID(m[3])
	if action == "s" {
		likes, dislikes, err := b.voteTotals(ctx, summaryID)
		if err != nil {
			slog.Error("summary score load failed", "update_id", update.UpdateID, "error", err)
			b.answer(ctx, update, voteScoreFailedText, true)
			return nil
		}
		b.answer(ctx, update, fmt.Sprintf("Current summary score is %d.", likes-dislikes), true)
		return nil
	}
	if query.Message == nil || query.From == nil {
		b.answer(ctx, update, "", false)
		return nil
	}
	err := b.applyVote(ctx, update, summaryID, groupChatID, map[string]string{"u": "LIKE", "d": "DISLIKE"}[action])
	switch {
	case err == nil:
	case messageNotModified(err):
		b.answer(ctx, update, "", false)
	default:
		slog.Error("summary vote update failed", "update_id", update.UpdateID, "error", err)
		b.answer(ctx, update, voteUpdateFailedText, true)
	}
	return nil
}

func (b *Bot) applyVote(ctx context.Context, update *Update, summaryID, groupChatID, vote string) error {
	query := update.CallbackQuery
	userID, err := b.ensureUser(ctx, query.From.ID, query.From.username(), query.From.displayName())
	if err != nil {
		return err
	}
	tag, err := b.db.Exec(ctx, `INSERT INTO summary_votes (action, summary_id, user_id) VALUES ($1, $2::uuid, $3::uuid) ON CONFLICT DO NOTHING`, vote, summaryID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if err := b.api.AnswerCallback(ctx, query.ID, voteAlreadyText, true, duplicateVoteCacheSeconds); err != nil {
			slog.Warn("telegram callback answer failed", "update_id", update.UpdateID, "error", err)
		}
		return nil
	}
	slog.Info("telegram summary vote", "vote_action", vote, "summary_id", summaryID, "group_chat_id", groupChatID,
		"telegram_chat_id", query.Message.Chat.ID, "telegram_chat_type", query.Message.Chat.Type, "telegram_user_id", query.From.ID)
	likes, dislikes, err := b.voteTotals(ctx, summaryID)
	if err != nil {
		return err
	}
	keyboard, err := summaryVoteKeyboard(groupChatID, summaryID, likes, dislikes)
	if err != nil {
		return err
	}
	if err := b.api.EditMessageReplyMarkup(ctx, query.Message.Chat.ID, query.Message.MessageID, keyboard); err != nil {
		return err
	}
	b.answer(ctx, update, "", false)
	return nil
}
