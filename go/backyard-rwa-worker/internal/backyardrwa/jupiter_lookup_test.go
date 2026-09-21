package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
)

func retainedEthenaExit(t *testing.T) (JupiterSwapRequest, ExpectedEffects) {
	t.Helper()
	b, err := catalogJupiterBindingForRoute(SwapCollateralToDebtStep, "Ethena/USDe/PYUSD")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []JupiterInstructionAccount
			}
		}
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	r := JupiterSwapRequest{Action: SwapCollateralToDebtStep, RouteLane: "Ethena/USDe/PYUSD", Policy: b.Policy, PolicyAccountDataSHA256: b.PolicySHA256, PolicyConstraintIndex: b.ConstraintIndex, RecentBlockhash: bridgeVault, LastValidBlockHeight: 999, LookupTables: retainedJupiterLookups(t)}
	for _, row := range evidence.Rows {
		if row.Key != "USDe->PYUSD" {
			continue
		}
		r.Instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
		if fmt.Sprint(row.LookupTables) != fmt.Sprint(jupiterLookupAddresses(r)) {
			t.Fatal("lookup identities diverge from retained route")
		}
	}
	ix, _ := base64.StdEncoding.Strict().DecodeString(r.Instruction.Data)
	r.AmountRaw, r.QuotedOutputRaw = readU64(ix[b.AmountOffset:]), readU64(ix[b.AmountOffset+8:])
	r.MinimumOutputRaw = r.QuotedOutputRaw
	return r, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: b.SourceCustody, Owner: b.SourceTokenProgram, Mint: b.SourceMint, Authority: bridgeVault, BeforeRaw: r.AmountRaw, AfterRaw: 0},
		{Address: b.DestinationCustody, Owner: b.DestinationTokenProgram, Mint: b.DestinationMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: r.MinimumOutputRaw, MinimumAfterRaw: &r.MinimumOutputRaw},
	}}
}

func lookupRPC(t *testing.T, tables []LookupTableSnapshot, mutate func(*LookupTableSnapshot), allowFee bool) (*RPCClient, *int) {
	t.Helper()
	rpc, _ := NewRPCClient("https://rpc.invalid")
	reads := 0
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var result any
		switch body.Method {
		case "getSlot":
			result = tables[0].ObservedSlot + 1
		case "getMultipleAccounts":
			reads++
			var addresses []string
			var config struct{ MinContextSlot int64 }
			if json.Unmarshal(body.Params[0], &addresses) != nil || json.Unmarshal(body.Params[1], &config) != nil || config.MinContextSlot < tables[0].ObservedSlot {
				t.Fatal("lookup read dropped minimum slot")
			}
			values := []any{}
			for i, address := range addresses {
				if i >= len(tables) || address != tables[i].Address {
					t.Fatal("unexpected lookup fetch identity")
				}
				s := tables[i]
				s.Data = append([]byte(nil), s.Data...)
				if i == 0 && mutate != nil {
					mutate(&s)
				}
				values = append(values, map[string]any{"owner": s.Owner, "lamports": s.Lamports, "executable": s.Executable, "data": []string{base64.StdEncoding.EncodeToString(s.Data), "base64"}})
			}
			result = map[string]any{"context": map[string]any{"slot": tables[0].ObservedSlot + 1}, "value": values}
		case "getFeeForMessage":
			if !allowFee {
				t.Fatal("invalid lookup reached fee valuation")
			}
			var encoded string
			_ = json.Unmarshal(body.Params[0], &encoded)
			message, _ := base64.StdEncoding.DecodeString(encoded)
			if len(message) < 4 || message[0] != 0x80 || message[1] != 1 {
				t.Fatal("fee request lost v0 message")
			}
			// Deliberately no fee/blockhash availability. The positive table path
			// reaches this next gate but may not load a signer or submit a wire.
			result = map[string]any{"context": map[string]any{"slot": tables[0].ObservedSlot + 1}, "value": nil}
		default:
			t.Fatalf("unexpected RPC/signing/simulation/send: %s", body.Method)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc, &reads
}

func TestJupiterLookupPreparationAndFinalSendRejectChangedAccounts(t *testing.T) {
	r, effects := retainedEthenaExit(t)
	// Exercise real v0 signing using deterministic, non-production test keys.
	// No production secret or signature is needed for byte/signature proof.
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignJupiterTransactionForDelegate(r, key, delegate)
	if err != nil || signed.signedWire[0] != 1 || signed.signedWire[65] != 0x80 ||
		!ed25519.Verify(key.Public().(ed25519.PublicKey), signed.signedWire[65:], signed.signedWire[1:65]) {
		t.Fatal("versioned wire does not retain valid one-signer envelope", err)
	}
	rpc, reads := lookupRPC(t, r.LookupTables, nil, false)
	legacy := r
	legacy.LookupTables = nil
	prepared, err := prepareJupiterLookupTables(context.Background(), rpc, legacy, r.LookupTables[0].ObservedSlot)
	if err != nil || len(prepared.LookupTables) != 2 || *reads != 1 {
		t.Fatal("production preparation failed", err)
	}
	original, err := CompileJupiterMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*JupiterSwapRequest){
		func(r *JupiterSwapRequest) { r.LookupTables = r.LookupTables[:1] },
		func(r *JupiterSwapRequest) { r.LookupTables[0].Address = bridgeVault },
		func(r *JupiterSwapRequest) { r.RouteLane = "AUTO/AUTO/PYUSD" },
	} {
		bad := r
		bad.LookupTables = append([]LookupTableSnapshot(nil), r.LookupTables...)
		mutate(&bad)
		if _, err := CompileJupiterMessage(bad); err == nil {
			t.Fatal("unreviewed lookup identity accepted")
		}
	}
	for name, tc := range map[string]struct {
		mutate   func(*LookupTableSnapshot)
		reason   string
		allowFee bool
	}{
		"mapping-substitution": {func(s *LookupTableSnapshot) { s.Data[56] ^= 1 }, "lookup_mapping_changed", false},
		"prefix-shortened":     {func(s *LookupTableSnapshot) { s.Data = s.Data[:len(s.Data)-32] }, "lookup_mapping_changed", false},
		"deactivated":          {func(s *LookupTableSnapshot) { s.Data[4] = 0 }, "lookup_account_invalid", false},
		"wrong-owner":          {func(s *LookupTableSnapshot) { s.Owner = bridgeTokenProgram }, "lookup_account_invalid", false},
		"active-append":        {func(s *LookupTableSnapshot) { s.Data = append(s.Data, make([]byte, 32)...) }, "network_fee_unavailable", true},
		"unchanged":            {nil, "network_fee_unavailable", true},
	} {
		t.Run(name, func(t *testing.T) {
			rpc, reads := lookupRPC(t, r.LookupTables, tc.mutate, tc.allowFee)
			err := BuildSimulateAndPersistJupiter(context.Background(), &Database{}, rpc, "lookup-negative", JupiterExecutionEvidence{Request: r, ExpectedEffects: effects})
			assertBudgetHold(t, err, tc.reason)
			if *reads != 1 {
				t.Fatal("builder did not refresh lookup tables")
			}
			encoded, err := jsonMarshalExpectedEffects(effects)
			if err != nil {
				t.Fatal(err)
			}
			input, err := encodePhase3BuildInput(r, encoded)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := Phase3IntentDigest(r, encoded)
			if err != nil {
				t.Fatal(err)
			}
			wire := append(make([]byte, 65), original...)
			wire[0] = 1 // unsigned local fixture, not signer proof
			op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
			auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, SignedWireSHA256: op.SignedWireSHA256, BuildInput: input}
			_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
			assertBudgetHold(t, err, tc.reason)
			if *reads != 2 || !bytes.Equal(original, op.SignedWire[65:]) {
				t.Fatal("final send failed fresh lookup check or changed wire")
			}
		})
	}
	// Legacy requests must retain their pre-v0 JSON shape and intent digest.
	encoded, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(encoded, []byte("LookupTables")) {
		t.Fatal("legacy persisted intent changed")
	}
}

