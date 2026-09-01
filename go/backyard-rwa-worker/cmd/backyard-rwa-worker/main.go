package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/backyard-rwa-worker/internal/backyardrwa"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := backyardrwa.Run(ctx, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
