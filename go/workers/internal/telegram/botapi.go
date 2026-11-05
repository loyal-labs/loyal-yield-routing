package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type BotAPI struct {
	base   string
	token  string
	client *http.Client
}

func NewBotAPI(base, token string) *BotAPI {
	return &BotAPI{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

func (a *BotAPI) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/bot"+a.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: invalid request", method)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read: %w", method, err)
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d with an unreadable body", method, response.StatusCode)
	}
	if !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = response.StatusCode
		}
		return &APIError{Method: method, Code: code, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("telegram %s: result: %w", method, err)
	}
	return nil
}

func (a *BotAPI) GetMe(ctx context.Context) (User, error) {
	var me User
	err := a.call(ctx, "getMe", struct{}{}, &me)
	return me, err
}

func (a *BotAPI) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	return a.SendTopicMessage(ctx, chatID, nil, text)
}

func (a *BotAPI) SendTopicMessage(ctx context.Context, chatID int64, topic *int64, text string) (int64, error) {
	params := map[string]any{"chat_id": chatID, "text": text}
	if topic != nil {
		params["message_thread_id"] = *topic
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	err := a.call(ctx, "sendMessage", params, &sent)
	return sent.MessageID, err
}

func (a *BotAPI) SendMessageDraft(ctx context.Context, chatID int64, topic *int64, draftID int64, text string) error {
	params := map[string]any{"chat_id": chatID, "draft_id": draftID, "text": text}
	if topic != nil {
		params["message_thread_id"] = *topic
	}
	return a.call(ctx, "sendMessageDraft", params, nil)
}

func (a *BotAPI) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	return a.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}, nil)
}

func (a *BotAPI) SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error {
	return a.call(ctx, "setMessageReaction", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []map[string]string{{"type": "emoji", "emoji": emoji}},
	}, nil)
}

func (a *BotAPI) AnswerCallbackQuery(ctx context.Context, id string) error {
	return a.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

func (a *BotAPI) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return a.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}
