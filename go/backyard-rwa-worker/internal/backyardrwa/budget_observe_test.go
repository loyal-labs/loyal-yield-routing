package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
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
	rpc, err := NewRPCClient(os.Getenv("SOLANA_RPC_URL"))
	if err != nil {
		t.Fatal("RPC configuration unavailable")
	}
	slot, err := rpc.ConfirmedSlot(ctx)
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
	blockhash, err := rpc.LatestBlockhash(ctx)
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
	fee, err := rpc.ObserveMessageFee(ctx, message, slot)
	if err != nil {
		t.Fatal("exact-message fee unavailable")
	}
	encoded, err := json.Marshal(fee)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("READ_ONLY_MESSAGE_FEE %s", encoded)
}
