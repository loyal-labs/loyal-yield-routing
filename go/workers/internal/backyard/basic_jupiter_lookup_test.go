package backyard

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func basicSwapActionForLeg(leg string) Action {
	switch leg {
	case "USDC->PRIME", "USDC->syrupUSDC", "USDC->ONyc":
		return SwapStableToCollateralStep
	case "PRIME->USDC", "syrupUSDC->USDC", "ONyc->USDC":
		return SwapCollateralToStableStep
	}
	return ""
}

// basicJupiterRequest is a basic lane's swap of leg as the production builder
// makes it from the v2 fixture, and the lookup tables it needs when it does
// not fit a legacy packet.
func basicJupiterRequest(t *testing.T, lane, leg string) (JupiterSwapRequest, []LookupTableSnapshot) {
	t.Helper()
	request := fixtureSwapRequest(t, lane, basicSwapActionForLeg(leg))
	_, tables := compileTestSwap(t, request)
	return request, tables
}

// decodeV0OuterInstruction reads the single Squads execute back out of a
// compiled v0 message. The Squads authority accounts must stay static, so their
// references are resolved from the static key list while venue accounts past
// them may legitimately resolve through a table. AUTO resource messages carry
// the ComputeBudget heap frame ahead of the outer, so the Squads instruction is
// located by program rather than by position; installed single-instruction
// messages decode exactly as before.
func decodeV0OuterInstruction(t *testing.T, message []byte) ([]string, []string, []byte) {
	t.Helper()
	offset := 4
	staticCount, err := decodeShortVec(message, &offset)
	if err != nil || staticCount == 0 || 4+32*staticCount > len(message) {
		t.Fatalf("v0 message truncates its static keys: %v", err)
	}
	staticKeys := make([]string, 0, staticCount)
	for index := 0; index < staticCount; index++ {
		staticKeys = append(staticKeys, encodeBase58(message[offset:offset+32]))
		offset += 32
	}
	offset += 32 // recent blockhash
	instructions, err := decodeShortVec(message, &offset)
	if err != nil || instructions == 0 || instructions > 3 {
		t.Fatalf("v0 message carries an unsupported instruction count: %v", err)
	}
	found := 0
	accounts := []string(nil)
	var outerData []byte
	for index := 0; index < instructions; index++ {
		programIndex, err := decodeShortVec(message, &offset)
		if err != nil || programIndex >= staticCount {
			t.Fatalf("v0 instruction %d program drifted: %v", index, err)
		}
		isOuter := staticKeys[programIndex] == squads.ProgramID.String()
		accountsCount, err := decodeShortVec(message, &offset)
		if err != nil {
			t.Fatal(err)
		}
		if isOuter {
			found++
			accounts = make([]string, 0, 3)
		}
		for account := 0; account < accountsCount; account++ {
			accountIndex, err := decodeShortVec(message, &offset)
			if err != nil {
				t.Fatal(err)
			}
			if !isOuter || account >= 3 {
				continue // venue accounts may legitimately resolve through a table
			}
			if accountIndex >= staticCount {
				t.Fatalf("v0 outer authority account %d moved into a lookup table", account)
			}
			accounts = append(accounts, staticKeys[accountIndex])
		}
		dataLength, err := decodeShortVec(message, &offset)
		if err != nil || offset+dataLength > len(message) {
			t.Fatalf("v0 message truncates its instruction data: %v", err)
		}
		if isOuter {
			outerData = message[offset : offset+dataLength]
		}
		offset += dataLength
	}
	if found != 1 {
		t.Fatalf("v0 message does not carry exactly one Squads instruction: %d", found)
	}
	return staticKeys, accounts, outerData
}

