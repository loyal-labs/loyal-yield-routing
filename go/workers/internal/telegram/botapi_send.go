package telegram

import "context"

type Button struct {
	Text         string    `json:"text"`
	URL          string    `json:"url,omitempty"`
	CallbackData string    `json:"callback_data,omitempty"`
	CopyText     *CopyText `json:"copy_text,omitempty"`
}

type CopyText struct {
	Text string `json:"text"`
}

type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

type SendOptions struct {
	ParseMode string
	ThreadID  int64
	Markup    *Keyboard
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

func (a *BotAPI) AnswerCallback(ctx context.Context, id, text string, alert bool) error {
	params := map[string]any{"callback_query_id": id, "text": text}
	if alert {
		params["show_alert"] = true
	}
	return a.call(ctx, "answerCallbackQuery", params, nil)
}
