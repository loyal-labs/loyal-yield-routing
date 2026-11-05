package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	secretTokenHeader = "X-Telegram-Bot-Api-Secret-Token"
	updateDeadline    = 30 * time.Second
	sideEffectTimeout = 10 * time.Second
)

type Config struct {
	DB            *pgxpool.Pool
	API           *BotAPI
	BotUsername   string
	WebhookSecret string
}

type Bot struct {
	db       *pgxpool.Pool
	api      *BotAPI
	username string
	secret   []byte
	routes   map[Route]Handler
}

func New(cfg Config) *Bot {
	b := &Bot{db: cfg.DB, api: cfg.API, username: cfg.BotUsername, secret: []byte(cfg.WebhookSecret)}
	b.routes = map[Route]Handler{
		RouteCommunityText:      b.communityText,
		RouteCallbackOther:      b.answerCallback,
		RouteBusinessConnection: b.businessConnection,
		RouteMyChatMember:       b.myChatMember,
	}
	return b
}

func (b *Bot) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /telegram/webhook", b.webhook)
	mux.HandleFunc("POST /api/telegram/webhook", b.webhook)
	return mux
}

func (b *Bot) webhook(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(secretTokenHeader)), b.secret) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var update Update
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&update); err != nil {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	matched := route(&update, b.username)
	handler := b.routes[matched]
	if handler == nil {
		if matched != RouteNone {
			slog.Warn("telegram update route has no Go handler", "update_id", update.UpdateID, "route", matched)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateDeadline)
	defer cancel()
	if err := handler(ctx, &update); err != nil {
		slog.Error("telegram update failed, asking Telegram to redeliver", "update_id", update.UpdateID, "route", matched, "error", err)
		http.Error(w, "retry", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) answerCallback(ctx context.Context, update *Update) error {
	if err := b.api.AnswerCallbackQuery(ctx, update.CallbackQuery.ID); err != nil {
		slog.Warn("telegram callback answer failed", "update_id", update.UpdateID, "error", err)
	}
	return nil
}
