package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type senderType string

const (
	senderUser senderType = "user"
	senderBot  senderType = "bot"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type storedContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Metadata struct {
		FileName string  `json:"fileName"`
		Duration float64 `json:"duration"`
	} `json:"metadata"`
}

type encryptedContent struct {
	Type       string `json:"type"`
	Ciphertext string `json:"ciphertext"`
	IV         string `json:"iv"`
}

func (b *Bot) findThread(ctx context.Context, chatID int64, topic int64) (string, error) {
	var id string
	err := b.db.QueryRow(ctx, `
		SELECT id::text FROM bot_threads
		WHERE telegram_chat_id = $1 AND telegram_thread_id IS NOT DISTINCT FROM $2::integer
		LIMIT 1`, chatID, threadColumn(topic)).Scan(&id)
	return id, err
}

func (b *Bot) getOrCreateThread(ctx context.Context, userID string, chatID int64, topic int64, title string) (string, error) {
	id, err := b.findThread(ctx, chatID, topic)
	if errors.Is(err, pgx.ErrNoRows) {
		err = b.db.QueryRow(ctx, `
			INSERT INTO bot_threads (user_id, telegram_chat_id, telegram_thread_id, title, status)
			VALUES ($1::uuid, $2, $3::integer, $4, 'active')
			ON CONFLICT DO NOTHING
			RETURNING id::text`, userID, chatID, threadColumn(topic), title).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			id, err = b.findThread(ctx, chatID, topic)
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve bot thread: %w", err)
	}
	return id, nil
}

func threadColumn(topic int64) *int64 {
	if topic == 0 {
		return nil
	}
	return &topic
}

func (b *Bot) renameThread(ctx context.Context, threadID, title string) error {
	_, err := b.db.Exec(ctx, `UPDATE bot_threads SET title = $2, updated_at = $3 WHERE id = $1::uuid`, threadID, title, time.Now())
	return err
}

func (b *Bot) addMessage(ctx context.Context, threadID string, sender senderType, text string, telegramMessageID *int64) (string, error) {
	var plaintext bytes.Buffer
	encoder := json.NewEncoder(&plaintext)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(textContent{Type: "text", Text: text}); err != nil {
		return "", err
	}
	box := b.cipher.seal(bytes.TrimSuffix(plaintext.Bytes(), []byte("\n")))
	content, err := json.Marshal(encryptedContent{Type: "text", Ciphertext: box.Ciphertext, IV: box.IV})
	if err != nil {
		return "", err
	}
	var id string
	err = pgx.BeginFunc(ctx, b.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO bot_messages (thread_id, sender_type, encrypted_content, telegram_message_id)
			VALUES ($1::uuid, $2, $3::jsonb, $4)
			RETURNING id::text`, threadID, string(sender), content, telegramMessageID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE bot_threads SET updated_at = $2 WHERE id = $1::uuid`, threadID, time.Now())
		return err
	})
	if err != nil {
		return "", fmt.Errorf("store %s message: %w", sender, err)
	}
	return id, nil
}

func (b *Bot) historyForAI(ctx context.Context, threadID, lastUserMessageID string, limit int) ([]chatMessage, error) {
	rows, err := b.db.Query(ctx, `
		SELECT id::text, sender_type,
		       coalesce(encrypted_content->>'ciphertext', ''), coalesce(encrypted_content->>'iv', '')
		FROM bot_messages WHERE thread_id = $1::uuid
		  AND (sender_type <> 'user' OR created_at <= (SELECT created_at FROM bot_messages WHERE id = $3::uuid))
		ORDER BY created_at DESC LIMIT $2`, threadID, limit, lastUserMessageID)
	if err != nil {
		return nil, fmt.Errorf("load thread history: %w", err)
	}
	defer rows.Close()
	var history []chatMessage
	for rows.Next() {
		var id, sender string
		var box sealed
		if err := rows.Scan(&id, &sender, &box.Ciphertext, &box.IV); err != nil {
			return nil, fmt.Errorf("load thread history: %w", err)
		}
		plaintext, err := b.cipher.open(box)
		if err != nil {
			slog.Error("bot message skipped", "message_id", id, "error", err)
			continue
		}
		var content storedContent
		if err := json.Unmarshal(plaintext, &content); err != nil {
			slog.Error("bot message skipped", "message_id", id, "error", "content is not JSON")
			continue
		}
		if senderType(sender) == "system" {
			continue
		}
		message := chatMessage{Role: "assistant"}
		if senderType(sender) == senderUser {
			message.Role = "user"
		}
		switch content.Type {
		case "text":
			message.Content = content.Text
		case "image":
			name := content.Metadata.FileName
			if name == "" {
				name = "image"
			}
			message.Content = "[Image: " + name + "]"
		default:
			message.Content = "[Voice message: " + strconv.FormatFloat(content.Metadata.Duration, 'f', -1, 64) + "s]"
		}
		history = append(history, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load thread history: %w", err)
	}
	slices.Reverse(history)
	return history, nil
}
