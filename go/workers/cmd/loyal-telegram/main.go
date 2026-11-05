package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/telegram"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

var webhookSecretFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func run(ctx context.Context) error {
	address := envOr("LOYAL_TELEGRAM_ADDRESS", "127.0.0.1:10083")
	apiBase := envOr("LOYAL_TELEGRAM_BOT_API", "https://api.telegram.org")
	redpillBase := envOr("LOYAL_TELEGRAM_REDPILL_API", "https://api.redpill.ai/v1")
	dsn, err := engine.Credential("TELEGRAM_APP_DATABASE_URL")
	if err != nil {
		return err
	}
	token, err := engine.Credential("TELEGRAM_BOT_TOKEN")
	if err != nil {
		return err
	}
	secret, err := engine.Credential("TELEGRAM_WEBHOOK_SECRET")
	if err != nil {
		return err
	}
	if !webhookSecretFormat.MatchString(secret) {
		return errors.New("TELEGRAM_WEBHOOK_SECRET: want 1-256 characters of A-Z, a-z, 0-9, _ and -")
	}
	redpillKey, err := engine.Credential("REDPILL_AI_API_KEY")
	if err != nil {
		return err
	}
	encryptionKey, err := engine.Credential("MESSAGE_ENCRYPTION_KEY")
	if err != nil {
		return err
	}
	messageCipher, err := telegram.NewMessageCipher(encryptionKey)
	if err != nil {
		return err
	}
	pool, err := db.OpenLazy(ctx, dsn, 8)
	if err != nil {
		return fmt.Errorf("TELEGRAM_APP_DATABASE_URL: %w", err)
	}
	defer pool.Close()

	api := telegram.NewBotAPI(apiBase, token)
	meCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	me, err := api.GetMe(meCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	bot := telegram.New(telegram.Config{
		DB:            pool,
		API:           api,
		Redpill:       telegram.NewRedpill(redpillBase, redpillKey),
		Cipher:        messageCipher,
		BotUsername:   me.Username,
		WebhookSecret: secret,
	})
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		bot.DrainDirectChats(drainCtx)
	}()

	server := &http.Server{
		Addr:              address,
		Handler:           bot.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	cleanupCtx, stopCleanup := context.WithCancel(ctx)
	var cleanup sync.WaitGroup
	cleanup.Go(func() { bot.RunHelperCleanup(cleanupCtx) })
	defer func() {
		stopCleanup()
		cleanup.Wait()
	}()

	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	slog.Info("loyal-telegram listening", "address", address, "bot", me.Username, "release", webserve.Release())
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-telegram")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("loyal-telegram stopped", "error", err)
		os.Exit(1)
	}
}
