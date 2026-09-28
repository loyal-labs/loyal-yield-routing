package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPhase3ReadOnlySOLReferenceDiscovery(t *testing.T) {
	if os.Getenv("PHASE3_READONLY_PRICE_PREFLIGHT") != "1" {
		t.Skip("explicit read-only preflight gate required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rpc, err := NewRPCClient(os.Getenv("SOLANA_RPC_URL"))
	if err != nil {
		t.Fatal("RPC configuration unavailable")
	}
	const market = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	const mint = "So11111111111111111111111111111111111111112"
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []struct {
			Pubkey  string `json:"pubkey"`
			Account struct {
				Owner string   `json:"owner"`
				Data  []string `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}
	params := []any{kaminoProgram, map[string]any{"encoding": "base64", "commitment": "confirmed", "withContext": true, "dataSlice": map[string]int{"offset": 0, "length": 224}, "filters": []any{map[string]int{"dataSize": kaminoReserveLength}, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": market}}, map[string]any{"memcmp": map[string]any{"offset": 128, "bytes": mint}}}}}
	if err = rpc.call(ctx, "getProgramAccounts", params, &result); err != nil {
		t.Fatal("SOL reference discovery RPC failed")
	}
	if result.Context.Slot <= 0 || len(result.Value) != 1 {
		t.Fatalf("SOL reference discovery not unique: count=%d", len(result.Value))
	}
	row := result.Value[0]
	if len(row.Account.Data) != 2 || row.Account.Owner != kaminoProgram {
		t.Fatal("SOL reference owner/encoding mismatch")
	}
	data, err := base64.StdEncoding.DecodeString(row.Account.Data[0])
	if err != nil || len(data) != 224 || !sameKey(data[32:64], market) || !sameKey(data[128:160], mint) {
		t.Fatal("SOL reference identity mismatch")
	}
	t.Logf("READ_ONLY_SOL_REFERENCE reserve=%s market=%s mint=%s slot=%d", row.Pubkey, market, mint, result.Context.Slot)
}

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
