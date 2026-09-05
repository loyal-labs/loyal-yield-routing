package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/backyard-rwa-worker/internal/backyardrwa"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] != "--inspect-phase3" || len(os.Args) < 3 {
			log.Fatal("usage: backyard-rwa-worker [--inspect-phase3 lane ...]")
		}
		result, err := backyardrwa.InspectPhase3Runtime(os.Args[2:])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(result))
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := backyardrwa.Run(ctx, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
