package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

var fixtureJupiterHTTP sync.Map // *jupiter.Client -> *http.Client

// fixtureJupiter binds a fixture transport; fixtureHTTP returns the HTTP
// client it holds so a test can wrap that transport.
func fixtureJupiter(transport http.RoundTripper) (*jupiter.Client, error) {
	httpClient := &http.Client{Transport: transport}
	client, err := jupiter.NewClient("https://jupiter.invalid", "", httpClient)
	fixtureJupiterHTTP.Store(client, httpClient)
	return client, err
}

func fixtureHTTP(client *jupiter.Client) *http.Client {
	httpClient, _ := fixtureJupiterHTTP.Load(client)
	return httpClient.(*http.Client)
}

// jupiterTestInstruction is the PRIME/USDC route's swap of action in the
// shape the swap API returns (v2TestInstruction).
func jupiterTestInstruction(action Action, amount, out uint64) JupiterSwapInstruction {
	return v2TestInstruction(RouteID, action, amount, out, 0)
}

// v2TestInstruction is lane's shared_accounts_route_v2 of action as the swap
// API shapes it: the twelve fixed accounts through program authority 0, then
// filler venue accounts, and one Manifest step (primeForwardPlan) at the
// worker's slippage bound.
func v2TestInstruction(lane string, action Action, amount, out uint64, filler int) JupiterSwapInstruction {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(action, lane)
	if err != nil {
		panic(err)
	}
	program := func(mint string) solana.PublicKey {
		a, err := routeSwapAssets()
		if err != nil {
			panic(err)
		}
		for _, asset := range []swapAsset{a.usdc, a.prime, a.usds, a.pyusd, a.auto, a.usde, a.syrup, a.onyc, a.usdg} {
			if asset.mint.String() == mint {
				return asset.program
			}
		}
		return solana.TokenProgramID // the basic lanes' collaterals are classic
	}
	authorityAccount := func(mint string) string {
		return jupiter.AuthorityTokenAccounts(1, solana.MustPublicKeyFromBase58(mint), program(mint))[0].String()
	}
	accounts := []jupiter.AccountMeta{{Pubkey: jupiter.ProgramAuthority(0).String()}, {Pubkey: bridgeVault, IsSigner: true},
		{Pubkey: sourceATA, IsWritable: true}, {Pubkey: authorityAccount(sourceMint), IsWritable: true},
		{Pubkey: authorityAccount(destinationMint), IsWritable: true}, {Pubkey: destinationATA, IsWritable: true},
		{Pubkey: sourceMint}, {Pubkey: destinationMint}, {Pubkey: program(sourceMint).String()}, {Pubkey: program(destinationMint).String()},
		{Pubkey: jupiter.EventAuthority.String()}, {Pubkey: jupiter.ProgramID.String()}}
	for index := range filler {
		accounts = append(accounts, jupiter.AccountMeta{Pubkey: autoVenueKey(byte(index))})
	}
	data := append([]byte(nil), jupiter.SharedAccountsRouteV2Discriminator[:]...)
	data = append(binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(append(data, 0), amount), out), 50, 0, 0, 0, 0, 0)
	data = append(data, primeForwardPlan...)
	return JupiterSwapInstruction{ProgramID: jupiter.ProgramID.String(), Accounts: accounts, Data: base64.StdEncoding.EncodeToString(data)}
}

func TestJupiterBuilderPinsBothExactEdgesAndPacketBoundary(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, test := range []struct {
		action Action
		policy string
	}{
		{SwapUSDCToPrimeStep, "FZjjJScy689WWSwhwr2HZPy2aevZukq75niD6gW3b1TG"},
		{SwapPrimeToUSDCStep, "Fks3YBQWBYA1d6ZZKEAEunjhVMXZA9gY7vfWUWWbQtDx"},
	} {
		request := JupiterSwapRequest{Action: test.action, AmountRaw: 1_000_000, QuotedOutputRaw: 990_000, MinimumOutputRaw: 985_050,
			Policy:      test.policy,
			Instruction: jupiterTestInstruction(test.action, 1_000_000, 990_000), RecentBlockhash: bridgeSettings, LastValidBlockHeight: 2}
		signed, err := buildAndSignJupiterTransactionForDelegate(request, key, delegate)
		if err != nil {
			t.Fatal(err)
		}
		unsigned, err := compileJupiterMessageForDelegate(request, delegate)
		if err != nil || !bytes.Equal(unsigned, signed.message) {
			t.Fatalf("unsigned Jupiter fee message differs: %v", err)
		}
		if len(signed.signedWire) > solanaPacketBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:65]) {
			t.Fatalf("%s wire is not a signed bounded packet", test.action)
		}
		result, err := signed.BuildResult(9)
		if err != nil {
			t.Fatal(err)
		}
		if err := result.validateForDelegate(delegate); err != nil {
			t.Fatalf("existing one-instruction Jupiter envelope was rejected: %v", err)
		}
	}
}

