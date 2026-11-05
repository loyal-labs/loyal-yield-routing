package telegram

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	directWorkers       = 16
	directBacklog       = 256
	directReplyDeadline = 5 * time.Minute

	historyLimit     = 20
	redpillModel     = "phala/gpt-oss-120b"
	draftInterval    = 500 * time.Millisecond
	maxReplyChunk    = 4096 - 100
	telegramAttempts = 3
	retryBaseDelay   = time.Second
)

const systemPrompt = `You are a helpful AI assistant integrated into Telegram. You provide concise, friendly, and accurate responses.

Guidelines:
- Be helpful and conversational
- Keep responses focused and not overly long unless the user asks for detailed explanations
- Use Telegram-compatible formatting when helpful (bold with *text*, italic with _text_, code with ` + "`code`" + `)
- If you don't know something, say so honestly
- Be respectful and professional`

const errorReply = "Sorry, I encountered an error processing your message. Please try again."

var (
	errDirectBacklogFull = errors.New("direct chat backlog is full")
	errUserBlocked       = errors.New("user blocked the bot")
)

func (b *Bot) enqueueDirect(_ context.Context, update *Update) error {
	if !b.direct.enqueue(update.Message.Chat.ID, update) {
		return errDirectBacklogFull
	}
	return nil
}

func (b *Bot) DrainDirectChats(ctx context.Context) {
	b.direct.drain(ctx)
}

func (b *Bot) directUpdate(ctx context.Context, update *Update) {
	if update.Message.ForumTopicCreated != nil {
		b.directTopicCreated(ctx, update)
		return
	}
	b.directMessage(ctx, update)
}

func (b *Bot) directTopicCreated(ctx context.Context, update *Update) {
	message := update.Message
	if message.From == nil || message.MessageThreadID == nil || message.ForumTopicCreated.Name == "" {
		slog.Warn("direct topic created message is missing fields", "update_id", update.UpdateID)
		return
	}
	name := message.ForumTopicCreated.Name
	err := func() error {
		userID, err := b.ensureUser(ctx, message.From.ID, message.From.username(), message.From.displayName())
		if err != nil {
			return err
		}
		threadID, err := b.getOrCreateThread(ctx, userID, message.Chat.ID, message.MessageThreadID, name)
		if err != nil {
			return err
		}
		return b.renameThread(ctx, threadID, name)
	}()
	if err != nil {
		slog.Error("direct topic created message failed", "update_id", update.UpdateID, "error", err)
	}
}

func (b *Bot) directMessage(ctx context.Context, update *Update) {
	message := update.Message
	if message.From == nil {
		slog.Error("direct message is missing fields", "update_id", update.UpdateID)
		return
	}
	err := b.answerDirectMessage(ctx, message)
	if err == nil {
		return
	}
	slog.Error("direct message failed", "update_id", update.UpdateID, "error", err)
	replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideEffectTimeout)
	defer cancel()
	if _, err := b.api.SendTopicMessage(replyCtx, message.Chat.ID, message.MessageThreadID, errorReply); err != nil {
		slog.Error("direct message error reply failed", "update_id", update.UpdateID, "error", err)
	}
}

func (b *Bot) answerDirectMessage(ctx context.Context, message *Message) error {
	from := message.From
	displayName := from.displayName()
	userID, err := b.ensureUser(ctx, from.ID, from.username(), displayName)
	if err != nil {
		return err
	}
	threadID, err := b.getOrCreateThread(ctx, userID, message.Chat.ID, message.MessageThreadID, "Conversation with "+displayName)
	if err != nil {
		return err
	}
	if _, err := b.addMessage(ctx, threadID, senderUser, message.Text, &message.MessageID); err != nil {
		return err
	}
	history, err := b.historyForAI(ctx, threadID, historyLimit)
	if err != nil {
		return err
	}
	return b.streamReply(ctx, message.Chat.ID, message.MessageThreadID, threadID, history)
}

