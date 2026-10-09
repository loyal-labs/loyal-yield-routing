package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Optional read-only preflight. Stale reserves may be refreshed in an unsigned,
// price-only simulation. No signer, journal mutation or submission is used.
// A passing result proves only price/fee observation, not lifecycle execution.
func TestPhase3ReadOnlyPricePreflight(t *testing.T) {
	if os.Getenv("PHASE3_READONLY_PRICE_PREFLIGHT") != "1" {
		t.Skip("explicit read-only preflight gate required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rpc, err := chain.New(os.Getenv("SOLANA_RPC_URL"), 15*time.Second)
	if err != nil {
		t.Fatal("RPC configuration unavailable")
	}
	slot, err := confirmedSlot(ctx, rpc)
	if err != nil {
		t.Fatal("confirmed slot unavailable")
	}
	for _, lane := range []string{RouteID, SelectedRouteID} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		for _, mint := range []string{route.Kamino.CollateralMint, bridgeUSDC} {
			price, err := ObserveBudgetTokenPrice(ctx, rpc, lane, ExecutableDebit{Source: route.CollateralCustody, Mint: mint, TokenProgram: classicTokenProgram, Raw: 1}, slot)
			if err != nil {
				var hold *BudgetHold
				if errors.As(err, &hold) {
					encoded, _ := json.Marshal(hold)
					t.Fatalf("%s %s: HOLD %s", lane, mint, encoded)
				}
				t.Fatalf("%s %s price observation failed (details withheld to protect RPC credentials)", lane, mint)
			}
			encoded, err := json.Marshal(price)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("READ_ONLY_PRICE %s", encoded)
		}
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		t.Fatal("blockhash unavailable")
	}
	request := bridgeTestRequest(ReportNAV, 0)
	request.RecentBlockhash = blockhash.Blockhash
	request.LastValidBlockHeight = blockhash.LastValidBlockHeight
	request.Report.ObservedSlot = uint64(slot)
	request.Report.Sequence = uint64(slot)
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := observeMessageFee(ctx, rpc, message, slot)
	if err != nil {
		t.Fatal("exact-message fee unavailable")
	}
	encoded, err := json.Marshal(fee)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("READ_ONLY_MESSAGE_FEE %s", encoded)
	sol, err := ObserveNativeSOLBudgetPrice(ctx, rpc, fee.Slot)
	if err != nil {
		var hold *BudgetHold
		if errors.As(err, &hold) {
			t.Fatalf("native SOL valuation HOLD: %s", hold.Reason)
		}
		t.Fatal("native SOL valuation unavailable")
	}
	cost, err := ValueTransactionCost(message, ExecutableDebit{}, fee, 0, BudgetPrice{}, sol, sol.ObservedSlot)
	if err != nil {
		t.Fatal("native SOL fee could not be normalized")
	}
	encoded, err = json.Marshal(struct {
		Price BudgetPrice           `json:"price"`
		Cost  ValuedTransactionCost `json:"cost"`
	}{sol, cost})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("READ_ONLY_NATIVE_FEE %s", encoded)
}
