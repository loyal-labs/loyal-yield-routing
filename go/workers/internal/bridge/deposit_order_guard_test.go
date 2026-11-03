package bridge

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

const depositOrderWallet = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"

var depositOrderNow = time.Date(2026, 11, 3, 4, 5, 0, 0, time.UTC)

func depositOrder(t *testing.T) map[string]any {
	deadline := depositOrderNow.Add(180 * 24 * time.Hour).Unix()
	raw := fmt.Sprintf(`{
  "steps": [{"id": "deposit", "kind": "transaction", "requestId": "0x17", "depositAddress": "TE6yWzepgFMYrtUdhHFNUc6J7B1amMktQz"}],
  "fees": {"relayer": {"amountUsd": "1.95"}},
  "details": {"recipient": %[1]q},
  "protocol": {"v2": {"orderData": {
    "inputs": [{
      "payment": {"chainId": "tron", "currency": %[2]q, "amount": "100000000"},
      "refunds": [{"chainId": "solana", "recipient": %[1]q, "currency": %[3]q, "deadline": %[4]d, "extraData": "0x"}]
    }],
    "output": {"chainId": "solana", "payments": [{"recipient": %[1]q, "currency": %[3]q, "minimumAmount": "97422136"}], "calls": [], "deadline": %[4]d, "extraData": "0x"},
    "fees": []
  }}}
}`, depositOrderWallet, Tron.Token, SolanaUSDCMint, deadline)
	var q map[string]any
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestRefusesDepositOrderThatCannotRefundOrNeverExpires(t *testing.T) {
	orderData := func(q map[string]any) map[string]any {
		return q["protocol"].(map[string]any)["v2"].(map[string]any)["orderData"].(map[string]any)
	}
	input := func(q map[string]any) map[string]any { return orderData(q)["inputs"].([]any)[0].(map[string]any) }
	output := func(q map[string]any) map[string]any { return orderData(q)["output"].(map[string]any) }
	payment := func(q map[string]any) map[string]any { return output(q)["payments"].([]any)[0].(map[string]any) }
	refund := func(q map[string]any) map[string]any { return input(q)["refunds"].([]any)[0].(map[string]any) }

	b, _ := json.Marshal(depositOrder(t))
	if _, err := CheckDepositOrder(Tron, depositOrderWallet, b, depositOrderNow); err != nil {
		t.Fatalf("order paying and refunding the wallet refused: %v", err)
	}
	for name, tamper := range map[string]func(q map[string]any){
		"no refund leg":   func(q map[string]any) { input(q)["refunds"] = []any{} },
		"refunds missing": func(q map[string]any) { delete(input(q), "refunds") },
		"refund in another token": func(q map[string]any) {
			input(q)["refunds"].([]any)[0].(map[string]any)["currency"] = "So11111111111111111111111111111111111111112"
		},
		"refund leg with calldata":              func(q map[string]any) { input(q)["refunds"].([]any)[0].(map[string]any)["extraData"] = "0xdeadbeef" },
		"deadline in milliseconds":              func(q map[string]any) { output(q)["deadline"] = depositOrderNow.Add(180 * 24 * time.Hour).UnixMilli() },
		"deadline in a year":                    func(q map[string]any) { output(q)["deadline"] = depositOrderNow.AddDate(1, 0, 0).Unix() },
		"output with calldata":                  func(q map[string]any) { output(q)["extraData"] = "0xdeadbeef" },
		"output calldata missing":               func(q map[string]any) { delete(output(q), "extraData") },
		"output minimum zero":                   func(q map[string]any) { payment(q)["minimumAmount"] = "0" },
		"output minimum at relay's 2% slippage": func(q map[string]any) { payment(q)["minimumAmount"] = "95953460" },
		"refund leg without deadline":           func(q map[string]any) { delete(refund(q), "deadline") },
		"refund leg ending before the order": func(q map[string]any) {
			refund(q)["deadline"] = depositOrderNow.Add(179 * 24 * time.Hour).Unix()
		},
	} {
		q := depositOrder(t)
		tamper(q)
		b, _ := json.Marshal(q)
		if _, err := CheckDepositOrder(Tron, depositOrderWallet, b, depositOrderNow); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDepositOrderShowsOnlyAFeeWithinTheMinimum(t *testing.T) {
	for fee, shown := range map[string]string{"1.95": "1.95", "100": "100", "100.01": "", "-1": "", "1e3": "", "NaN": "", "": ""} {
		q := depositOrder(t)
		q["fees"] = map[string]any{"relayer": map[string]any{"amountUsd": fee}}
		b, _ := json.Marshal(q)
		got, err := CheckDepositOrder(Tron, depositOrderWallet, b, depositOrderNow)
		if err != nil {
			t.Fatalf("fee %q refused the order: %v", fee, err)
		}
		if got.FeeUSD != shown {
			t.Errorf("fee %q shown as %q, want %q", fee, got.FeeUSD, shown)
		}
	}
}