// The forward PRIME/USDC policy admits its one pinned route plan and no other.
func TestForwardJupiterPolicyAdmitsOnlyItsRoutePlan(t *testing.T) {
	forward, err := primeUSDCForwardPolicy()
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := validateJupiterInstructionForRoute(jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99), SwapUSDCToPrimeStep, 100, 99, 98, RouteID)
	if err != nil {
		t.Fatal(err)
	}
	if !squads.Admits(forward.Constraints[splitLeg], instruction.squads(), nil) {
		t.Fatal("the forward policy refuses its own route plan")
	}
	instruction.data[jupiter.V2RoutePlanOffset+4] = 117 // another venue
	if squads.Admits(forward.Constraints[splitLeg], instruction.squads(), nil) {
		t.Fatal("the forward policy admits another route plan")
	}
}

func TestJupiterValidatorAcceptsOnlySharedDialectsAndExactCustodies(t *testing.T) {
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99)
	if _, err := validateJupiterInstructionForRoute(instruction, SwapUSDCToPrimeStep, 100, 99, 98, RouteID); err != nil {
		t.Fatal(err)
	}
	instruction.Accounts[2].Pubkey = previousBackyardVault
	if _, err := validateJupiterInstructionForRoute(instruction, SwapUSDCToPrimeStep, 100, 99, 98, RouteID); err == nil {
		t.Fatal("accepted drifted/prior custody")
	}
	instruction = jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99)
	data, _ := base64.StdEncoding.DecodeString(instruction.Data)
	data[0] ^= 1
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	if _, err := validateJupiterInstructionForRoute(instruction, SwapUSDCToPrimeStep, 100, 99, 98, RouteID); err == nil {
		t.Fatal("accepted arbitrary Jupiter dialect")
	}
}

func TestJupiterFreshSwapIsBoundedAndRejectsCompanionInstructions(t *testing.T) {
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 99)
	companion := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			if r.URL.Query().Get("maxAccounts") != "32" || r.URL.Query().Get("slippageBps") != "50" || r.URL.Query().Get("instructionVersion") != "V2" {
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
					return []any{map[string]any{"programId": jupiter.ProgramID.String()}}
				}
				return []any{}
			}(), "otherInstructions": []any{}, "cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction})
			response = string(body)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	client, err := jupiter.NewClient("https://jupiter.invalid", "", &http.Client{Timeout: time.Second, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := freshSwapForRoute(context.Background(), client, RouteID, SwapUSDCToPrimeStep, 100); err != nil {
		t.Fatal(err)
	}
	companion = true
	if _, _, err := freshSwapForRoute(context.Background(), client, RouteID, SwapUSDCToPrimeStep, 100); err == nil {
		t.Fatal("accepted a setup instruction outside the policy contract")
	}
}

func TestJupiterAcceptsCanonicalSystemProgramAccount(t *testing.T) {
	instruction := jupiterTestInstruction(SwapUSDCToPrimeStep, 100, 95)
	instruction.Accounts = append(instruction.Accounts, jupiter.AccountMeta{
		Pubkey: "11111111111111111111111111111111",
	})
	if _, err := validateJupiterInstructionForRoute(instruction, SwapUSDCToPrimeStep, 100, 95, 94, RouteID); err != nil {
		t.Fatal(err)
	}
	key, err := decodeKey("11111111111111111111111111111111")
	if err != nil || key != (publicKey{}) {
		t.Fatalf("system program decoded incorrectly: key=%x err=%v", key, err)
	}
}
