package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func jupiterTestInstruction(action Action, amount, out uint64, v2 bool) JupiterSwapInstruction {
	sourceMint, destinationMint, sourceATA, destinationATA, _ := jupiterEdge(action)
	accounts := make([]JupiterInstructionAccount, 10)
	for index := range accounts {
		accounts[index] = JupiterInstructionAccount{Pubkey: bridgeTokenProgram}
	}
	dataLength := 41
	if action == SwapPrimeToUSDCStep {
		dataLength = 37
	}
	data := make([]byte, dataLength)
	copy(data, jupiterSharedAccountsRoute)
	accounts[0] = JupiterInstructionAccount{Pubkey: bridgeTokenProgram}
	accounts[2] = JupiterInstructionAccount{Pubkey: bridgeVault, IsSigner: true}
	accounts[3] = JupiterInstructionAccount{Pubkey: sourceATA, IsWritable: true}
	accounts[6] = JupiterInstructionAccount{Pubkey: destinationATA, IsWritable: true}
	accounts[7] = JupiterInstructionAccount{Pubkey: sourceMint}
	accounts[8] = JupiterInstructionAccount{Pubkey: destinationMint}
	if v2 {
		data = make([]byte, 47)
		copy(data, jupiterSharedAccountsRouteV2)
		accounts[1] = JupiterInstructionAccount{Pubkey: bridgeVault, IsSigner: true}
		accounts[2] = JupiterInstructionAccount{Pubkey: sourceATA, IsWritable: true}
		accounts[5] = JupiterInstructionAccount{Pubkey: destinationATA, IsWritable: true}
		accounts[6] = JupiterInstructionAccount{Pubkey: sourceMint}
		accounts[7] = JupiterInstructionAccount{Pubkey: destinationMint}
		accounts[8] = JupiterInstructionAccount{Pubkey: bridgeTokenProgram}
		accounts[9] = JupiterInstructionAccount{Pubkey: bridgeTokenProgram}
		data[25], data[26], data[27] = 50, 0, 0
	}
	for index := 0; index < 8; index++ {
		data[len(data)-19+index] = byte(amount >> (8 * index))
		data[len(data)-11+index] = byte(out >> (8 * index))
	}
	if !v2 {
		data[len(data)-3], data[len(data)-2], data[len(data)-1] = 50, 0, 0
	}
	return JupiterSwapInstruction{ProgramID: jupiterV6Program, Accounts: accounts, Data: base64.StdEncoding.EncodeToString(data)}
}

func TestJupiterBuilderPinsBothExactEdgesAndPacketBoundary(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, test := range []struct {
		action     Action
		constraint byte
	}{{SwapUSDCToPrimeStep, 0}, {SwapPrimeToUSDCStep, 1}} {
		request := JupiterSwapRequest{Action: test.action, AmountRaw: 1_000_000, QuotedOutputRaw: 990_000, MinimumOutputRaw: 985_050,
			Policy: "Fks3YBQWBYA1d6ZZKEAEunjhVMXZA9gY7vfWUWWbQtDx", PolicyAccountDataSHA256: "6cdf12f0cd4623d60b32dc6d58b655e1fcbddf82ae7f75cd7b12783087b9ecc7", PolicyConstraintIndex: test.constraint,
			Instruction: jupiterTestInstruction(test.action, 1_000_000, 990_000, false), RecentBlockhash: bridgeSettings, LastValidBlockHeight: 2}
		signed, err := buildAndSignJupiterTransactionForDelegate(request, key, delegate)
		if err != nil {
			t.Fatal(err)
		}
		if len(signed.signedWire) > solanaPacketBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:65]) {
			t.Fatalf("%s wire is not a signed bounded packet", test.action)
		}
		if _, err := signed.BuildResult(9); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJupiterValidatorAcceptsOnlySharedDialectsAndExactCustodies(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99, v2)
		if _, err := validateJupiterInstruction(instruction, SwapUSDCToPrimeStep, 100, 99, 98); err != nil {
			t.Fatal(err)
		}
		instruction.Accounts[3].Pubkey = previousBackyardVault
		if _, err := validateJupiterInstruction(instruction, SwapUSDCToPrimeStep, 100, 99, 98); err == nil {
			t.Fatal("accepted drifted/prior custody")
		}
	}
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99, false)
	data, _ := base64.StdEncoding.DecodeString(instruction.Data)
	data[0] ^= 1
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	if _, err := validateJupiterInstruction(instruction, SwapUSDCToPrimeStep, 100, 99, 98); err == nil {
		t.Fatal("accepted arbitrary Jupiter dialect")
	}
}

func TestJupiterFreshSwapIsBoundedAndRejectsCompanionInstructions(t *testing.T) {
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99, false)
	companion := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			if r.URL.Query().Get("maxAccounts") != "32" || r.URL.Query().Get("slippageBps") != "50" {
				t.Error("quote bounds drifted")
			}
			response = `{"inputMint":"` + bridgeUSDC + `","outputMint":"` + kaminoPrimeMint + `","inAmount":"100","outAmount":"99","otherAmountThreshold":"98","swapMode":"ExactIn","slippageBps":50,"platformFee":null,"routePlan":[{}]}`
		case "/swap-instructions":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["userPublicKey"] != bridgeVault || request["wrapAndUnwrapSol"] != false || request["useSharedAccounts"] != true || request["dynamicComputeUnitLimit"] != false {
				t.Error("swap request boundary drifted")
			}
			body, _ := json.Marshal(map[string]any{"setupInstructions": func() []any {
				if companion {
					return []any{map[string]any{"programId": jupiterV6Program}}
				}
				return []any{}
			}(), "otherInstructions": []any{}, "cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction})
			response = string(body)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Timeout: time.Second, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.FreshSwap(context.Background(), SwapUSDCToPrimeStep, 100); err != nil {
		t.Fatal(err)
	}
	companion = true
	if _, _, err := client.FreshSwap(context.Background(), SwapUSDCToPrimeStep, 100); err == nil {
		t.Fatal("accepted a setup instruction outside the policy contract")
	}
}

func TestJupiterAcceptsCanonicalSystemProgramAccount(t *testing.T) {
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 95, false)
	instruction.Accounts = append(instruction.Accounts, JupiterInstructionAccount{
		Pubkey: "11111111111111111111111111111111",
	})
	if _, err := validateJupiterInstruction(instruction, SwapUSDCToPrimeStep, 100, 95, 94); err != nil {
		t.Fatal(err)
	}
	key, err := decodeKey("11111111111111111111111111111111")
	if err != nil || key != (publicKey{}) {
		t.Fatalf("system program decoded incorrectly: key=%x err=%v", key, err)
	}
}
