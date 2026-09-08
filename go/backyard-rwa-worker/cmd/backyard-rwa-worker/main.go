package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/backyard-rwa-worker/internal/backyardrwa"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "--initialize-phase3-budget" && len(os.Args) == 2 {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			result, err := backyardrwa.RunPhase3BudgetInitialization(ctx, os.Getenv("NEON_DATABASE_URL"), os.Getenv("BACKYARD_RWA_ROUTE_KEY"))
			if err != nil {
				log.Fatal(err)
			}
			if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
				log.Fatal("budget initialization output unavailable; retry is idempotent")
			}
			return
		}
		if os.Args[1] == "--inspect-phase3-setup-rent" && len(os.Args) == 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result, err := backyardrwa.InspectPhase3SetupRent(ctx, os.Getenv("SOLANA_RPC_URL"))
			if err != nil {
				// The inspector exposes only fixed stage errors, never RPC URLs
				// or provider response bodies that can contain credentials.
				log.Fatal(err)
			}
			fmt.Println(string(result))
			return
		}
		if os.Args[1] != "--inspect-phase3" || len(os.Args) < 3 {
			log.Fatal("usage: backyard-rwa-worker [--inspect-phase3 lane ... | --inspect-phase3-setup-rent | --initialize-phase3-budget]")
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
