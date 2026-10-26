package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	t.Parallel()
	seen := map[string]bool{}
	for _, lane := range []string{ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			edge, err := catalogEdge(action, lane)
			if err != nil {
				t.Fatal(err)
			}
			key := edge.conversion().from + "->" + edge.conversion().to
			seen[key] = true
			t.Run(lane+"/"+key, func(t *testing.T) {
				request := fixtureSwapRequest(t, lane, action)
				if _, index, err := jupiterPolicyLeg(rwa, lane, action); err != nil || index != edge.leg {
					t.Fatal("policy selector drift", err)
				}
				message := compileTestSwap(t, request)
				packetEvidence, _ := json.Marshal(map[string]any{"lane": lane, "edge": key, "transactionBytes": len(message) + 64})
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
					if _, err := validateJupiterInstructionForRoute(rwa, mutant, action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, lane); err == nil {
						t.Fatal("accepted another amount or quote", offset)
					}
				}
			})
		}
	}
	if len(seen) != 14 {
		t.Fatal("missing conversion coverage", len(seen))
	}
	if _, _, _, _, err := jupiterEdgeForRoute(rwa, SwapStableToCollateralStep, "unknown/asset/debt"); err == nil {
		t.Fatal("unknown lane inherited Prime edge")
	}
}

// The worker dispatches the wYLDS lane's debt conversions to Hastra and its
// collateral and USDC conversions to Jupiter, carrying each decided
// conversion's identity unchanged through prepare, record and build;
// AUTO/AUTO/PYUSD keeps dispatching every conversion to Jupiter (above).
// (Borrowed wYLDS stakes inside an atomic step; collateral into debt is a
// down step's conversion; TestEveryHastraLegExecutesAtItsOwnConstraint builds
// both.)
func TestWorkerDispatchesWYLDSDebtConversionsToHastra(t *testing.T) {
	t.Parallel()
	for action, venue := range map[Action]string{SwapStableToCollateralStep: "jupiter", SwapCollateralToStableStep: "jupiter",
		SwapDebtToCollateralStep: "hastra"} {
		t.Run(string(action), func(t *testing.T) {
			s := base()
			s.RouteLane = autoAUTOWYLDSLane
			switch action {
			case SwapStableToCollateralStep:
				s.SquadsIdleRaw, s.Target = 5, LaneTarget{Rule: ruleEntry}
			case SwapCollateralToStableStep:
				s.CollateralIdleRaw, s.Target = 5_000, exitAllTarget
			case SwapDebtToCollateralStep:
				// An exit's wYLDS residue stakes to AUTO.
				s.DebtIdleRaw, s.Target = 5_000, exitAllTarget
			}
			o := tickObservation(s)
			want := Decide(s)
			if want.Action != action || want.StrategyKey != autoAUTOWYLDSLane {
				t.Fatal(want)
			}
			var order []string
			worker := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil }, observe: func(context.Context) (Observation, error) { return o, nil },
				prepareJupiter: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, JupiterExecutionEvidence, error) {
					order = append(order, "jupiter")
					return o, JupiterExecutionEvidence{Request: JupiterSwapRequest{Action: d.Action, RouteLane: d.StrategyKey}}, nil
				},
				prepareHastra: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, HastraExecutionEvidence, error) {
					if d != want {
						t.Fatal("conversion identity changed")
					}
					order = append(order, "hastra")
					return o, HastraExecutionEvidence{Request: HastraConversionRequest{Action: d.Action, RouteLane: d.StrategyKey}}, nil
				},
				recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
					order = append(order, "record")
					return DecisionRecord{OperationID: "local-conversion", Status: Decided}, nil
				},
				build: func(_ context.Context, id string, request any, _ ExpectedEffects) error {
					switch r := request.(type) {
					case JupiterSwapRequest:
						order = append(order, "build-jupiter")
					case HastraConversionRequest:
						if id != "local-conversion" || r.Action != action || r.RouteLane != autoAUTOWYLDSLane {
							return fmt.Errorf("dispatch drift")
						}
						order = append(order, "build-hastra")
					}
					return nil
				},
			}}
			if err := worker.Tick(context.Background()); err != nil || strings.Join(order, ",") != venue+",record,build-"+venue {
				t.Fatal(order, err)
			}
		})
	}
}
