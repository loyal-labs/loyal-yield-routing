package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
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
		if os.Args[1] == "--prepare-pilot-cleanup" && len(os.Args) == 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := backyardrwa.CompilePilotCleanupWire(ctx, os.Getenv("SOLANA_RPC_URL"), os.Stdout); err != nil {
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
		if os.Args[1] == "--selector-evaluate" && (len(os.Args) == 2 || (len(os.Args) == 3 && os.Args[2] == "--execute")) {
			execute := len(os.Args) == 3
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// One-shot selector evaluation. The default is the read-only
			// shadow path plus an explicit canary-request report; --execute
			// performs exactly one locked evaluation under its own short
			// route lease — the same call the continuous live sample loop
			// makes — and the committed entry is durable route state for the
			// next worker start.
			if err := backyardrwa.RunSelectorEvaluate(ctx, os.Stdout, execute); err != nil {
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
		if os.Args[1] == "commit-unwind-intent" {
			request, execute, err := parseUnwindIntentFlags(os.Args[2:])
			if err != nil {
				log.Fatal(err)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The operator exit seam for the bounded entry/exit proof. The
			// default is a dry-run: it validates the exact intent through the
			// installed embedded manifest and prints it without any database
			// write. --execute acquires its own short route lease and goes
			// through the shared guarded commit.
			result, err := backyardrwa.RunUnwindIntentCommit(ctx, os.Getenv("NEON_DATABASE_URL"), request, execute)
			if err != nil {
				// A lease-release failure after a durable commit still prints
				// the committed intent: the commit is NOT unsent.
				if errors.Is(err, backyardrwa.ErrUnwindIntentCommittedReleaseUnconfirmed) {
					if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
						log.Fatal(err)
					}
				}
				log.Fatal(err)
			}
			if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
				log.Fatal("unwind intent output unavailable")
			}
			return
		}
		if os.Args[1] == "settle-manual-restore" {
			request, execute, err := parseSettleManualRestoreFlags(os.Args[2:])
			if err != nil {
				log.Fatal(err)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The scoped operator seam for one already-manual VOLTR_RESTORE_IDLE
			// ReportSlot failure. The default is a dry-run: it validates the exact
			// row identity, recovery marker, and refusal classification, and
			// prints the sanitized result without any database write. --execute
			// additionally requires an empty route under a short route lease and
			// runs the same finalized fee settlement the automatic walk uses.
			result, err := backyardrwa.RunManualRestoreReprocess(ctx, os.Getenv("NEON_DATABASE_URL"), os.Getenv("SOLANA_RPC_URL"), request, execute)
			if err != nil {
				if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
					log.Fatal(err)
				}
				log.Fatal(err)
			}
			if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
				log.Fatal("settlement result output unavailable")
			}
			return
		}
		if os.Args[1] != "--inspect-phase3" || len(os.Args) < 3 {
			log.Fatal("usage: backyard-rwa-worker [--prepare-pilot-cleanup | --inspect-pilot-flat-state | --activate-pilot-budget | --selector-shadow | --selector-evaluate [--execute] | --inspect-phase3 lane ... | --inspect-phase3-setup-rent | --initialize-phase3-budget | clear-hold --route <route key> --reason \"<text>\" | commit-unwind-intent --lane <lane> --reason <reason> --observation-id <id> --max-collateral-raw <n> --max-debt-raw <n> --cost-bound-raw <n> --evidence-id <sha256> [--execute] | settle-manual-restore --operation <operation id> --signature <signature> [--execute]]")
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

// parseSettleManualRestoreFlags reads the manual-restore seam's only two
// inputs. The default is a dry-run; --execute is the explicit write. Every
// authority check stays with the settlement package, so the parser is
// syntactic only.
func parseSettleManualRestoreFlags(args []string) (backyardrwa.ManualRestoreReprocessRequest, bool, error) {
	request := backyardrwa.ManualRestoreReprocessRequest{}
	execute := false
	usage := `usage: backyard-rwa-worker settle-manual-restore --operation <operation id> --signature <signature> [--execute]`
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--execute" {
			execute = true
			continue
		}
		var target *string
		switch arg {
		case "--operation":
			target = &request.OperationID
		case "--signature":
			target = &request.Signature
		default:
			return request, execute, fmt.Errorf("settle-manual-restore: unexpected argument %q\n%s", arg, usage)
		}
		if index+1 >= len(args) {
			return request, execute, fmt.Errorf("settle-manual-restore: %s requires a value\n%s", arg, usage)
		}
		*target = args[index+1]
		index++
	}
	if request.OperationID == "" || request.Signature == "" {
		return request, execute, fmt.Errorf("settle-manual-restore: --operation and --signature are required\n%s", usage)
	}
	return request, execute, nil
}

// parseUnwindIntentFlags reads the exit seam's operator inputs. The default is
// a dry-run; --execute is the explicit write. Value shape and lane authority
// stay with the shared unwind-intent validation, so the parser is syntactic
// only.
func parseUnwindIntentFlags(args []string) (backyardrwa.UnwindIntentCommitRequest, bool, error) {
	request := backyardrwa.UnwindIntentCommitRequest{}
	execute := false
	usage := `usage: backyard-rwa-worker commit-unwind-intent --lane <lane> --reason <economic_rotation|withdrawal_shortfall|hard_ltv_reduction> --observation-id <id> --max-collateral-raw <n> --max-debt-raw <n> --cost-bound-raw <n> --evidence-id <sha256> [--execute]`
	strings := map[string]*string{
		"--lane":           &request.Lane,
		"--reason":         &request.Reason,
		"--observation-id": &request.ObservationID,
		"--evidence-id":    &request.EvidenceID,
	}
	numbers := map[string]*int64{
		"--max-collateral-raw": &request.MaxCollateralRaw,
		"--max-debt-raw":       &request.MaxDebtRaw,
		"--cost-bound-raw":     &request.CostBoundRaw,
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--execute" {
			execute = true
			continue
		}
		if target, ok := numbers[arg]; ok {
			if index+1 >= len(args) {
				return request, execute, fmt.Errorf("commit-unwind-intent: %s requires a value\n%s", arg, usage)
			}
			value, err := strconv.ParseInt(args[index+1], 10, 64)
			if err != nil {
				return request, execute, fmt.Errorf("commit-unwind-intent: %s requires an integer\n%s", arg, usage)
			}
			*target = value
			index++
			continue
		}
		target, ok := strings[arg]
		if !ok {
			return request, execute, fmt.Errorf("commit-unwind-intent: unexpected argument %q\n%s", arg, usage)
		}
		if index+1 >= len(args) {
			return request, execute, fmt.Errorf("commit-unwind-intent: %s requires a value\n%s", arg, usage)
		}
		*target = args[index+1]
		index++
	}
	if request.Lane == "" || request.Reason == "" || request.ObservationID == "" || request.EvidenceID == "" {
		return request, execute, fmt.Errorf("commit-unwind-intent: --lane, --reason, --observation-id and --evidence-id are required\n%s", usage)
	}
	return request, execute, nil
}
