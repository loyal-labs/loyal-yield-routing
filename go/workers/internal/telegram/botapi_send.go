package telegram

import "context"

type Button struct {
	Text              string    `json:"text"`
	URL               string    `json:"url,omitempty"`
	CallbackData      string    `json:"callback_data,omitempty"`
	CopyText          *CopyText `json:"copy_text,omitempty"`
	IconCustomEmojiID string    `json:"icon_custom_emoji_id,omitempty"`
	Style             string    `json:"style,omitempty"`
}

type CopyText struct {
	Text string `json:"text"`
}

type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

type SendOptions struct {
	ParseMode        string
	ThreadID         int64
	Markup           *Keyboard
	PreferLargeMedia bool
	ReplyTo          int64
}

type linkPreview struct {
	PreferLargeMedia bool `json:"prefer_large_media"`
}

func (o SendOptions) apply(params map[string]any) map[string]any {
	if o.ParseMode != "" {
		params["parse_mode"] = o.ParseMode
	}
	if o.ThreadID != 0 {
		params["message_thread_id"] = o.ThreadID
	}
	if o.Markup != nil {
		params["reply_markup"] = o.Markup
	}
	if o.PreferLargeMedia {
		params["link_preview_options"] = linkPreview{PreferLargeMedia: true}
	}
	if o.ReplyTo != 0 {
		params["reply_parameters"] = map[string]int64{"message_id": o.ReplyTo}
	}
	return params
}

func (a *BotAPI) Send(ctx context.Context, chatID int64, text string, opts SendOptions) (int64, error) {
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	err := a.call(ctx, "sendMessage", opts.apply(map[string]any{"chat_id": chatID, "text": text}), &sent)
	return sent.MessageID, err
}

func (a *BotAPI) EditMessageText(ctx context.Context, chatID, messageID int64, text string, opts SendOptions) error {
	return a.call(ctx, "editMessageText", opts.apply(map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}), nil)
}

func (a *BotAPI) AnswerCallback(ctx context.Context, id, text string, alert bool, cacheSeconds int) error {
	params := map[string]any{"callback_query_id": id, "text": text}
	if alert {
		params["show_alert"] = true
	}
	if cacheSeconds > 0 {
		params["cache_time"] = cacheSeconds
	}
	return a.call(ctx, "answerCallbackQuery", params, nil)
}

func (a *BotAPI) EditMessageReplyMarkup(ctx context.Context, chatID, messageID int64, markup *Keyboard) error {
	return a.call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chatID, "message_id": messageID, "reply_markup": markup}, nil)
}

func (a *BotAPI) ForwardMessage(ctx context.Context, chatID, fromChatID, messageID int64) error {
	return a.call(ctx, "forwardMessage", map[string]any{"chat_id": chatID, "from_chat_id": fromChatID, "message_id": messageID}, nil)
}

func (a *BotAPI) AnswerInlineQuery(ctx context.Context, id string, results any, personal bool) error {
	params := map[string]any{
		"inline_query_id": id,
		"results":         results,
		"button":          map[string]any{"text": "Loyal Wallet", "web_app": map[string]string{"url": miniAppLink}},
	}
	if personal {
		params["is_personal"] = true
	}
	return a.call(ctx, "answerInlineQuery", params, nil)
}
