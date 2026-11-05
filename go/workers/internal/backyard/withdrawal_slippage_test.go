package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func withTestSlippage(ix JupiterSwapInstruction, bps uint16) JupiterSwapInstruction {
	data, _ := base64.StdEncoding.DecodeString(ix.Data)
	binary.LittleEndian.PutUint16(data[25:27], bps)
	ix.Data = base64.StdEncoding.EncodeToString(data)
	return ix
}

func TestWithdrawalSlippageDecisionPriority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
		want   uint16
	}{
		{"partial withdrawal", func(s *Snapshot) {}, 20},
		{"later withdrawal tick", func(s *Snapshot) { s.ChainUnix += 3600 }, 20},
		{"partly covered", func(s *Snapshot) { s.VoltrIdleRaw = s.WithdrawalDemandRaw / 2 }, 20},
		{"cash covered", func(s *Snapshot) { s.VoltrIdleRaw = s.WithdrawalDemandRaw }, 0},
		{"no demand", func(s *Snapshot) { s.WithdrawalDemandRaw = 0 }, 0},
		{"collateral residue", func(s *Snapshot) { s.CollateralIdleRaw = 1_000_000; s.CollateralIdleValueRaw = 1_000_000 }, 20},
		{"debt residue", func(s *Snapshot) { s.DebtIdleRaw = 1_000_000 }, 20},
		{"hard LTV", func(s *Snapshot) { s.LTVBPS = s.MaxLTVBPS + 1 }, 0},
		{"term maturity", func(s *Snapshot) { s.ChainUnix = 1_700_000_000; s.DebtTermEndUnix = s.ChainUnix + 1 }, 0},
		{"Looping", func(s *Snapshot) { s.Trustful = true }, 0},
		{"signed recovery", func(s *Snapshot) { s.Nonterminal = Signed }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := livePartialSnapshot()
			tc.change(&s)
			s = targeted(s)
			d := Decide(s)
			if d.MaxSlippageBPS != tc.want {
				t.Fatalf("target=%+v decision=%+v, want %d bps", s.Target, d, tc.want)
			}
			if tc.want != 0 && d.Action != DeleverRouteStep && d.Action != SwapCollateralToStableStep && d.Action != SwapDebtToUSDCStep && d.Action != SwapDebtToCollateralStep {
				t.Fatalf("not a withdrawal sale: %+v", d)
			}
		})
	}
	s := targeted(livePartialSnapshot())
	s.Target = exitAllTarget
	if d := Decide(s); d.MaxSlippageBPS != 0 {
		t.Fatalf("operator exit tightened: %+v", d)
	}
	s = targeted(livePartialSnapshot())
	s.LTVBPS = s.MaxLTVBPS + 1
	if d := Decide(s); d.Reason != hardLTVDeleverReason || d.MaxSlippageBPS != 0 {
		t.Fatalf("stale withdrawal target tightened urgent repayment: %+v", d)
	}
}

func TestWithdrawalSlippageQuoteAndWireBoundaries(t *testing.T) {
	const amount, out = uint64(1_000_000), uint64(990_000)
	for _, tc := range []struct {
		name              string
		quoteBPS, wireBPS uint16
		minimum           uint64
		wantOK            bool
	}{
		{"accepted", 20, 20, 988020, true},
		{"quote widened", 50, 20, 988020, false},
		{"wire widened", 20, 50, 988020, false},
		{"minimum lowered", 20, 20, 985050, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instruction := withTestSlippage(jupiterTestInstruction(SwapCollateralToStableStep, amount, out), tc.wireBPS)
			source, destination, _, _, _ := jupiterEdgeForRoute(rwa, SwapCollateralToStableStep, PrimeUSDCLaneID)
			requested := false
			client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body []byte
				if r.URL.Path == "/quote" {
					requested = true
					if r.URL.Query().Get("slippageBps") != "20" {
						t.Error("withdrawal quote did not request 20 bps")
					}
					body = []byte(fmt.Sprintf(`{"inputMint":%q,"outputMint":%q,"inAmount":"1000000","outAmount":"990000","otherAmountThreshold":"%d","swapMode":"ExactIn","slippageBps":%d,"platformFee":null,"routePlan":[{}]}`, source, destination, tc.minimum, tc.quoteBPS))
				} else {
					body, _ = json.Marshal(map[string]any{"swapInstruction": instruction})
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = freshSwapForRoute(context.Background(), rwa, client, PrimeUSDCLaneID, SwapCollateralToStableStep, amount, 32, 20)
			if !requested || (err == nil) != tc.wantOK {
				t.Fatalf("requested=%v err=%v", requested, err)
			}
			if !tc.wantOK {
				var hold *PreSendHold
				if !errors.As(err, &hold) || hold.Reason != holdPriceLimit {
					t.Fatalf("missing price-limit reason: %v", err)
				}
			}
		})
	}
	// Multiplication must not wrap for large integer token amounts.
	if floor := jupiterOutputFloor(math.MaxUint64, 20); floor != 18_409_850_585_562_132_511 {
		t.Fatalf("overflowed floor: %d", floor)
	}
}