func TestFreshJupiterLookupHintsPreservePolicyAndPersistedMapping(t *testing.T) {
	r, _ := retainedEthenaExit(t)
	// A different table can encode the same already-validated instruction keys.
	// Its address grants no account, signer or program authority.
	r.LookupTables[0].Address = encodeBase58(bytes.Repeat([]byte{83}, 32))
	for _, table := range r.LookupTables {
		r.Instruction.LookupTableAddresses = append(r.Instruction.LookupTableAddresses, table.Address)
	}
	rpc, reads := lookupRPC(t, r.LookupTables, nil, false)
	input := r
	input.LookupTables = nil
	prepared, err := prepareJupiterLookupTables(context.Background(), rpc, input, r.LookupTables[0].ObservedSlot)
	if err != nil || *reads != 1 {
		t.Fatal("fresh hint preparation failed", err)
	}
	message, err := CompileJupiterMessage(prepared)
	if err != nil || len(message)+65 > solanaPacketBytes {
		t.Fatal("fresh lookup packet failed", err)
	}
	encoded, _ := json.Marshal(prepared)
	var restored JupiterSwapRequest
	if json.Unmarshal(encoded, &restored) != nil {
		t.Fatal("persisted lookup decode failed")
	}
	again, err := CompileJupiterMessage(restored)
	if err != nil || !bytes.Equal(message, again) {
		t.Fatal("persisted hint changed wire", err)
	}
	if _, err := revalidateJupiterLookupTables(context.Background(), rpc, restored, r.LookupTables[0].ObservedSlot); err != nil {
		t.Fatal("final-send cannot revalidate fresh hint", err)
	}
	rpc, _ = lookupRPC(t, r.LookupTables, func(s *LookupTableSnapshot) { s.Data[56] ^= 1 }, false)
	_, err = revalidateJupiterLookupTables(context.Background(), rpc, restored, r.LookupTables[0].ObservedSlot)
	assertBudgetHold(t, err, "lookup_mapping_changed")
	for _, addresses := range [][]string{{"not-a-key"}, {bridgeVault, bridgeVault}, {bridgeVault, bridgeDelegate, bridgeUSDC, bridgeSquadsATA, bridgeTokenProgram}} {
		bad := input
		bad.Instruction.LookupTableAddresses = addresses
		if _, err := CompileJupiterMessage(bad); err == nil {
			t.Fatal("invalid hint accepted")
		}
	}
	bad := restored
	bad.Instruction.Accounts = append([]JupiterInstructionAccount(nil), restored.Instruction.Accounts...)
	b, _ := catalogJupiterBindingForRoute(bad.Action, bad.RouteLane)
	bad.Instruction.Accounts[b.DestinationIndex].Pubkey = bridgeVault
	if _, err := CompileJupiterMessage(bad); err == nil {
		t.Fatal("lookup hint bypassed destination policy")
	}
}
