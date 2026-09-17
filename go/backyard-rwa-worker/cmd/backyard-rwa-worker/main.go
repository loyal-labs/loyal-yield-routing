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
		if os.Args[1] == "--inspect-pilot-flat-state" && len(os.Args) == 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := backyardrwa.InspectPilotBudgetFlatState(ctx, os.Getenv("SOLANA_RPC_URL"), os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		}
		if os.Args[1] == "--selector-shadow" && len(os.Args) == 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := backyardrwa.RunSelectorShadow(ctx, os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		}
		if os.Args[1] == "--activate-pilot-budget" && len(os.Args) == 2 {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			result, err := backyardrwa.RunPilotBudgetActivation(ctx, os.Getenv("NEON_DATABASE_URL"), os.Getenv("SOLANA_RPC_URL"), os.Getenv("BACKYARD_RWA_ROUTE_KEY"))
			if err != nil {
				log.Fatal(err)
			}
			if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
				log.Fatal("pilot activation output unavailable; retry is idempotent")
			}
			return
		}
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
		if os.Args[1] == "clear-hold" {
			reason, routeKey, err := parseClearHoldFlags(os.Args[2:])
			if err != nil {
				log.Fatal(err)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The only operator path that lifts a durable manual recovery stop.
			result, err := backyardrwa.ClearManualRecoveryHold(ctx, os.Getenv("NEON_DATABASE_URL"), routeKey, reason)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(result)
			return
		}
		if os.Args[1] != "--inspect-phase3" || len(os.Args) < 3 {
			log.Fatal("usage: backyard-rwa-worker [--inspect-pilot-flat-state | --activate-pilot-budget | --selector-shadow | --inspect-phase3 lane ... | --inspect-phase3-setup-rent | --initialize-phase3-budget | clear-hold --route <route key> --reason \"<text>\"]")
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

	// Strategy-two cutover gate: never run while a legacy seed 62-65 policy
	// still exists at finalized commitment (see the strategy-two runbook).
	if _, err := backyardrwa.AssertLegacyPoliciesRetired(ctx, os.Getenv("SOLANA_RPC_URL")); err != nil {
		log.Fatal(err)
	}

	if err := backyardrwa.Run(ctx, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// parseClearHoldFlags reads the operator command's only two flags. The reason
// is mandatory and must be non-empty: it is what the HOLD_CLEARED journal row
// records.
func parseClearHoldFlags(args []string) (string, string, error) {
	reason, routeKey := "", ""
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--route":
			if index+1 >= len(args) {
				return "", "", fmt.Errorf("clear-hold: --route requires a route key")
			}
			routeKey = args[index+1]
			index++
		case "--reason":
			if index+1 >= len(args) {
				return "", "", fmt.Errorf("clear-hold: --reason requires text")
			}
			reason = args[index+1]
			index++
		default:
			return "", "", fmt.Errorf("clear-hold: unexpected argument %q", args[index])
		}
	}
	if routeKey == "" || reason == "" {
		return "", "", fmt.Errorf(`usage: backyard-rwa-worker clear-hold --route <route key> --reason "<text>"`)
	}
	return reason, routeKey, nil
}
