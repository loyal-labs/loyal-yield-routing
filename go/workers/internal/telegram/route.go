package telegram

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf16"
)

type Route string

const (
	RouteNone                         Route = ""
	RouteCommandStart                 Route = "command:start"
	RouteCommandSettings              Route = "command:settings"
	RouteCommandCA                    Route = "command:ca"
	RouteCommandStats                 Route = "command:stats"
	RouteCommandActivateCommunity     Route = "command:activate_community"
	RouteCommandDeactivateCommunity   Route = "command:deactivate_community"
	RouteCommandHide                  Route = "command:hide"
	RouteCommandUnhide                Route = "command:unhide"
	RouteCommandSummary               Route = "command:summary"
	RouteCommandNotifications         Route = "command:notifications"
	RouteInlineSummary                Route = "inline:summary"
	RouteInline                       Route = "inline"
	RouteCallbackNotificationSettings Route = "callback:notification_settings"
	RouteCallbackUserSettings         Route = "callback:user_settings"
	RouteCallbackSummaryVote          Route = "callback:summary_vote"
	RouteCallbackOther                Route = "callback:other"
	RouteBusinessConnection           Route = "business_connection"
	RouteMyChatMember                 Route = "my_chat_member"
	RouteDirectTopicCreated           Route = "direct:topic_created"
	RouteDirectText                   Route = "direct:text"
	RouteCommunityText                Route = "community:text"
)

type Handler func(ctx context.Context, update *Update) (redeliver error)

var commandRoutes = map[string]Route{
	"start":                RouteCommandStart,
	"settings":             RouteCommandSettings,
	"ca":                   RouteCommandCA,
	"stats":                RouteCommandStats,
	"activate_community":   RouteCommandActivateCommunity,
	"deactivate_community": RouteCommandDeactivateCommunity,
	"hide":                 RouteCommandHide,
	"unhide":               RouteCommandUnhide,
	"summary":              RouteCommandSummary,
	"notifications":        RouteCommandNotifications,
}

const jsSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

const uuidPattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

var (
	summaryInlineQuery         = regexp.MustCompile(`^summary:` + jsSpace + `*(-?[0-9]+)` + jsSpace + `*$`)
	notificationSettingsButton = regexp.MustCompile(`^notif:settings:(` + uuidPattern + `):(time|msg|master):(off|24|48|500|1000|on)$`)
	userSettingsButton         = regexp.MustCompile(`^usr:settings:(` + uuidPattern + `):(notif):(on|off)$`)
	summaryVoteButton          = regexp.MustCompile(`^sv:(u|d|s):(` + uuidPattern + `):(-?[0-9]+)$`)
)

func route(update *Update, botUsername string) Route {
	if message := commandMessage(update); message != nil {
		if r, ok := commandRoute(message, botUsername); ok {
			return r
		}
	}
	switch {
	case update.InlineQuery != nil:
		if summaryInlineQuery.MatchString(update.InlineQuery.Query) {
			return RouteInlineSummary
		}
		return RouteInline
	case update.CallbackQuery != nil:
		data := update.CallbackQuery.Data
		switch {
		case notificationSettingsButton.MatchString(data):
			return RouteCallbackNotificationSettings
		case userSettingsButton.MatchString(data):
			return RouteCallbackUserSettings
		case summaryVoteButton.MatchString(data):
			return RouteCallbackSummaryVote
		case data != "":
			return RouteCallbackOther
		}
		return RouteNone
	case update.BusinessConnection != nil:
		return RouteBusinessConnection
	case update.MyChatMember != nil:
		return RouteMyChatMember
	case update.Message != nil:
		message := update.Message
		if message.ForumTopicCreated != nil {
			if message.Chat.private() {
				return RouteDirectTopicCreated
			}
			return RouteNone
		}
		if message.Text == "" {
			return RouteNone
		}
		if !message.Chat.private() {
			return RouteCommunityText
		}
		if strings.HasPrefix(message.Text, "/") {
			return RouteNone
		}
		return RouteDirectText
	}
	return RouteNone
}

func commandMessage(update *Update) *Message {
	if update.Message != nil {
		return update.Message
	}
	return update.ChannelPost
}

func commandRoute(message *Message, botUsername string) (Route, bool) {
	text := utf16.Encode([]rune(message.Text))
	for _, entity := range message.Entities {
		if entity.Type != "bot_command" || entity.Offset != 0 || entity.Length < 1 || entity.Length > len(text) {
			continue
		}
		name := string(utf16.Decode(text[1:entity.Length]))
		if target, rest, found := strings.Cut(name, "@"); found {
			if !strings.EqualFold(rest, botUsername) {
				continue
			}
			name = target
		}
		r, ok := commandRoutes[name]
		if !ok || (r == RouteCommandSettings && !message.Chat.private()) {
			continue
		}
		return r, true
	}
	return RouteNone, false
}
