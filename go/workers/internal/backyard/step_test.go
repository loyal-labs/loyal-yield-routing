package backyard

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// stepTestPolicies is the captured policy set, with every literal the capture
// predates (the wYLDS lane's, Hastra's) at a stand-in account, and each
// account's literal.
func stepTestPolicies(t testing.TB) (installedPolicies, map[solana.PublicKey]policyKey, map[policyKey]squads.Policy) {
	t.Helper()
	policies := testPolicies(t)
	literals, err := backyardPolicies(rwa)
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[solana.PublicKey]policyKey{}
	for key := range literals {
		if policies[key].matches != 1 {
			policies[key] = installedPolicy{Installed: squads.Installed{Account: solana.PublicKey(sha256.Sum256([]byte(key.String())))}, matches: 1}
		}
		byAccount[policies[key].Account] = key
	}
	return policies, byAccount, literals
}

// stepTestBuild compiles lane's up or down step through the captured
// policies: 1,000,000 raw borrowed or withdrawn and converted, the close leg
// the conversion's guaranteed output. The conversion is Hastra's at about the
// mainnet price, or the fixture's Jupiter swap with venue synthetic venue
// accounts behind its twelve fixed ones (venue < 0 keeps the fixture route).
func stepTestBuild(t *testing.T, lane string, up bool, venue int) (StepRequest, stepMessage, error) {
	t.Helper()
	manifest := embeddedTestManifest(t)
	policies, _, _ := stepTestPolicies(t)
	route, err := runtimeRoute(lane)
	if err != nil {
		t.Fatal(err)
	}
	openLeg, closeLeg, action, conversion := kaminoLegWithdraw, kaminoLegRepay, DeleverRouteStep, SwapCollateralToDebtStep
	if up {
		openLeg, closeLeg, action, conversion = kaminoLegBorrow, kaminoLegDeposit, OpenRouteStep, SwapDebtToCollateralStep
	}
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	open, err := manifest.kaminoPacketForRoute(policies, action, openLeg, 1_000_000, blockhash, lane)
	if err != nil {
		t.Fatal(err)
	}
	open.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	step := StepRequest{Open: open}
	var request any
	if leg, ok := hastraConversionLeg(conversion, lane); ok {
		account, err := policies.account(policyKey{hastra: hastraName()})
		if err != nil {
			t.Fatal(err)
		}
		price := hastra.StakePrice{Price: hastraTestPrice, Scale: 1_000_000_000_000_000_000, Timestamp: kaminoFixtureUnix}
		r := HastraConversionRequest{Action: conversion, AmountRaw: 1_000_000, Price: price.Price.String(), PriceScale: price.Scale, PriceTimestamp: price.Timestamp,
			Policy: account, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99, RouteLane: lane}
		if r.OutputRaw, err = hastraOutput(leg, price, r.AmountRaw); err != nil {
			t.Fatal(err)
		}
		step.Hastra, request = &r, r
	} else {
		r := stepTestSwap(t, lane, conversion, venue)
		step.Swap, request = &r, r
	}
	closeRaw, err := stepCloseAmount(0, request)
	if err != nil {
		t.Fatal(err)
	}
	closing, err := manifest.kaminoPacketForRoute(policies, action, closeLeg, closeRaw, blockhash, lane)
	if err != nil {
		t.Fatal(err)
	}
	step.Close = &closing
	message, err := buildStepMessage(rwa, step, mustKey(bridgeDelegate))
	return step, message, err
}

// stepTestSwap is the fixture's Jupiter swap of lane's conversion, with venue
// synthetic venue accounts behind its twelve fixed ones (venue < 0 keeps the
// fixture route).
func stepTestSwap(t *testing.T, lane string, conversion Action, venue int) JupiterSwapRequest {
	t.Helper()
	r := fixtureSwapRequest(t, lane, conversion)
	if venue >= 0 {
		r.Instruction.Accounts = append([]jupiter.AccountMeta{}, r.Instruction.Accounts[:12]...)
		for i := range venue {
			key := sha256.Sum256(fmt.Appendf(nil, "venue %s %d", lane, i))
			r.Instruction.Accounts = append(r.Instruction.Accounts, jupiter.AccountMeta{Pubkey: encodeBase58(key[:]), IsWritable: i%2 == 0})
		}
	}
	return r
}

