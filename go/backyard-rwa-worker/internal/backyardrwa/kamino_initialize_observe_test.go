package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

func initializationPrestateFixture(t *testing.T) (KaminoInitializationRequest, map[string]ConfirmedAccount) {
	e, _ := initializationReconcileFixture(t)
	r := *e.Initialization
	r.PolicySeed = 139 // Captured Settings last-used seed is 139.
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	route, _ := runtimeRoute(r.RouteLane)
	policy, _ := policySetupAddress(r.PolicySeed)
	policyAddress := encodeBase58(policy[:])
	metadataAddress := encodeBase58(inner.accounts[6].key[:])
	rentAddress := "SysvarRent111111111111111111111111111111111"
	system := "11111111111111111111111111111111"
	accounts := map[string]ConfirmedAccount{}
	for _, meta := range inner.accounts {
		address := encodeBase58(meta.key[:])
		accounts[address] = ConfirmedAccount{Address: address, Owner: system, Lamports: 1}
	}
	delete(accounts, route.Kamino.Obligation)
	accounts[bridgeSettings] = setupSettingsAccount(t)
	accounts[policyAddress] = ConfirmedAccount{Address: policyAddress, Owner: bridgeSquadsProgram, Lamports: 1, Data: []byte("controlled policy")}
	accounts[bridgeVault] = ConfirmedAccount{Address: bridgeVault, Owner: system, Lamports: r.RentLamports}
	accounts[bridgeDelegate] = ConfirmedAccount{Address: bridgeDelegate, Owner: system, Lamports: r.MaximumFeeLamports}
	m := ConfirmedAccount{Address: metadataAddress, Owner: kaminoProgram, Lamports: 1, Data: make([]byte, 1032)}
	copy(m.Data, []byte{157, 214, 220, 235, 98, 135, 171, 28})
	putKey(t, m.Data[80:112], bridgeVault)
	accounts[metadataAddress] = m
	accounts[route.Kamino.Market] = marketFixture(t, route.Kamino.Market)
	for _, mint := range []string{route.Kamino.CollateralMint, bridgeUSDC} {
		a := ConfirmedAccount{Address: mint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
		a.Data[45] = 1
		accounts[mint] = a
	}
	rent := ConfirmedAccount{Address: rentAddress, Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 17)}
	binary.LittleEndian.PutUint64(rent.Data, 5080)
	binary.LittleEndian.PutUint64(rent.Data[8:16], math.Float64bits(1))
	accounts[rentAddress] = rent
	return r, accounts
}

// Exercise the actual RPC null-account contract and the native funding gate.
// Only the exact target obligation may be absent; all prerequisites must exist.
func TestInitializationPrestateRequiresAbsentTargetAndFundedExactGraph(t *testing.T) {
	for _, drift := range []string{"", "target_exists", "missing_policy", "policy_hash", "policy_owner", "settings_authority", "settings_seed", "vault_funding", "delegate_funding", "metadata_owner", "metadata_referrer", "metadata_vault", "market_emergency", "mint_program", "mint_uninitialized", "rent_changed", "rent_nan", "old_slot"} {
		t.Run(drift, func(t *testing.T) {
			r, accounts := initializationPrestateFixture(t)
			inner, _ := kaminoMultiplyInitializer(r.RouteLane)
			route, _ := runtimeRoute(r.RouteLane)
			policy, _ := policySetupAddress(r.PolicySeed)
			policyAddress := encodeBase58(policy[:])
			metadataAddress := encodeBase58(inner.accounts[6].key[:])
			rentAddress := "SysvarRent111111111111111111111111111111111"
			change := func(address string, fn func(*ConfirmedAccount)) {
				a := accounts[address]
				fn(&a)
				accounts[address] = a
			}
			switch drift {
			case "target_exists":
				accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Lamports: 1}
			case "missing_policy":
				delete(accounts, policyAddress)
			case "policy_hash":
				change(policyAddress, func(a *ConfirmedAccount) { a.Data[0] ^= 1 })
			case "policy_owner":
				change(policyAddress, func(a *ConfirmedAccount) { a.Owner = classicTokenProgram })
			case "settings_authority":
				change(bridgeSettings, func(a *ConfirmedAccount) { a.Data[24] = 1 })
			case "settings_seed":
				r.PolicySeed = 140
			case "vault_funding":
				change(bridgeVault, func(a *ConfirmedAccount) { a.Lamports-- })
			case "delegate_funding":
				change(bridgeDelegate, func(a *ConfirmedAccount) { a.Lamports-- })
			case "metadata_owner":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Owner = classicTokenProgram })
			case "metadata_referrer":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Data[8] = 1 })
			case "metadata_vault":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Data[80] ^= 1 })
			case "market_emergency":
				change(route.Kamino.Market, func(a *ConfirmedAccount) { a.Data[kaminoMarketEmergencyModeOffset] = 1 })
			case "mint_program":
				change(bridgeUSDC, func(a *ConfirmedAccount) { a.Owner = bridgeSquadsProgram })
			case "mint_uninitialized":
				change(bridgeUSDC, func(a *ConfirmedAccount) { a.Data[45] = 0 })
			case "rent_changed":
				change(rentAddress, func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data, 5081) })
			case "rent_nan":
				change(rentAddress, func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data[8:16], math.Float64bits(math.NaN())) })
			}
			rpc, _ := NewRPCClient("https://rpc.invalid")
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var body struct {
					Method string
					Params []json.RawMessage
					ID     any
				}
				_ = json.NewDecoder(req.Body).Decode(&body)
				if body.Method != "getMultipleAccounts" {
					t.Fatal("unexpected RPC method", body.Method)
				}
				var addresses []string
				var config map[string]any
				_ = json.Unmarshal(body.Params[0], &addresses)
				_ = json.Unmarshal(body.Params[1], &config)
				if config["commitment"] != "confirmed" || config["minContextSlot"] != float64(77) {
					t.Fatal("unanchored prestate")
				}
				values := make([]any, len(addresses))
				for i, address := range addresses {
					if a, ok := accounts[address]; ok {
						values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
					}
				}
				slot := 78
				if drift == "old_slot" {
					slot = 76
				}
				encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": map[string]any{"context": map[string]any{"slot": slot}, "value": values}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
			})
			slot, err := validateKaminoInitializationPrestate(context.Background(), rpc, r, 77)
			if (err == nil) != (drift == "") || (err == nil && slot != 78) {
				t.Fatalf("drift=%s slot=%d err=%v", drift, slot, err)
			}
		})
	}
}

