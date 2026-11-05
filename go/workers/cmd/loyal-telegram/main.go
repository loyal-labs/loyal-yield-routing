package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/r2"
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

func r2Config() (*r2.Config, error) {
	id, err := engine.OptionalCredential("R2_ACCESS_KEY_ID")
	if err != nil {
		return nil, err
	}
	secret, err := engine.OptionalCredential("R2_SECRET_ACCESS_KEY")
	if err != nil {
		return nil, err
	}
	bucket, endpoint, cdnBase := os.Getenv("LOYAL_TELEGRAM_R2_BUCKET"), os.Getenv("LOYAL_TELEGRAM_R2_ENDPOINT"), os.Getenv("LOYAL_TELEGRAM_CDN_BASE_URL")
	if id == "" || secret == "" || bucket == "" || endpoint == "" || cdnBase == "" {
		return nil, nil
	}
	cfg, err := r2.New(id, secret, bucket, endpoint, cdnBase, os.Getenv("LOYAL_TELEGRAM_R2_PREFIX"))
	if err != nil {
		return nil, fmt.Errorf("LOYAL_TELEGRAM_R2_*: %w", err)
	}
	return cfg, nil
}

func publicBase() (*url.URL, error) {
	raw := os.Getenv("LOYAL_TELEGRAM_PUBLIC_URL")
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("LOYAL_TELEGRAM_PUBLIC_URL: want an absolute URL")
	}
	return parsed, nil
}

func run(ctx context.Context) error {
	address := envOr("LOYAL_TELEGRAM_ADDRESS", "127.0.0.1:10083")
	apiBase := envOr("LOYAL_TELEGRAM_BOT_API", "https://api.telegram.org")
	jupiterSearch := os.Getenv("LOYAL_TELEGRAM_JUPITER_SEARCH")
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
	uploadToken, err := engine.OptionalCredential("LIBRARY_UPLOAD_TOKEN")
	if err != nil {
		return err
	}
	storage, err := r2Config()
	if err != nil {
		return err
	}
	base, err := publicBase()
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
		DB: pool, API: api, BotUsername: me.Username, WebhookSecret: secret,
		BotID: me.ID, R2: storage, LibraryUploadToken: uploadToken, PublicBase: base,
		JupiterSearch: jupiterSearch,
	})

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