// stepTestExit compiles lane's exit step through the captured policies: the
// whole 1,000,000 raw deposit withdrawn beside no debt and swapped to USDC.
func stepTestExit(t *testing.T, lane string, venue int) (StepRequest, stepMessage, error) {
	t.Helper()
	route, err := runtimeRoute(lane)
	if err != nil {
		t.Fatal(err)
	}
	policies, _, _ := stepTestPolicies(t)
	open, err := embeddedTestManifest(t).kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegWithdraw, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, lane)
	if err != nil {
		t.Fatal(err)
	}
	open.ObligationReserves = []string{route.Kamino.CollateralReserve}
	swap := stepTestSwap(t, lane, SwapCollateralToStableStep, venue)
	step := StepRequest{Open: open, Swap: &swap}
	message, err := buildStepMessage(rwa, step, mustKey(bridgeDelegate))
	return step, message, err
}

// An equity-zero exit's last step is one v1 message that closes nothing:
// the refresh prefix, the withdrawal and its swap to USDC into the Squads
// bridge account, each at the one constraint of its installed literal that
// admits it. A close leg on it does not compile.
func TestExitStepWithdrawsAndSwapsToUSDCInOneMessage(t *testing.T) {
	t.Parallel()
	_, byAccount, literals := stepTestPolicies(t)
	for _, lane := range []string{autoAUTOWYLDSLane, autoAUTOPYUSD.Lane, mapleSyrupUSDCUSDC.Lane} {
		step, message, err := stepTestExit(t, lane, -1)
		if err != nil {
			t.Fatalf("%s: %v", lane, err)
		}
		instructions := decodeTestInstructions(t, message.Message)
		last := len(instructions) - 1
		withdraw, swap := instructions[last-1], instructions[last]
		for i, ix := range []squads.Instruction{withdraw, swap} {
			execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
			if ix.ProgramID != squads.ProgramID || err != nil || len(execution.Inner) != 1 {
				t.Fatalf("%s: instruction %d is not one Squads execute: %v", lane, last-1+i, err)
			}
			inner, key := execution.Inner[0], byAccount[execution.Policy]
			if i == 0 && (inner.ProgramID != kamino.ProgramID || [8]byte(inner.Data[:8]) != kamino.WithdrawV2Discriminator) {
				t.Fatalf("%s: the exit does not open with the KLend withdrawal", lane)
			}
			if i == 1 && inner.ProgramID != jupiter.ProgramID {
				t.Fatalf("%s: the exit does not end with the Jupiter swap", lane)
			}
			var admitting []int
			for index, constraint := range literals[key].Constraints {
				if squads.Admits(constraint, inner, vaultObligation) {
					admitting = append(admitting, index)
				}
			}
			if fmt.Sprint(admitting) != fmt.Sprint([]int{int(execution.ConstraintIndexes[0])}) {
				t.Errorf("%s: %s executes at constraint %d, but constraints %v admit it", lane, key, execution.ConstraintIndexes[0], admitting)
			}
		}
		if _, _, _, to, err := jupiterEdgeForRoute(rwa, SwapCollateralToStableStep, lane); err != nil || to != bridgeSquadsATA {
			t.Fatalf("%s: the exit swaps into %s (%v), not the Squads bridge account", lane, to, err)
		}
		closing := step.Open
		step.Close = &closing
		if _, err := buildStepMessage(rwa, step, mustKey(bridgeDelegate)); err == nil {
			t.Fatalf("%s: a closing withdrawal compiled into an exit", lane)
		}
	}
}