func TestInitializationMissingPrerequisiteKeepsValidatedExpiryRecovery(t *testing.T) {
	e, _ := initializationReconcileFixture(t)
	r := *e.Initialization
	raw, _ := json.Marshal(e)
	input, err := encodePhase3BuildInput(r, raw)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	wire = append(wire, message...)
	digest, err := Phase3IntentDigest(r, raw)
	if err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{Decision: Decision{Action: InitializeKaminoObligation, StrategyKey: r.RouteLane, Reason: "multiply_obligation_missing", IdempotencyKey: "initializer-controlled"}}, Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, SignedWireSHA256: op.SignedWireSHA256, BuildInput: input}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		var result any
		switch body.Method {
		case "getSlot":
			result = 77
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			values := make([]any, len(addresses))
			for i, a := range addresses {
				values[i] = map[string]any{"owner": bridgeSquadsProgram, "lamports": 1, "data": []string{"", "base64"}}
				if a != bridgeSettings {
					values[i] = nil
				}
			}
			result = map[string]any{"context": map[string]any{"slot": 78}, "value": values}
		default:
			t.Fatal("unexpected RPC", body.Method)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	})
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	var hold *validatedSignedBudgetHold
	if !errors.As(err, &hold) || hold.hold.Reason != "initializer_prestate_unavailable" {
		t.Fatalf("missing policy stranded validated wire: %v", err)
	}
	op.SignedWireSHA256 = sha256Bytes([]byte("other"))
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	if errors.As(err, &hold) {
		t.Fatal("untrusted wire gained expiry permission")
	}
}

// Fresh later fee and oracle data must not extend the earlier policy/rent read.
func TestInitializationBuildPricesRentAndRetainsPrestateExpiry(t *testing.T) {
	for _, finalSlot := range []int64{60, 74, 75} {
		t.Run(fmt.Sprint(finalSlot), func(t *testing.T) {
			r, accounts := initializationPrestateFixture(t)
			// Controlled lower rent allows the success path under the finite canary cap.
			r.RentLamports = 3_472_000
			rent := accounts["SysvarRent111111111111111111111111111111111"]
			binary.LittleEndian.PutUint64(rent.Data, 1000)
			e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
			rpc := budgetBuildRPC(t, 5000, finalSlot)
			base := rpc.client.Transport
			slotReads := 0
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				req.Body = io.NopCloser(bytes.NewReader(raw))
				var body struct {
					Method string
					Params []json.RawMessage
					ID     any
				}
				_ = json.Unmarshal(raw, &body)
				var result any
				switch body.Method {
				case "getSlot":
					slotReads++
					result = 42
					if slotReads > 1 {
						result = finalSlot
					}
				case "getFeeForMessage":
					result = map[string]any{"context": map[string]any{"slot": 60}, "value": 5000}
				case "getMultipleAccounts":
					var addresses []string
					_ = json.Unmarshal(body.Params[0], &addresses)
					if len(addresses) > 0 && addresses[0] == bridgeSettings {
						values := make([]any, len(addresses))
						for i, address := range addresses {
							if a, ok := accounts[address]; ok {
								values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
							}
						}
						result = map[string]any{"context": map[string]any{"slot": 42}, "value": values}
					} else {
						response, err := base.RoundTrip(req)
						if err != nil {
							return nil, err
						}
						defer response.Body.Close()
						var envelope map[string]any
						if json.NewDecoder(response.Body).Decode(&envelope) != nil {
							t.Fatal("base response")
						}
						out := envelope["result"].(map[string]any)
						out["context"] = map[string]any{"slot": 60}
						// Keep the reserve update fresh in the later valuation batch.
						values := out["value"].([]any)
						for i, address := range addresses {
							if address == budgetSOLReserve {
								a := values[i].(map[string]any)
								d := a["data"].([]any)
								data, _ := base64.StdEncoding.DecodeString(d[0].(string))
								binary.LittleEndian.PutUint64(data[16:24], 60)
								d[0] = base64.StdEncoding.EncodeToString(data)
							}
						}
						result = out
					}
				default:
					t.Fatal("unexpected build RPC", body.Method)
				}
				encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
			})
			cost, err := observePhase3KnownBuildCost(context.Background(), rpc, r, e)
			if finalSlot == 75 {
				assertBudgetHold(t, err, "initializer_prestate_expired")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cost.SetupLamports != r.RentLamports || cost.SetupLamportsMicros <= 0 || cost.PrincipalMicros != 0 || cost.TotalMicros != cost.SetupLamportsMicros+cost.NetworkFeeMicros || cost.ValidThroughSlot != 74 {
				t.Fatalf("rent or prestate bound lost: %+v", cost)
			}
		})
	}
}