func TestWithdrawalSlippageCompilesSignsAndRejectsWidening(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{63}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, lane := range []string{PrimeUSDCLaneID, MapleSyrupUSDCLaneID, autoAUTOPYUSD.Lane} {
		t.Run(lane, func(t *testing.T) {
			step, _, err := stepTestExit(t, lane, 0)
			if err != nil {
				t.Fatal(err)
			}
			step.Swap.MaxSlippageBPS = 20
			step.Swap.Instruction = withTestSlippage(step.Swap.Instruction, 20)
			step.Swap.MinimumOutputRaw = jupiterOutputFloor(step.Swap.QuotedOutputRaw, 20)
			for _, atomic := range []bool{false, true} {
				compile := func() ([]byte, error) {
					if atomic {
						return compileStepMessageForDelegate(rwa, step, delegate)
					}
					return compileJupiterMessageForDelegate(rwa, *step.Swap, delegate)
				}
				message, err := compile()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ix := range decodeTestInstructions(t, message) {
					if ix.ProgramID != squads.ProgramID {
						continue
					}
					execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
					if err != nil {
						t.Fatal(err)
					}
					for _, inner := range execution.Inner {
						if inner.ProgramID == jupiter.ProgramID {
							args, err := jupiter.DecodeSharedAccountsRouteV2Args(inner.Data)
							if err != nil || args.SlippageBPS != 20 {
								t.Fatalf("actual wire not bounded: %+v %v", args, err)
							}
							found = true
						}
					}
				}
				if !found {
					t.Fatal("no executable swap")
				}
				signed, err := signMessage(key, delegate, message, step.Swap.RecentBlockhash, step.Swap.LastValidBlockHeight)
				if err != nil {
					t.Fatal(err)
				}
				result, err := signed.BuildResult(123)
				if err != nil {
					t.Fatal(err)
				}
				if err = result.validateForDelegate(delegate); err != nil {
					t.Fatalf("persist/sign gate: %v", err)
				}
				saved := step.Swap.Instruction
				step.Swap.Instruction = withTestSlippage(saved, 50)
				if _, err = compile(); err == nil {
					t.Fatal("widened instruction compiled")
				}
				step.Swap.Instruction = saved
				minimum := step.Swap.MinimumOutputRaw
				for _, bad := range []uint64{minimum - 1, minimum + 1} {
					step.Swap.MinimumOutputRaw = bad
					if _, err = compile(); err == nil {
						t.Fatal("unenforceable minimum compiled")
					}
				}
				step.Swap.MinimumOutputRaw = minimum
			}
		})
	}
}

func TestWithdrawalSlippageDurableDecisionAndLegacyRecovery(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: targeted(livePartialSnapshot())}
	d := Decide(o.Snapshot)
	raw, err := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, "manifest")})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restorePersistedDecision(raw, d.Action, d.IdempotencyKey, d.StrategyKey)
	if err != nil || restored != d {
		t.Fatalf("restart lost decision: %+v %v", restored, err)
	}
	legacy := bytes.Replace(raw, []byte(`,"maxSlippageBps":20`), nil, 1)
	restored, err = restorePersistedDecision(legacy, d.Action, d.IdempotencyKey, d.StrategyKey)
	if err != nil || restored.MaxSlippageBPS != 0 {
		t.Fatalf("legacy replay changed: %+v %v", restored, err)
	}
	for _, bad := range []string{"51", "65536", "-1"} {
		corrupted := bytes.Replace(raw, []byte(`"maxSlippageBps":20`), []byte(`"maxSlippageBps":`+bad), 1)
		if _, err := restorePersistedDecision(corrupted, d.Action, d.IdempotencyKey, d.StrategyKey); err == nil {
			t.Fatal("invalid durable limit accepted")
		}
	}
	// An older already signed 50-bps exit goes directly to exact-wire recovery.
	request := fixtureSwapRequest(t, PrimeUSDCLaneID, SwapCollateralToStableStep)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{67}, ed25519.SeedSize))
	signed, err := buildAndSignJupiterTransactionForDelegate(request, key, publicKeyFromBytes(key.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: "old-signed-exit", Decision: restored}, Status: Signed, SignedWire: signed.signedWire}
	advanced := false
	w := Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return &op, nil },
		observe: func(context.Context) (Observation, error) {
			t.Fatal("signed recovery reobserved/replanned")
			return Observation{}, nil
		},
		advance: func(_ context.Context, got PersistedOperation) error {
			advanced = true
			if !bytes.Equal(got.SignedWire, signed.signedWire) {
				t.Fatal("signed wire changed")
			}
			return nil
		},
	}}
	if err := w.Tick(context.Background()); err != nil || !advanced {
		t.Fatalf("legacy recovery: advanced=%v err=%v", advanced, err)
	}
}