// An atomic step is one v1 message: each KLend execute directly behind its
// own refresh prefix, the conversion between them, every Squads execute at
// the one constraint of its installed literal that admits it.
func TestAtomicStepIsOneMessageOfPolicyExecutes(t *testing.T) {
	t.Parallel()
	_, byAccount, literals := stepTestPolicies(t)
	for _, lane := range []string{autoAUTOWYLDSLane, autoAUTOPYUSD.Lane, primePRIMEPYUSD.Lane} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		for _, up := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/up=%t", lane, up), func(t *testing.T) {
				_, step, err := stepTestBuild(t, lane, up, -1)
				if err != nil {
					t.Fatal(err)
				}
				instructions := decodeTestInstructions(t, step.Message)
				if len(instructions) != 9 {
					t.Fatalf("step has %d instructions, want 9", len(instructions))
				}
				open, closeLeg := kamino.BorrowV2Discriminator, kamino.DepositV2Discriminator
				if !up {
					open, closeLeg = kamino.WithdrawV2Discriminator, kamino.RepayV2Discriminator
				}
				for _, prefix := range []int{0, 5} {
					for i, want := range [][8]byte{kamino.RefreshReserveDiscriminator, kamino.RefreshReserveDiscriminator, kamino.RefreshObligationDiscriminator} {
						ix := instructions[prefix+i]
						if ix.ProgramID != kamino.ProgramID || [8]byte(ix.Data[:8]) != want {
							t.Fatalf("instruction %d is not the refresh prefix of the KLend execute at %d", prefix+i, prefix+3)
						}
					}
				}
				// The close leg's obligation refresh runs over the topology the open
				// leg left: the collateral deposit, then the debt.
				if refresh := instructions[7]; len(refresh.Accounts) != 4 || refresh.Accounts[2].PublicKey.String() != route.Kamino.CollateralReserve ||
					refresh.Accounts[3].PublicKey.String() != route.Kamino.DebtReserve {
					t.Fatalf("close refresh runs over %v", refresh.Accounts)
				}
				for i, want := range map[int][]byte{3: open[:], 4: nil, 8: closeLeg[:]} {
					ix := instructions[i]
					execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
					if ix.ProgramID != squads.ProgramID || err != nil {
						t.Fatalf("instruction %d is not a Squads execute: %v", i, err)
					}
					key, ok := byAccount[execution.Policy]
					if !ok || len(execution.Inner) != 1 || len(execution.ConstraintIndexes) != 1 {
						t.Fatalf("instruction %d executes %+v through %s, no installed literal", i, execution, execution.Policy)
					}
					inner := execution.Inner[0]
					if want != nil && (inner.ProgramID != kamino.ProgramID || string(inner.Data[:8]) != string(want)) {
						t.Fatalf("instruction %d executes the wrong KLend leg", i)
					}
					var admitting []int
					for index, constraint := range literals[key].Constraints {
						if squads.Admits(constraint, inner, vaultObligation) {
							admitting = append(admitting, index)
						}
					}
					if fmt.Sprint(admitting) != fmt.Sprint([]int{int(execution.ConstraintIndexes[0])}) {
						t.Errorf("%s: instruction %d executes at constraint %d, but constraints %v admit it", key, i, execution.ConstraintIndexes[0], admitting)
					}
				}
			})
		}
	}
}

// The close leg is exactly held custody plus what the conversion's own wire
// guarantees: a Jupiter swap's quoted out less its slippage, never the
// advisory JSON threshold; Hastra's output at its quoted price.
func TestStepCloseAmountIsHeldPlusTheGuaranteedOutput(t *testing.T) {
	t.Parallel()
	swap := fixtureSwapRequest(t, autoAUTOPYUSD.Lane, SwapDebtToCollateralStep)
	floor, err := jupiterInstructionWireFloor(swap.Instruction)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := stepCloseAmount(7, swap); err != nil || got != 7+floor {
		t.Fatalf("Jupiter close %d %v, want %d", got, err, 7+floor)
	}
	conversion := HastraConversionRequest{OutputRaw: 976_612}
	if got, err := stepCloseAmount(7, conversion); err != nil || got != 7+976_612 {
		t.Fatalf("Hastra close %d %v", got, err)
	}
	if _, err := stepCloseAmount(^uint64(0), conversion); err == nil {
		t.Fatal("an overflowing close amount builds")
	}
}

