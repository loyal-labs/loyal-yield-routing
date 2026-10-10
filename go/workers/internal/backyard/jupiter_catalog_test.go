package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Each catalog lane's conversion builds through its literal edge: the swap
// API is asked for a V2 shared-accounts route of the edge's mints, the live
// instruction compiles under the edge's leg (through lookup tables when it
// does not fit a legacy packet), and the worker refuses an instruction that
// does not swap the requested amount at the quoted output.
func TestCatalogJupiterInstructionsBuildThroughInstalledEdges(t *testing.T) {
	seen := map[string]bool{}
	for _, lane := range []string{ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			edges, leg, err := catalogEdge(action, lane)
			if err != nil {
				t.Fatal(err)
			}
			key := edges[leg].from.symbol + "->" + edges[leg].to.symbol
			seen[key] = true
			t.Run(lane+"/"+key, func(t *testing.T) {
				request := fixtureSwapRequest(t, lane, action)
				if hinted := len(request.Instruction.LookupTableAddresses) > 0; hinted != acceptsJupiterLookupHints(lane, action) {
					t.Fatalf("lookup hints kept %v outside their lanes", hinted)
				}
				if _, index, err := jupiterPolicyLeg(lane, action); err != nil || index != leg {
					t.Fatal("policy selector drift", err)
				}
				message, tables := compileTestSwap(t, request)
				packetEvidence, _ := json.Marshal(map[string]any{"lane": lane, "edge": key, "packetBytes": len(message) + 65, "v0": len(tables) > 0})
				t.Logf("PHASE3_JUPITER_PACKET %s", packetEvidence)
				data, err := base64.StdEncoding.Strict().DecodeString(request.Instruction.Data)
				if err != nil {
					t.Fatal(err)
				}
				for _, offset := range []int{jupiter.V2InAmountOffset, jupiter.V2QuotedOutOffset} {
					mutant := request.Instruction
					changed := append([]byte(nil), data...)
					changed[offset] ^= 1
					mutant.Data = base64.StdEncoding.EncodeToString(changed)
					if _, err := validateJupiterInstructionForRoute(mutant, action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, lane); err == nil {
						t.Fatal("accepted another amount or quote", offset)
					}
				}
				if len(tables) == 0 || !strings.HasPrefix(lane, "Prime/") {
					return
				}
				request.LookupTables = tables
				rpc, reads := lookupRPC(t, tables, nil, false)
				unprepared := request
				unprepared.LookupTables = nil
				prepared, err := prepareJupiterLookupTables(context.Background(), rpc, unprepared, tables[0].ObservedSlot)
				if err != nil || *reads != 1 {
					t.Fatal("Prime preparation did not load the hinted chain tables", err)
				}
				if preparedMessage, err := CompileJupiterMessage(prepared); err != nil || !bytesEqual(message, preparedMessage) {
					t.Fatal("fresh preparation changed Prime packet", err)
				}
				rpc, _ = lookupRPC(t, tables, func(s *LookupTableSnapshot) { s.Data[56] ^= 1 }, false)
				_, err = revalidateJupiterLookupTables(context.Background(), rpc, request, tables[0].ObservedSlot)
				assertBudgetHold(t, err, "lookup_mapping_changed")
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