func TestWithdrawalSlippageHoldAndLaterCashRecovery(t *testing.T) {
	failure := &simulationFailure{raw: json.RawMessage(`{"InstructionError":[2,{"Custom":6001}]}`), logs: []string{"Program " + jupiter.ProgramID.String() + " failed: custom program error: 0x1771"}, text: "Jupiter slippage limit exceeded"}
	err := simulationRefused(failure)
	var hold *PreSendHold
	if !errors.As(err, &hold) || hold.Reason != holdPriceLimit {
		t.Fatalf("simulation refusal: %v", err)
	}
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: targeted(livePartialSnapshot())}
	h, ok := assessWithdrawalHealth(rwa, o, Decide(o.Snapshot), err)
	if !ok || h.Status != "unavailable" || h.Reason != "withdrawal_price_limit" {
		t.Fatalf("missing actionable hold: %+v", h)
	}
	o.ObservedAt = o.ObservedAt.Add(11 * time.Minute)
	o.Snapshot.Slot++
	o.Snapshot.ChainUnix += 660
	if d := Decide(o.Snapshot); d.MaxSlippageBPS != 20 {
		t.Fatalf("age widened withdrawal: %+v", d)
	}
	o.Snapshot.VoltrIdleRaw = o.Snapshot.WithdrawalDemandRaw
	recovered, ok := assessWithdrawalHealth(rwa, o, Decision{}, nil)
	recovered = mergeWithdrawalHealth(&h.WithdrawalHealth, recovered)
	if !ok || recovered.Reason != "withdrawal_covered" || strings.Contains(recovered.Reason, "limit") {
		t.Fatalf("cash recovery not shown: %+v", recovered)
	}
}

func TestWithdrawalSlippageRetriesAfterReobserving(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: targeted(livePartialSnapshot())}
	observations, preparations, records, builds := 0, 0, 0, 0
	var health withdrawalAssessment
	w := Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observations++
			o.Snapshot.Slot++
			o.ObservedAt = o.ObservedAt.Add(time.Minute)
			return o, nil
		},
		prepareStep: func(_ context.Context, _ RouteManifest, d Decision, observed Observation) (Observation, StepExecutionEvidence, error) {
			preparations++
			if d.MaxSlippageBPS != 20 || observations != preparations {
				t.Fatalf("retry lost the limit or fresh observation: %+v", d)
			}
			if preparations == 1 {
				return Observation{}, StepExecutionEvidence{}, holdOf(holdPriceLimit, "jupiter_quote_slippage", nil)
			}
			return observed, StepExecutionEvidence{}, nil
		},
		recordDecision: func(_ context.Context, _ string, _ Observation, d Decision, _ string) (DecisionRecord, error) {
			records++
			if d.MaxSlippageBPS != 20 {
				t.Fatal("retry changed durable limit")
			}
			return DecisionRecord{OperationID: "repriced-withdrawal", Status: Decided}, nil
		},
		build:            func(context.Context, string, any, ExpectedEffects) error { builds++; return nil },
		withdrawalHealth: func(_ context.Context, h withdrawalAssessment) error { health = h; return nil },
	}}
	if err := w.Tick(context.Background()); !isPreSendHold(err) || records != 0 || builds != 0 || health.Reason != "withdrawal_price_limit" {
		t.Fatalf("refusal advanced execution: %v, records=%d builds=%d health=%+v", err, records, builds, health)
	}
	if err := w.Tick(context.Background()); err != nil || observations != 2 || records != 1 || builds != 1 || health.Reason != "withdrawal_in_progress" {
		t.Fatalf("acceptable retry did not resume: %v, observations=%d records=%d builds=%d health=%+v", err, observations, records, builds, health)
	}
}
