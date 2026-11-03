package bridge

import (
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
)

func baseUSDCQuote(t *testing.T) (QuoteRequest, map[string]any) {
	raw, err := os.ReadFile("testdata/relay-quote-base-usdc.json")
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	amt, _ := new(big.Int).SetString("5000000", 10)
	return QuoteRequest{Chain: Base, Asset: USDC, AmountRaw: amt, From: "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7", Recipient: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"}, q
}

func TestRefusesApproveCarryingValue(t *testing.T) {
	req, q := baseUSDCQuote(t)
	step := q["steps"].([]any)[0].(map[string]any)
	step["items"].([]any)[0].(map[string]any)["data"].(map[string]any)["value"] = "500000000000000000"
	b, _ := json.Marshal(q)
	if _, err := CheckQuote(req, b); err == nil {
		t.Fatal("approve sending 0.5 ETH accepted")
	}
}

func TestRefusesTokenDepositWithoutApprove(t *testing.T) {
	req, q := baseUSDCQuote(t)
	q["steps"] = q["steps"].([]any)[1:]
	b, _ := json.Marshal(q)
	if _, err := CheckQuote(req, b); err == nil {
		t.Fatal("ERC-20 deposit with no approve accepted")
	}
}

func TestRefusesDepositForAnotherDepositor(t *testing.T) {
	req, q := baseUSDCQuote(t)
	step := q["steps"].([]any)[1].(map[string]any)
	data := step["items"].([]any)[0].(map[string]any)["data"].(map[string]any)
	data["data"] = strings.Replace(data["data"].(string), "98f566dc02faf8fd0f3ebb6aab48c40164207fc7", "1111111111111111111111111111111111111111", 1)
	b, _ := json.Marshal(q)
	_, err := CheckQuote(req, b)
	if err == nil || !strings.Contains(err.Error(), "deposit data") {
		t.Fatalf("deposit naming another depositor: err = %v", err)
	}
}
