package telegram

import "encoding/json"

type Update struct {
	UpdateID           int64               `json:"update_id"`
	Message            *Message            `json:"message"`
	ChannelPost        *Message            `json:"channel_post"`
	InlineQuery        *InlineQuery        `json:"inline_query"`
	CallbackQuery      *CallbackQuery      `json:"callback_query"`
	BusinessConnection *BusinessConnection `json:"business_connection"`
	MyChatMember       *ChatMemberUpdated  `json:"my_chat_member"`
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u User) displayName() string {
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}

func (u User) username() *string {
	if u.Username == "" {
		return nil
	}
	return &u.Username
}

type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

func (c Chat) private() bool { return c.Type == "private" }

func (c Chat) group() bool { return c.Type == "group" || c.Type == "supergroup" }

func (c Chat) community() bool { return c.group() || c.Type == "channel" }

type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type Message struct {
	MessageID         int64           `json:"message_id"`
	From              *User           `json:"from"`
	Chat              Chat            `json:"chat"`
	Text              string          `json:"text"`
	Entities          []Entity        `json:"entities"`
	ForumTopicCreated json.RawMessage `json:"forum_topic_created"`
	MessageThreadID   int64           `json:"message_thread_id"`
	IsTopicMessage    bool            `json:"is_topic_message"`
}

type InlineQuery struct {
	ID    string `json:"id"`
	Query string `json:"query"`
}

type CallbackQuery struct {
	ID      string           `json:"id"`
	From    *User            `json:"from"`
	Data    string           `json:"data"`
	Message *CallbackMessage `json:"message"`
}

type CallbackMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
}

type BusinessConnection struct {
	ID         string          `json:"id"`
	User       User            `json:"user"`
	UserChatID int64           `json:"user_chat_id"`
	Date       int64           `json:"date"`
	IsEnabled  bool            `json:"is_enabled"`
	Rights     json.RawMessage `json:"rights"`
}

type ChatMember struct {
	Status string `json:"status"`
}

type ChatMemberUpdated struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}