// Every earn lane's up and down step fits one v1 transaction at the route
// size its swap is quoted at (stepSwapMaxAccounts: the 64 keys less the
// step's own), and at the captured fixture route; a route past that size is
// refused, never truncated. Run with -v for the table.
func TestEveryEarnStepFitsOneV1Transaction(t *testing.T) {
	t.Parallel()
	var table strings.Builder
	fmt.Fprintf(&table, "%-20s %-4s %-12s %5s %5s\n", "lane", "dir", "route", "keys", "bytes")
	for _, lane := range earnLaneIDs(true) {
		for _, up := range []bool{true, false} {
			conversion := map[bool]Action{true: SwapDebtToCollateralStep, false: SwapCollateralToDebtStep}[up]
			if _, hastra := hastraConversionLeg(conversion, lane); hastra {
				_, step, err := stepTestBuild(t, lane, up, -1)
				if err != nil || step.Keys > maxTransactionKeys || step.WireBytes > maxTransactionBytes {
					t.Errorf("%s up=%t: %d keys, %d bytes: %v", lane, up, step.Keys, step.WireBytes, err)
				}
				fmt.Fprintf(&table, "%-20s %-4s %-12s %5d %5d\n", lane, map[bool]string{true: "up", false: "down"}[up], "hastra", step.Keys, step.WireBytes)
				continue
			}
			request, _, _ := stepTestBuild(t, lane, up, 0)
			venue, err := stepSwapMaxAccounts(rwa, request, conversion)
			if err != nil || venue < 16 {
				t.Fatalf("%s up=%t: route size %d: %v", lane, up, venue, err)
			}
			for _, variant := range []int{-1, venue} {
				_, step, err := stepTestBuild(t, lane, up, variant)
				name := fmt.Sprintf("max %d", venue)
				if variant < 0 {
					name = "fixture"
				}
				if err != nil || step.Keys > maxTransactionKeys || step.WireBytes > maxTransactionBytes {
					t.Errorf("%s up=%t %s: %d keys, %d bytes: %v", lane, up, name, step.Keys, step.WireBytes, err)
				}
				fmt.Fprintf(&table, "%-20s %-4s %-12s %5d %5d\n", lane, map[bool]string{true: "up", false: "down"}[up], name, step.Keys, step.WireBytes)
			}
			if _, _, err := stepTestBuild(t, lane, up, venue+1); err == nil {
				t.Errorf("%s up=%t: a route one account past its size compiled", lane, up)
			}
		}
		request, _, _ := stepTestExit(t, lane, 0)
		venue, err := stepSwapMaxAccounts(rwa, request, SwapCollateralToStableStep)
		if err != nil || venue < 16 {
			t.Fatalf("%s exit: route size %d: %v", lane, venue, err)
		}
		for _, variant := range []int{-1, venue} {
			_, step, err := stepTestExit(t, lane, variant)
			name := map[bool]string{true: "fixture", false: fmt.Sprintf("max %d", venue)}[variant < 0]
			if err != nil || step.Keys > maxTransactionKeys || step.WireBytes > maxTransactionBytes {
				t.Errorf("%s exit %s: %d keys, %d bytes: %v", lane, name, step.Keys, step.WireBytes, err)
			}
			fmt.Fprintf(&table, "%-20s %-4s %-12s %5d %5d\n", lane, "exit", name, step.Keys, step.WireBytes)
		}
		if _, _, err := stepTestExit(t, lane, venue+1); err == nil {
			t.Errorf("%s exit: a route one account past its size compiled", lane)
		}
	}
	t.Log("\n" + table.String())
}
