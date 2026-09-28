package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCatalogJupiterInstructionsMatchInstalledEdgesAndRejectMutations(t *testing.T) {
	read := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var headers struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []JupiterInstructionAccount
			}
		}
	}
	read("policy-jupiter-headers-v1.json", &headers)
	var installed struct {
		Operations []struct {
			PolicyAddress, DataSHA256, DataBase64 string
			Active                                bool
		}
	}
	read("policy-install-readback-v1.json", &installed)
	var compiled struct {
		Policies []struct {
			Policy      string
			Constraints []struct {
				ProgramID      string
				AccountPubkeys []struct {
					Index   int
					Pubkeys []string
				}
				Data []struct {
					Kind     string
					Offset   int
					Value    uint64
					ValueHex string
				}
			}
		}
	}
	read("policy-compiled-v1.json", &compiled)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, lane := range []string{"AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			b, err := catalogJupiterBindingForRoute(action, lane)
			if err != nil {
				t.Fatal(err)
			}
			key := b.From + "->" + b.To
			seen[key] = true
			t.Run(lane+"/"+key, func(t *testing.T) {
				foundPolicy := false
				for _, p := range installed.Operations {
					if p.PolicyAddress == b.Policy {
						bytes, err := base64.StdEncoding.Strict().DecodeString(p.DataBase64)
						if err != nil || !p.Active || p.DataSHA256 != b.PolicySHA256 || sha256Bytes(bytes) != b.PolicySHA256 {
							t.Fatal("installed policy provenance drift")
						}
						foundPolicy = true
					}
				}
				if !foundPolicy {
					t.Fatal("missing installed policy")
				}
				pinned := map[int]string{b.AuthorityIndex: b.Authority, b.SourceIndex: b.SourceCustody, b.DestinationIndex: b.DestinationCustody,
					b.SourceMintIndex: b.SourceMint, b.DestinationMintIndex: b.DestinationMint, b.SourceTokenProgramIndex: b.SourceTokenProgram, b.DestinationTokenProgramIndex: b.DestinationTokenProgram}
				foundConstraint := false
				for _, p := range compiled.Policies {
					if p.Policy == b.Policy {
						if int(b.ConstraintIndex) >= len(p.Constraints) {
							t.Fatal("constraint absent")
						}
						c := p.Constraints[b.ConstraintIndex]
						if c.ProgramID != jupiterV6Program || len(c.AccountPubkeys) != len(pinned) || len(c.Data) != 4 {
							t.Fatal("omitted compiled constraint")
						}
						for _, a := range c.AccountPubkeys {
							if len(a.Pubkeys) != 1 || pinned[a.Index] != a.Pubkeys[0] {
								t.Fatal("compiled account constraint drift")
							}
						}
						if c.Data[0].Kind != "slice-equals" || c.Data[0].Offset != 0 || c.Data[0].ValueHex != b.DiscriminatorHex ||
							c.Data[1].Kind != "u64-less-than-or-equal" || c.Data[1].Offset != b.AmountOffset || c.Data[1].Value != b.MaxInputRaw ||
							c.Data[2].Kind != "u16-less-than-or-equal" || c.Data[2].Offset != b.SlippageOffset || c.Data[2].Value != uint64(b.MaxSlippageBPS) ||
							c.Data[3].Kind != "u8-equals" || c.Data[3].Offset != b.FeeOffset || c.Data[3].Value != 0 {
							t.Fatal("compiled data constraint drift")
						}
						foundConstraint = true
					}
				}
				if !foundConstraint {
					t.Fatal("missing compiled policy")
				}
				var instruction JupiterSwapInstruction
				for _, row := range headers.Rows {
					if row.Key == key {
						instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
						if strings.HasPrefix(lane, "Prime/") {
							instruction.LookupTableAddresses = row.LookupTables
						}
					}
				}
				data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
				if err != nil || len(data) != b.FeeOffset+1 {
					t.Fatal("retained instruction missing")
				}
				amount, out := readU64(data[b.AmountOffset:]), readU64(data[b.AmountOffset+8:])
				calls := 0
				client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					var payload any
					if r.Method == "GET" && r.URL.Path == "/quote" {
						q := r.URL.Query()
						if q.Get("inputMint") != b.SourceMint || q.Get("outputMint") != b.DestinationMint || q.Get("amount") != fmt.Sprint(amount) {
							t.Fatal("quote identity drift")
						}
						payload = JupiterQuote{InputMint: b.SourceMint, OutputMint: b.DestinationMint, InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
					} else if r.Method == "POST" && r.URL.Path == "/swap-instructions" {
						var request struct {
							UserPublicKey     string
							UseSharedAccounts bool
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						if request.UserPublicKey != bridgeVault || request.UseSharedAccounts != (b.DiscriminatorHex == "c1209b3341d69c81") {
							t.Fatal("requested wrong installed instruction family")
						}
						payload = map[string]any{"swapInstruction": instruction, "addressLookupTableAddresses": []string{bridgeVault}}
					} else {
						t.Fatal("unexpected API call")
					}
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					return response(string(encoded)), nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				_, returned, err := client.freshSwapForRoute(context.Background(), lane, action, amount)
				if err != nil || calls != 2 {
					t.Fatal("production client failed exact retained layout", err, calls)
				}
				if (lane == "Ethena/USDe/PYUSD" && action == SwapCollateralToDebtStep) || strings.HasPrefix(lane, "Prime/") {
					if len(returned.LookupTableAddresses) != 1 || returned.LookupTableAddresses[0] != bridgeVault {
						t.Fatal("fresh lookup hints lost at API boundary")
					}
				} else if len(returned.LookupTableAddresses) != 0 {
					t.Fatal("lookup hints expanded outside selected conversion")
				}
				binding, err := manifest.jupiterPolicyForRoute(action, lane)
				if err != nil {
					t.Fatal(err)
				}
				index, err := binding.constraintIndex(instruction)
				if err != nil || index != b.ConstraintIndex {
					t.Fatal("policy selector drift", err)
				}
				request := JupiterSwapRequest{Action: action, RouteLane: lane, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: out,
					Policy: b.Policy, PolicyAccountDataSHA256: b.PolicySHA256, PolicyConstraintIndex: index, Instruction: instruction, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
				inner, err := validateJupiterInstructionForRoute(instruction, action, amount, out, out, lane)
				if err != nil {
					t.Fatal(err)
				}
				outer, err := wrapSquadsJupiterPolicy(mustKey(b.Policy), mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := compileLegacyMessage(mustKey(bridgeDelegate), mustKey(bridgeVault), []compiledInstruction{outer})
				if err != nil {
					t.Fatal(err)
				}
				message, err := CompileJupiterMessage(request)
				if len(raw)+65 <= solanaPacketBytes {
					if err != nil || !bytesEqual(message, raw) {
						t.Fatal("fitting packet rejected", err)
					}
				} else {
					if err == nil {
						t.Fatal("oversized legacy packet accepted")
					}
					request.LookupTables = retainedJupiterLookups(t)
					if strings.HasPrefix(lane, "Prime/") {
						request.LookupTables = nil
						tables := readRetainedJupiterLookups(t, "prime-sibling-lookup-review-2026-09-05.json", 8)
						for _, address := range instruction.LookupTableAddresses {
							found := false
							for _, table := range tables {
								if table.Address == address {
									request.LookupTables = append(request.LookupTables, table)
									found = true
								}
							}
							if !found {
								t.Fatal("missing independently captured Prime lookup")
							}
						}
					}
					message, err = CompileJupiterMessage(request)
					if err != nil || message[0] != 0x80 {
						t.Fatal("retained exit does not fit with reviewed lookups", err)
					}
					assertV0SDKParity(t, mustKey(bridgeDelegate), mustKey(bridgeVault), []compiledInstruction{outer}, request.LookupTables, message)
					if strings.HasPrefix(lane, "Prime/") {
						rpc, reads := lookupRPC(t, request.LookupTables, nil, false)
						unprepared := request
						unprepared.LookupTables = nil
						prepared, err := prepareJupiterLookupTables(context.Background(), rpc, unprepared, request.LookupTables[0].ObservedSlot)
						if err != nil || *reads != 1 {
							t.Fatal("Prime preparation did not load the hinted chain tables", err)
						}
						preparedMessage, err := CompileJupiterMessage(prepared)
						if err != nil || !bytesEqual(message, preparedMessage) {
							t.Fatal("fresh preparation changed Prime packet", err)
						}
						for _, tc := range []struct {
							mutate func(*LookupTableSnapshot)
							reason string
						}{
							{func(s *LookupTableSnapshot) { s.Data[56] ^= 1 }, "lookup_mapping_changed"},
							{func(s *LookupTableSnapshot) { s.Owner = classicTokenProgram }, "lookup_account_invalid"},
						} {
							rpc, _ := lookupRPC(t, request.LookupTables, tc.mutate, false)
							_, err := revalidateJupiterLookupTables(context.Background(), rpc, request, request.LookupTables[0].ObservedSlot)
							assertBudgetHold(t, err, tc.reason)
						}
					}
				}
				packetEvidence, _ := json.Marshal(map[string]any{"lane": lane, "edge": key, "packetBytes": len(message) + 65, "legacyPacketBytes": len(raw) + 65, "fits": len(message)+65 <= solanaPacketBytes})
				t.Logf("PHASE3_JUPITER_PACKET %s", packetEvidence)
				for index := range pinned {
					for _, field := range []string{"key", "signer", "writable"} {
						mutant := instruction
						mutant.Accounts = append([]JupiterInstructionAccount(nil), instruction.Accounts...)
						switch field {
						case "key":
							mutant.Accounts[index].Pubkey = bridgeSettings
						case "signer":
							mutant.Accounts[index].IsSigner = !mutant.Accounts[index].IsSigner
						case "writable":
							mutant.Accounts[index].IsWritable = !mutant.Accounts[index].IsWritable
						}
						if _, err := validateJupiterInstructionForRoute(mutant, action, amount, out, out, lane); err == nil {
							t.Fatalf("accepted %d %s substitution", index, field)
						}
					}
				}
				for _, offset := range []int{0, b.AmountOffset, b.AmountOffset + 8, b.FeeOffset} {
					mutant := instruction
					changed := append([]byte(nil), data...)
					changed[offset] ^= 1
					mutant.Data = base64.StdEncoding.EncodeToString(changed)
					if _, err := validateJupiterInstructionForRoute(mutant, action, amount, out, out, lane); err == nil {
						t.Fatal("accepted data substitution", offset)
					}
				}
				mutant := request
				mutant.Policy = bridgeAllocationPolicy
				if _, err := CompileJupiterMessage(mutant); err == nil {
					t.Fatal("accepted wrong policy")
				}
			})
		}
	}
	if len(seen) != 18 {
		t.Fatal("missing conversion coverage")
	}
	if _, _, _, _, err := jupiterEdgeForRoute(SwapUSDCToPrimeStep, "unknown/asset/debt"); err == nil {
		t.Fatal("unknown lane inherited Prime edge")
	}
}

func TestWorkerDispatchesNonUSDCConversionsWithoutChangingTheirIdentity(t *testing.T) {
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		t.Run(string(action), func(t *testing.T) {
			s := base()
			s.RouteLane = "AUTO/AUTO/PYUSD"
			switch action {
			case SwapStableToCollateralStep:
				s.SquadsIdleRaw = 5
			case SwapCollateralToStableStep:
				s.CutoverDrain = true
				s.CollateralIdleRaw = 5
			case SwapDebtToCollateralStep:
				s.HasPosition = true
				s.PositionCollateralRaw = 10
				s.PositionDebtRaw = 5
				s.DebtIdleRaw = 5
			case SwapCollateralToDebtStep:
				s.CutoverDrain = true
				s.HasPosition = true
				s.PositionCollateralRaw = 10
				s.PositionDebtRaw = 5
				s.CollateralIdleRaw = 5
				s.PositionDebtValueRaw, s.CollateralIdleValueRaw = 5, 6
			case SwapUSDCToDebtStep:
				s.CutoverDrain = true
				s.HasPosition = true
				s.PositionCollateralRaw = 10
				s.PositionDebtRaw = 5
				s.PositionDebtValueRaw, s.SquadsIdleRaw = 5, 6
			case SwapDebtToUSDCStep:
				s.CutoverDrain = true
				s.DebtIdleRaw = 5
			}
			o := tickObservation(s)
			want := Decide(s)
			if want.Action != action {
				t.Fatal(want)
			}
			order := []string{}
			worker := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil }, observe: func(context.Context) (Observation, error) { return o, nil },
				prepareJupiter: func(_ context.Context, _ RouteManifest, d Decision) (Observation, JupiterExecutionEvidence, error) {
					if d != want {
						t.Fatal("conversion identity changed")
					}
					order = append(order, "prepare")
					return o, JupiterExecutionEvidence{Request: JupiterSwapRequest{Action: d.Action, RouteLane: d.StrategyKey}}, nil
				},
				recordDecision: func(_ context.Context, _ string, _ Observation, d Decision, _, _ string) (DecisionRecord, error) {
					if d != want {
						t.Fatal(d)
					}
					order = append(order, "record")
					return DecisionRecord{OperationID: "local-conversion", Status: Decided}, nil
				},
				buildJupiter: func(_ context.Context, id string, e JupiterExecutionEvidence) error {
					if id != "local-conversion" || e.Request.Action != action || e.Request.RouteLane != s.RouteLane {
						return fmt.Errorf("dispatch drift")
					}
					order = append(order, "build")
					return nil
				},
				admitJupiter: func(_ context.Context, id string, observed Observation, d Decision, _ JupiterExecutionEvidence) error {
					if id != "local-conversion" || observed != o || d != want {
						t.Fatal("admission identity drift")
					}
					order = append(order, "admit")
					return nil
				},
			}}
			if err := worker.Tick(context.Background()); err != nil || strings.Join(order, ",") != "prepare,record,admit,build" {
				t.Fatal(order, err)
			}
			worker.runtime.admitJupiter = func(context.Context, string, Observation, Decision, JupiterExecutionEvidence) error {
				return budgetHold("complete_collateral_return_admission_unavailable")
			}
			worker.runtime.buildJupiter = func(context.Context, string, JupiterExecutionEvidence) error {
				t.Fatal("swap admission HOLD reached signing")
				return nil
			}
			assertBudgetHold(t, worker.Tick(context.Background()), "complete_collateral_return_admission_unavailable")
		})
	}
}
