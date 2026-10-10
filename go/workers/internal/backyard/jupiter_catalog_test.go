package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Each catalog lane's conversion builds through its installed literal edge:
// the swap API is asked for the edge's mints and dialect, the retained
// instruction compiles under the edge's leg (with reviewed lookups when the
// legacy packet does not fit), and the worker refuses an instruction that does
// not swap the requested amount at the quoted output.
func TestCatalogJupiterInstructionsBuildThroughInstalledEdges(t *testing.T) {
	var headers struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json")
	if err != nil || json.Unmarshal(data, &headers) != nil {
		t.Fatal("retained Jupiter headers unavailable", err)
	}
	seen := map[string]bool{}
	for _, lane := range []string{"Ethena/USDe/PYUSD", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			edges, leg, err := catalogEdge(action, lane)
			if err != nil {
				t.Fatal(err)
			}
			edge := edges[leg]
			key := edge.from.symbol + "->" + edge.to.symbol
			seen[key] = true
			t.Run(lane+"/"+key, func(t *testing.T) {
				var instruction JupiterSwapInstruction
				for _, row := range headers.Rows {
					if row.Key == key {
						instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
						if strings.HasPrefix(lane, "Prime/") {
							instruction.LookupTableAddresses = row.LookupTables
						}
					}
				}
				at := int(edge.inAmountAt)
				data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
				if err != nil || len(data) != at+jupiter.PlatformFeeAfterInAmount+1 {
					t.Fatal("retained instruction missing")
				}
				amount, out := readU64(data[at:]), readU64(data[at+jupiter.QuotedOutAfterInAmount:])
				calls := 0
				client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					var payload any
					if r.Method == "GET" && r.URL.Path == "/quote" {
						q := r.URL.Query()
						if q.Get("inputMint") != edge.from.mint.String() || q.Get("outputMint") != edge.to.mint.String() || q.Get("amount") != fmt.Sprint(amount) {
							t.Fatal("quote identity drift")
						}
						payload = jupiter.Quote{InputMint: edge.from.mint.String(), OutputMint: edge.to.mint.String(), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
					} else if r.Method == "POST" && r.URL.Path == "/swap-instructions" {
						var request struct {
							UserPublicKey     string
							UseSharedAccounts bool
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						if request.UserPublicKey != bridgeVault || request.UseSharedAccounts != edge.shared {
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
				}))
				if err != nil {
					t.Fatal(err)
				}
				_, returned, err := freshSwapForRoute(context.Background(), client, lane, action, amount)
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
				policyKey, index, err := jupiterPolicyLeg(lane, action, data)
				if err != nil || index != leg {
					t.Fatal("policy selector drift", err)
				}
				request := JupiterSwapRequest{Action: action, RouteLane: lane, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: out,
					Policy: testPolicyAccount(policyKey), Instruction: instruction, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
				inner, err := validateJupiterInstructionForRoute(instruction, action, amount, out, out, lane)
				if err != nil {
					t.Fatal(err)
				}
				outer, err := wrapSquadsJupiterPolicy(mustKey(request.Policy), mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
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
				for _, offset := range []int{at, at + jupiter.QuotedOutAfterInAmount} {
					mutant := instruction
					changed := append([]byte(nil), data...)
					changed[offset] ^= 1
					mutant.Data = base64.StdEncoding.EncodeToString(changed)
					if _, err := validateJupiterInstructionForRoute(mutant, action, amount, out, out, lane); err == nil {
						t.Fatal("accepted another amount or quote", offset)
					}
				}
				mutant := instruction
				mutant.Data = base64.StdEncoding.EncodeToString(append(append([]byte(nil), data...), 0))
				if _, err := validateJupiterInstructionForRoute(mutant, action, amount, out, out, lane); err == nil {
					t.Fatal("accepted a route tail the edge's offsets do not end")
				}
			})
		}
	}
	if len(seen) != 14 {
		t.Fatal("missing conversion coverage", len(seen))
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
				s.RouteLane = ethenaUSDePYUSD.Lane // AUTO has no USDC->PYUSD edge
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
				prepareJupiter: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, JupiterExecutionEvidence, error) {
					if d != want {
						t.Fatal("conversion identity changed")
					}
					order = append(order, "prepare")
					return o, JupiterExecutionEvidence{Request: JupiterSwapRequest{Action: d.Action, RouteLane: d.StrategyKey}}, nil
				},
				recordDecision: func(_ context.Context, _ string, _ Observation, d Decision, _ string) (DecisionRecord, error) {
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
				bind: func(_ context.Context, id string, observed Observation, d Decision, _ any, _ ExpectedEffects) error {
					if id != "local-conversion" || !reflect.DeepEqual(observed, o) || d != want {
						t.Fatal("admission identity drift")
					}
					order = append(order, "admit")
					return nil
				},
			}}
			if err := worker.Tick(context.Background()); err != nil || strings.Join(order, ",") != "prepare,record,admit,build" {
				t.Fatal(order, err)
			}
			worker.runtime.bind = func(context.Context, string, Observation, Decision, any, ExpectedEffects) error {
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