func (b *Bot) streamReply(ctx context.Context, chatID int64, topic *int64, threadID string, history []chatMessage) error {
	var full strings.Builder
	var sent []int64
	var lastUpdate time.Time
	var fallback int64
	chunk := ""
	draftID := newDraftID()
	fail := func(err error) error {
		if full.Len() > 0 {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideEffectTimeout)
			defer cancel()
			if _, saveErr := b.addMessage(saveCtx, threadID, senderBot, full.String(), nil); saveErr != nil {
				slog.Error("partial bot reply not stored", "thread_id", threadID, "error", saveErr)
			}
		}
		return err
	}
	request := completionRequest{
		Model:       redpillModel,
		Messages:    append([]chatMessage{{Role: "system", Content: systemPrompt}}, history...),
		Temperature: 0.7,
		Stream:      true,
	}
	for token, err := range b.redpill.stream(ctx, request) {
		if err != nil {
			return fail(err)
		}
		full.WriteString(token)
		chunk += token
		now := time.Now()
		if now.Sub(lastUpdate) < draftInterval {
			continue
		}
		if utf16Len(chunk) > maxReplyChunk {
			parts := splitAtSentenceBoundary(chunk, maxReplyChunk)
			if fallback != 0 {
				if err := b.editReply(ctx, chatID, fallback, parts[0]); err != nil {
					return fail(err)
				}
				sent = append(sent, fallback)
			} else {
				id, err := b.sendReply(ctx, chatID, topic, parts[0])
				if err != nil {
					return fail(err)
				}
				sent = append(sent, id)
			}
			chunk = strings.Join(parts[1:], "")
			fallback = 0
			draftID = newDraftID()
		} else if err := b.sendDraft(ctx, chatID, topic, draftID, chunk); err != nil {
			slog.Warn("sendMessageDraft failed, using fallback", "chat_id", chatID, "error", err)
			if fallback != 0 {
				if err := b.editReply(ctx, chatID, fallback, chunk); err != nil {
					slog.Warn("fallback message edit failed", "chat_id", chatID, "error", err)
				}
			} else if id, err := b.sendReply(ctx, chatID, topic, chunk); err != nil {
				slog.Error("fallback message send failed", "chat_id", chatID, "error", err)
			} else {
				fallback = id
			}
		}
		lastUpdate = now
	}
	if chunk != "" {
		for i, part := range splitAtSentenceBoundary(chunk, maxReplyChunk) {
			if i == 0 && fallback != 0 {
				if err := b.editReply(ctx, chatID, fallback, part); err != nil {
					return fail(err)
				}
				sent = append(sent, fallback)
				continue
			}
			id, err := b.sendReply(ctx, chatID, topic, part)
			if err != nil {
				return fail(err)
			}
			sent = append(sent, id)
		}
	}
	var first *int64
	if len(sent) > 0 {
		first = &sent[0]
	}
	if _, err := b.addMessage(ctx, threadID, senderBot, full.String(), first); err != nil {
		return fail(err)
	}
	return nil
}

func newDraftID() int64 {
	return rand.Int64N(1<<31-1) + 1
}

func (b *Bot) sendDraft(ctx context.Context, chatID int64, topic *int64, draftID int64, text string) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, b.api.SendMessageDraft(ctx, chatID, topic, draftID, text)
	})
	return err
}

func (b *Bot) sendReply(ctx context.Context, chatID int64, topic *int64, text string) (int64, error) {
	return withRetry(ctx, func() (int64, error) {
		return b.api.SendTopicMessage(ctx, chatID, topic, text)
	})
}

func (b *Bot) editReply(ctx context.Context, chatID, messageID int64, text string) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		err := b.api.EditMessageText(ctx, chatID, messageID, text)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(apiErr.Description, "message is not modified") {
			return struct{}{}, nil
		}
		return struct{}{}, err
	})
	return err
}

func withRetry[T any](ctx context.Context, attempt func() (T, error)) (T, error) {
	var zero T
	var last error
	for i := range telegramAttempts {
		value, err := attempt()
		if err == nil {
			return value, nil
		}
		last = err
		wait := time.Duration(0)
		var apiErr *APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Code == 429:
			wait = time.Duration(max(apiErr.RetryAfter, 1)) * time.Second
		case errors.As(err, &apiErr) && apiErr.Code == 403:
			return zero, errUserBlocked
		case i < telegramAttempts-1:
			wait = retryBaseDelay << i
		}
		if wait == 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		}
	}
	return zero, last
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

var sentenceEndings = [][]uint16{
	utf16.Encode([]rune(". ")), utf16.Encode([]rune("! ")), utf16.Encode([]rune("? ")),
	utf16.Encode([]rune(".\n")), utf16.Encode([]rune("!\n")), utf16.Encode([]rune("?\n")),
}

func splitAtSentenceBoundary(text string, maxLength int) []string {
	remaining := utf16.Encode([]rune(text))
	if len(remaining) <= maxLength {
		return []string{text}
	}
	var parts []string
	for len(remaining) > maxLength {
		chunk := remaining[:maxLength]
		split := -1
		for _, ending := range sentenceEndings {
			split = max(split, lastIndexUnits(chunk, ending))
		}
		if split*2 > maxLength {
			parts = append(parts, unitsString(trimJSSpace(remaining[:split+1])))
			remaining = trimJSSpace(remaining[split+2:])
			continue
		}
		if space := lastIndexUnits(chunk, []uint16{' '}); space*5 > maxLength*4 {
			parts = append(parts, unitsString(trimJSSpace(remaining[:space])))
			remaining = trimJSSpace(remaining[space+1:])
			continue
		}
		parts = append(parts, unitsString(trimJSSpace(chunk)))
		remaining = trimJSSpace(remaining[maxLength:])
	}
	if len(remaining) > 0 {
		parts = append(parts, unitsString(remaining))
	}
	return parts
}

func lastIndexUnits(haystack, needle []uint16) int {
	for i := len(haystack) - len(needle); i >= 0; i-- {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func unitsString(units []uint16) string {
	return string(utf16.Decode(units))
}

func trimJSSpace(units []uint16) []uint16 {
	start, end := 0, len(units)
	for start < end && jsSpaceUnit(units[start]) {
		start++
	}
	for end > start && jsSpaceUnit(units[end-1]) {
		end--
	}
	return units[start:end]
}

func jsSpaceUnit(u uint16) bool {
	switch {
	case u >= 0x09 && u <= 0x0d, u == 0x20, u == 0xa0, u == 0x1680, u >= 0x2000 && u <= 0x200a,
		u == 0x2028, u == 0x2029, u == 0x202f, u == 0x205f, u == 0x3000, u == 0xfeff:
		return true
	}
	return false
}