// Every basic swap leg builds from the swap API's answer: one that fits keeps
// its legacy packet and reads no table; one that does not compiles to v0
// through the API's lookup tables with the Squads authorities static, and the
// runtime's chain preparation of those tables yields the same packet.
func TestBasicSwapLegsBuildLegacyOrV0FromQuotedLookupHints(t *testing.T) {
	for lane, legs := range map[string][2]string{PhaseOneLaneID: {"USDC->PRIME", "PRIME->USDC"}, SelectedRouteID: {"USDC->syrupUSDC", "syrupUSDC->USDC"}, onreONycUSDC: {"USDC->ONyc", "ONyc->USDC"}} {
		for _, leg := range legs {
			t.Run(lane+"/"+leg, func(t *testing.T) {
				request, tables := basicJupiterRequest(t, lane, leg)
				if len(request.Instruction.LookupTableAddresses) == 0 {
					t.Fatal("basic lane dropped its quoted lookup hints")
				}
				message, err := CompileJupiterMessage(request)
				if len(tables) == 0 {
					if err != nil || message[0] != 1 {
						t.Fatalf("fitting basic lane leg did not stay legacy: %v", err)
					}
					prepared, err := embeddedTestManifest(t).prepareJupiterLookupTables(context.Background(), nil, request, 1)
					if err != nil || len(prepared.LookupTables) != 0 {
						t.Fatalf("fitting leg reached chain lookup preparation: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "unsigned message does not fit") {
					t.Fatalf("oversized request without tables compiled as %v", err)
				}
				request.LookupTables = tables
				message, err = CompileJupiterMessage(request)
				if err != nil || message[0] != 0x80 || message[1] != 1 || len(message)+65 > solanaPacketBytes {
					t.Fatalf("validated hints did not produce a fitting v0 outer message: %v", err)
				}
				staticKeys, accounts, _ := decodeV0OuterInstruction(t, message)
				for _, authority := range []string{request.Policy, squads.ProgramID.String(), bridgeDelegate} {
					if !slices.Contains(staticKeys, authority) {
						t.Fatalf("v0 message offloaded authority account %s", authority)
					}
				}
				if accounts[0] != request.Policy || accounts[1] != squads.ProgramID.String() || accounts[2] != bridgeDelegate {
					t.Fatalf("v0 outer authority accounts drifted: %v", accounts)
				}
				rpc, reads := lookupRPC(t, tables, nil, false)
				unprepared := request
				unprepared.LookupTables = nil
				prepared, err := embeddedTestManifest(t).prepareJupiterLookupTables(context.Background(), rpc, unprepared, tables[0].ObservedSlot)
				if err != nil || *reads != 1 || len(prepared.LookupTables) != len(tables) {
					t.Fatalf("basic lane preparation did not load the hinted chain tables: %v", err)
				}
				if preparedMessage, err := CompileJupiterMessage(prepared); err != nil || !bytes.Equal(message, preparedMessage) {
					t.Fatalf("fresh preparation changed the basic lane packet: %v", err)
				}
			})
		}
	}
}

func TestBasicLaneLookupHintsStayScopedToSwapEdges(t *testing.T) {
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		if !acceptsJupiterLookupHints(lane, SwapStableToCollateralStep) || !acceptsJupiterLookupHints(lane, SwapCollateralToStableStep) {
			t.Fatalf("basic lane %s lost its swap hint admission", lane)
		}
		for _, action := range []Action{OpenRouteStep, DeleverRouteStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep, HoldManualRecovery} {
			if acceptsJupiterLookupHints(lane, action) {
				t.Fatalf("basic lane %s admitted hints for %s", lane, action)
			}
		}
	}
	if acceptsJupiterLookupHints(RouteID, SwapUSDCToPrimeStep) {
		t.Fatal("legacy Prime lane changed its hint admission")
	}
	for _, lane := range []string{"unknown/asset/debt"} {
		if acceptsJupiterLookupHints(lane, SwapStableToCollateralStep) {
			t.Fatalf("lane %s gained unregistered hint admission", lane)
		}
	}
	// The candidate AUTO lane keeps the v0 escape hatch on its five reviewed
	// edges only; unknown routes and non-edge actions stay rejected.
	for _, action := range []Action{SwapStableToCollateralStep, SwapDebtToCollateralStep, SwapCollateralToStableStep, SwapCollateralToDebtStep, SwapDebtToUSDCStep} {
		if !acceptsJupiterLookupHints(autoAUTOPYUSD.Lane, action) {
			t.Fatalf("AUTO lane lost its exact-edge hint admission for %s", action)
		}
	}
	for _, action := range []Action{SwapUSDCToDebtStep, OpenRouteStep, DeleverRouteStep, HoldManualRecovery} {
		if acceptsJupiterLookupHints(autoAUTOPYUSD.Lane, action) {
			t.Fatalf("AUTO lane admitted hints for non-edge action %s", action)
		}
	}
}
