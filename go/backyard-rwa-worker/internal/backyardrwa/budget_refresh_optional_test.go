package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// The selector destination batch pins lifecycle addresses that may legitimately
// be absent (an unopened obligation or farm user state). A simulated reserve
// refresh over that batch must keep absent optional accounts absent — the same
// zero-value shape GetMultipleAccountsWithOptional returns — while every
// required address still resolves.
func TestSimulatedReserveRefreshOptionalCapturePreservesPinnedAbsence(t *testing.T) {
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatalf("NewRPCClient: %v", err)
	}
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch payload.Method {
		case "getLatestBlockhash":
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
		case "simulateTransaction":
			populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
			body := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[null,` + populated + `,null]}}}`
			return response(body), nil
		default:
			t.Fatalf("unexpected RPC method %q", payload.Method)
			return nil, nil
		}
	})
	addresses := []string{budgetClockAddress, bridgeVault, bridgeStrategy}
	slot, accounts, err := client.simulateBudgetReserveRefreshOptional(context.Background(), RouteID, addresses, []string{budgetClockAddress, bridgeStrategy}, 41)
	if err != nil || slot != 43 {
		t.Fatalf("slot=%d err=%v", slot, err)
	}
	for _, index := range []int{0, 2} {
		if accounts[index].Address != addresses[index] || accounts[index].Lamports != 0 || accounts[index].Owner != "" || accounts[index].Data != nil {
			t.Fatalf("optional absence was not preserved at %d: %+v", index, accounts[index])
		}
	}
	if accounts[1].Lamports != 9 || accounts[1].Data == nil {
		t.Fatalf("required capture corrupted: %+v", accounts[1])
	}
}

// Absence stays fail-closed everywhere it was never explicitly permitted: the
// optional-aware capture still rejects a null required account, and the strict
// price consumer keeps rejecting any null at all.
func TestSimulatedReserveRefreshCaptureStaysFailClosed(t *testing.T) {
	cases := []struct {
		name      string
		optional  bool
		nullIndex int
	}{
		{name: "optional_capture_rejects_required_null", optional: true, nullIndex: 1},
		{name: "strict_consumer_rejects_any_null", optional: false, nullIndex: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, err := NewRPCClient("https://rpc.invalid")
			if err != nil {
				t.Fatalf("NewRPCClient: %v", err)
			}
			client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var payload struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				switch payload.Method {
				case "getLatestBlockhash":
					return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
				case "simulateTransaction":
					populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
					values := []string{populated, populated}
					values[testCase.nullIndex] = "null"
					body := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[` + values[0] + `,` + values[1] + `]}}}`
					return response(body), nil
				default:
					t.Fatalf("unexpected RPC method %q", payload.Method)
					return nil, nil
				}
			})
			addresses := []string{budgetClockAddress, bridgeVault}
			var captureErr error
			if testCase.optional {
				_, _, captureErr = client.simulateBudgetReserveRefreshOptional(context.Background(), RouteID, addresses, []string{budgetClockAddress}, 41)
			} else {
				_, _, captureErr = client.simulateBudgetReserveRefresh(context.Background(), RouteID, addresses, 41)
			}
			var hold *BudgetHold
			if !errors.As(captureErr, &hold) || hold.Reason != "price_refresh_capture_incomplete" {
				t.Fatalf("unexpected error: %v", captureErr)
			}
		})
	}
}

