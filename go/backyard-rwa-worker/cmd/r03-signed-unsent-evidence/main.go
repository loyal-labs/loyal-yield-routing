// Command r03-signed-unsent-evidence is read/simulate-only.  It consumes a
// plan emitted by the canonical Go lifecycle builders and calls exactly one
// Helius simulateBundle request.  There is intentionally no execute flag and
// no sendTransaction call in this command.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/backyard-rwa-worker/internal/backyardrwa"
)

func main() {
	planPath := flag.String("plan", "", "canonical signed-unsent R03 plan JSON")
	outPath := flag.String("out", "", "new evidence output path")
	rpcURL := flag.String("rpc", os.Getenv("SOLANA_RPC_URL"), "mainnet RPC URL")
	flag.Parse()
	if *planPath == "" || *outPath == "" || *rpcURL == "" {
		fatal("--plan, --out, and --rpc (or SOLANA_RPC_URL) are required")
	}
	planBytes, err := os.ReadFile(*planPath)
	if err != nil {
		fatal("read plan: %v", err)
	}
	var plan backyardrwa.R03LifecyclePlan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		fatal("decode plan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := backyardrwa.SimulateR03Lifecycle(ctx, *rpcURL, plan)
	if err != nil {
		fatal("R03 simulation blocked: %v", err)
	}
	if err := backyardrwa.WriteR03Evidence(*outPath, plan, result); err != nil {
		fatal("write evidence: %v", err)
	}
	fmt.Printf("{\"verdict\":\"PASS\",\"broadcast\":false,\"signedUnsent\":true,\"output\":%q}\n", *outPath)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
