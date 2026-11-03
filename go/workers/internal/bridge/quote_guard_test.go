package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func recordedQuote(t *testing.T, file string) map[string]any {
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func baseUSDCQuote(t *testing.T) (QuoteRequest, map[string]any) {
	amt, _ := new(big.Int).SetString("5000000", 10)
	return QuoteRequest{Chain: Base, Asset: USDC, AmountRaw: amt, From: "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7", Recipient: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"}, recordedQuote(t, "relay-quote-base-usdc.json")
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

func TestApprovalProxyDepositBindsDepositorAndAmount(t *testing.T) {
	amt, _ := new(big.Int).SetString("25000000000000000000", 10)
	req := QuoteRequest{Chain: Ethereum, Asset: USDS, AmountRaw: amt, From: "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7", Recipient: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"}
	tampered := func(old, new string) []byte {
		q := recordedQuote(t, "relay-quote-ethereum-usds.json")
		step := q["steps"].([]any)[1].(map[string]any)
		data := step["items"].([]any)[0].(map[string]any)["data"].(map[string]any)
		data["data"] = strings.Replace(data["data"].(string), old, new, 1)
		b, _ := json.Marshal(q)
		return b
	}
	recorded, _ := json.Marshal(recordedQuote(t, "relay-quote-ethereum-usds.json"))
	if _, err := CheckQuote(req, recorded); err != nil {
		t.Fatalf("recorded ApprovalProxy quote refused: %v", err)
	}
	amountWord := fmt.Sprintf("%064x", amt)
	for name, quote := range map[string][]byte{
		"depositor": tampered("98f566dc02faf8fd0f3ebb6aab48c40164207fc7", "1111111111111111111111111111111111111111"),
		"amount":    tampered(amountWord, fmt.Sprintf("%064x", new(big.Int).Rsh(amt, 1))),
	} {
		if _, err := CheckQuote(req, quote); err == nil || !strings.Contains(err.Error(), "deposit data") {
			t.Errorf("ApprovalProxy deposit with the %s tampered: err = %v", name, err)
		}
	}
}

func TestRelayQuoteRefusesSwapThatDepositsLess(t *testing.T) {
	const user = "98f566dc02faf8fd0f3ebb6aab48c40164207fc7"
	const usdsAddress = "0xdc035d45d973e3ec169d2276ddab16f1e407384f"
	usds, _ := new(big.Int).SetString("25000000000000000000", 10)
	recorded, _ := json.Marshal(recordedQuote(t, "relay-quote-ethereum-usds.json"))
	tampered := []byte(strings.ReplaceAll(string(recorded), "0000000000001ff3684f28c67538d4d072c22734", "000000000000000000000000000000000000dead"))
	floorless := recordedQuote(t, "relay-quote-ethereum-usds.json")
	floorless["details"].(map[string]any)["currencyOut"].(map[string]any)["minimumAmount"] = "1"
	minimumOne, _ := json.Marshal(floorless)
	_, baseQuote := baseUSDCQuote(t)
	direct, _ := json.Marshal(baseQuote)
	for _, c := range []struct {
		name       string
		req        QuoteRequest
		quote      []byte
		credit     string
		accept     bool
		rpcBatches int
	}{
		{"swap crediting the user", QuoteRequest{Chain: Ethereum, Asset: USDS, AmountRaw: usds}, recorded, "24969055", true, 2},
		{"swap to another swapper", QuoteRequest{Chain: Ethereum, Asset: USDS, AmountRaw: usds}, tampered, "", false, 2},
		{"minimum of 1 and a swap forwarding 1", QuoteRequest{Chain: Ethereum, Asset: USDS, AmountRaw: usds}, minimumOne, "1", false, 0},
		{"direct deposit", QuoteRequest{Chain: Base, Asset: USDC, AmountRaw: big.NewInt(5000000)}, direct, "", true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			batches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/quote/v2" {
					w.Write(c.quote)
					return
				}
				batches++
				var calls []struct {
					ID     int               `json:"id"`
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				json.NewDecoder(r.Body).Decode(&calls)
				balanceOf := "0x70a08231000000000000000000000000" + user
				var replies []any
				for _, call := range calls {
					var result any
					switch call.Method {
					case "eth_createAccessList":
						var probe struct{ To, Data string }
						json.Unmarshal(call.Params[0], &probe)
						if probe.To != usdsAddress || probe.Data != balanceOf {
							t.Errorf("access list probes %s %s, want balanceOf(user) on USDS", probe.To, probe.Data)
						}
						result = map[string]any{"accessList": []any{map[string]any{"address": usdsAddress, "storageKeys": []string{"0x01"}}}}
					case "eth_simulateV1":
						var sim struct {
							BlockStateCalls []struct {
								Calls []struct{ To, Data string }
							}
						}
						json.Unmarshal(call.Params[0], &sim)
						want := []struct{ To, Data string }{{usdsAddress, balanceOf}}
						var served relayQuote
						json.Unmarshal(c.quote, &served)
						for _, step := range served.Steps {
							want = append(want, struct{ To, Data string }{step.Items[0].Data.To, step.Items[0].Data.Data})
						}
						if len(sim.BlockStateCalls) != 1 || fmt.Sprint(sim.BlockStateCalls[0].Calls) != fmt.Sprint(want) {
							t.Errorf("simulated %v, want the served quote's calls after balanceOf: %v", sim.BlockStateCalls, want)
						}
						var logs []any
						if c.credit != "" {
							amount, _ := new(big.Int).SetString(c.credit, 10)
							logs = append(logs, map[string]any{
								"address": relayDepository,
								"topics":  []string{"0x49fed1d0b752ce30eee63c7a81133f3363b532fec5d4d7dd1ccfd005de4555e1"},
								"data":    fmt.Sprintf("0x%064s%064s%064x%064x", user, "a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", amount, 1),
							})
						}
						result = []any{map[string]any{"calls": []any{
							map[string]any{"status": "0x1", "returnData": fmt.Sprintf("0x%064x", c.req.AmountRaw), "logs": []any{}},
							map[string]any{"status": "0x1", "returnData": "0x", "logs": []any{}},
							map[string]any{"status": "0x1", "returnData": "0x", "logs": logs},
						}}}
					}
					replies = append(replies, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
				}
				json.NewEncoder(w).Encode(replies)
			}))
			defer srv.Close()
			relay := &Relay{BaseURL: srv.URL, HTTP: srv.Client(), EVMRPC: map[Chain]string{Ethereum: srv.URL + "/rpc", Base: srv.URL + "/rpc"}}
			c.req.From, c.req.Recipient = "0x"+user, "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
			_, err := relay.Quote(context.Background(), c.req)
			if (err == nil) != c.accept {
				t.Errorf("accepted = %v, want %v (err = %v)", err == nil, c.accept, err)
			}
			if batches != c.rpcBatches {
				t.Errorf("%d RPC batches, want %d", batches, c.rpcBatches)
			}
		})
	}
}
