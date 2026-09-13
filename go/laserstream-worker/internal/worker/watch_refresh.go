package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const autodepositWatchChannel = "loyal_yield_autodeposit_watch"

func startWatchRefreshSignals(ctx context.Context, databaseURL string, interval time.Duration, logger *slog.Logger) <-chan struct{} {
	refresh := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				signalWatchRefresh(refresh)
			}
		}
	}()
	go listenForWatchChanges(ctx, databaseURL, logger, refresh)
	return refresh
}

func listenForWatchChanges(ctx context.Context, databaseURL string, logger *slog.Logger, refresh chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, databaseURL)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN "+autodepositWatchChannel)
		}
		if err == nil {
			for ctx.Err() == nil {
				if _, err = conn.WaitForNotification(ctx); err != nil {
					break
				}
				signalWatchRefresh(refresh)
			}
		}
		if conn != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		logger.Warn("Autodeposit watch listener disconnected; periodic refresh remains active", "event", "autodeposit_watch_listener_disconnected", "error", err)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func signalWatchRefresh(refresh chan<- struct{}) {
	select {
	case refresh <- struct{}{}:
	default:
	}
}