// Capture keys are read-only non-signing static accounts. The header must stay
// a single-fee-payer message with canonical roles, deduplicated keys, valid
// instruction indexes, and no lowered privilege for instruction accounts.
func TestEncodeLegacyMessageCaptureAccountsStayReadOnly(t *testing.T) {
	feePayer := mustKey(bridgeDelegate)
	hash := mustKey(bridgeUSDC)
	instruction := compiledInstruction{
		program: mustKey(kaminoProgram),
		accounts: []accountMeta{
			{key: mustKey(bridgeUSDC), writable: true},
			{key: mustKey(budgetClockAddress), writable: true},
		},
		data: []byte{1, 2, 3},
	}
	// bridgeUSDC repeats an instruction account (must stay writable, not be
	// duplicated); the rest are capture-only read-only keys.
	capture := []publicKey{mustKey(bridgeVault), mustKey(bridgeUSDC), mustKey(bridgeStrategy), mustKey(bridgeVault)}
	message, err := encodeLegacyMessage(feePayer, hash, []compiledInstruction{instruction}, capture...)
	if err != nil {
		t.Fatalf("encodeLegacyMessage: %v", err)
	}
	if message[0] != 1 || message[1] != 0 {
		t.Fatalf("expected exactly one writable signer: header=%v", message[:3])
	}
	numAccounts := int(message[3])
	if numAccounts != 6 || message[2] != 3 {
		t.Fatalf("unexpected key count/header: header=%v", message[:4])
	}
	keys := make([]publicKey, numAccounts)
	seen := map[publicKey]bool{}
	for i := range keys {
		copy(keys[i][:], message[4+32*i:36+32*i])
		if seen[keys[i]] {
			t.Fatalf("duplicate message key at %d", i)
		}
		seen[keys[i]] = true
	}
	if keys[0] != feePayer {
		t.Fatalf("fee payer lost canonical position: %v", keys[0])
	}
	indexOf := func(key publicKey) int {
		for i := range keys {
			if keys[i] == key {
				return i
			}
		}
		return -1
	}
	usdcIndex, vaultIndex := indexOf(mustKey(bridgeUSDC)), indexOf(mustKey(bridgeVault))
	// Writable instruction accounts sort into the writable band ahead of
	// read-only capture keys; a capture duplicate never demotes them.
	if usdcIndex < 0 || vaultIndex < 0 || usdcIndex > vaultIndex {
		t.Fatalf("instruction privilege was lowered or key lost: usdc=%d vault=%d", usdcIndex, vaultIndex)
	}
	// Read the single instruction record and validate its account indexes.
	// Layout: keys, blockhash, shortvec instruction count, then the record.
	offset := 4 + 32*numAccounts + 32 + 1
	if int(message[offset]) != indexOf(mustKey(kaminoProgram)) {
		t.Fatalf("program index invalid")
	}
	count := int(message[offset+1])
	if count != 2 {
		t.Fatalf("unexpected instruction account count %d", count)
	}
	for i := 0; i < count; i++ {
		if int(message[offset+2+i]) >= numAccounts {
			t.Fatalf("instruction account index out of range")
		}
	}
}

// The live RPC only returns accounts named in the message, rejecting the rest
// with -32602. The compiled refresh message must therefore carry every
// requested capture address so one simulation returns the full batch.
func TestSimulatedReserveRefreshMessageCarriesCaptureAccounts(t *testing.T) {
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatalf("NewRPCClient: %v", err)
	}
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload.Method == "getLatestBlockhash" {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":41},"value":{"blockhash":"` + bridgeDelegate + `","lastValidBlockHeight":100}}}`), nil
		}
		if payload.Method != "simulateTransaction" {
			t.Fatalf("unexpected RPC method %q", payload.Method)
		}
		var wire string
		var config struct {
			Accounts struct {
				Addresses []string `json:"addresses"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(payload.Params[0], &wire); err != nil {
			t.Fatalf("decode wire param: %v", err)
		}
		if err := json.Unmarshal(payload.Params[1], &config); err != nil {
			t.Fatalf("decode config param: %v", err)
		}
		raw, err := base64.StdEncoding.DecodeString(wire)
		if err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		body := raw[1+64:] // version byte + all-zero placeholder signature
		numAccounts := int(body[3])
		messageKeys := map[publicKey]bool{}
		for i := 0; i < numAccounts; i++ {
			var key publicKey
			copy(key[:], body[4+32*i:36+32*i])
			messageKeys[key] = true
		}
		for _, address := range config.Accounts.Addresses {
			key, err := decodeKey(address)
			if err != nil || !messageKeys[key] {
				t.Fatalf("requested account %s is not a message key: %v", address, err)
			}
		}
		if len(config.Accounts.Addresses) > numAccounts {
			return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Too many accounts provided"}}`), nil
		}
		populated := `{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","lamports":9,"data":["AQ==","base64"],"executable":false}`
		body2 := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":{"err":null,"accounts":[` + populated + `,null]}}}`
		return response(body2), nil
	})
	slot, accounts, err := client.simulateBudgetReserveRefreshOptional(context.Background(), RouteID, []string{bridgeVault, budgetClockAddress}, []string{budgetClockAddress}, 41)
	if err != nil || slot != 43 {
		t.Fatalf("slot=%d err=%v", slot, err)
	}
	if accounts[0].Lamports != 9 || accounts[1].Data != nil || accounts[1].Address != budgetClockAddress {
		t.Fatalf("capture snapshot misaligned: %+v", accounts)
	}
}
