package backyard

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func manifestAccounts(metas []accountMeta) KaminoPrimeUSDCAccounts {
	accounts := make(KaminoPrimeUSDCAccounts, len(metas))
	for index, value := range metas {
		accounts[index].Address = encodeBase58(value.key[:])
		accounts[index].Signer = value.signer
		accounts[index].Writable = value.writable
	}
	return accounts
}

// kaminoTestMetas is lane's SDK v2 account layout for leg.
func kaminoTestMetas(leg kaminoPrimeUSDCLeg, lane string) []accountMeta {
	route, err := runtimeRoute(lane)
	if err != nil {
		panic(err)
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	return map[kaminoPrimeUSDCLeg][]accountMeta{kaminoLegDeposit: deposit, kaminoLegBorrow: borrow, kaminoLegRepay: repay, kaminoLegWithdraw: withdraw}[leg]
}

func kaminoTestRequest(action Action, leg kaminoPrimeUSDCLeg) KaminoPrimeUSDCRequest {
	var discriminator []byte
	var metas []accountMeta
	switch leg {
	case kaminoLegDeposit:
		discriminator, metas = kamino.DepositV2Discriminator[:], kaminoTestMetas(kaminoLegDeposit, PhaseOneLaneID)
	case kaminoLegBorrow:
		discriminator, metas = kamino.BorrowV2Discriminator[:], kaminoTestMetas(kaminoLegBorrow, PhaseOneLaneID)
	case kaminoLegRepay:
		discriminator, metas = kamino.RepayV2Discriminator[:], kaminoTestMetas(kaminoLegRepay, PhaseOneLaneID)
	case kaminoLegWithdraw:
		discriminator, metas = kamino.WithdrawV2Discriminator[:], kaminoTestMetas(kaminoLegWithdraw, PhaseOneLaneID)
	default:
		panic("unknown test leg")
	}
	data := append([]byte(nil), discriminator...)
	data = appendU64(data, 1_000_000)
	return KaminoPrimeUSDCRequest{
		// Synthetic fixture address, frozen literal: independent of the Voltr
		// bridge policy rollover.
		Action: action, AmountRaw: 1_000_000, Policy: "8Nd646MD6H6hQrXZuP6utG5QZjRZ44GrRmdZJGShmhnh",
		Accounts: manifestAccounts(metas), Data: data, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99, RouteLane: PhaseOneLaneID,
	}
}

func TestSharedKaminoTokenProgramAndLaneBoundary(t *testing.T) {
	t.Parallel()
	route, err := runtimeRoute(PhaseOneLaneID)
	if err != nil {
		t.Fatal(err)
	}
	// Controlled layout variation, not an installed or executable new lane.
	route.CollateralTokenProgram, route.DebtTokenProgram = token2022Program, token2022Program
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	for _, m := range [][]accountMeta{deposit, withdraw} {
		if m[11].key != mustKey(classicTokenProgram) || m[12].key != mustKey(token2022Program) {
			t.Fatal("receipt and underlying token programs were conflated")
		}
	}
	if borrow[10].key != mustKey(token2022Program) || repay[7].key != mustKey(token2022Program) {
		t.Fatal("debt transfer used the wrong token program")
	}
	installed, err := runtimeRoute(PhaseOneLaneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matchesKaminoStepForResolvedRoute(OpenRouteStep, kamino.DepositV2Discriminator[:], deposit, installed); ok {
		t.Fatal("caller mutation changed an installed lane's token program")
	}
}

func TestKaminoPrimeUSDCBuilderPinsAllFourV2SDKLegsAndRefreshes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		action        Action
		leg           kaminoPrimeUSDCLeg
		count         int
		messageSHA256 string
		wireSHA256    string
		packetBytes   int
	}{
		{"deposit", OpenRouteStep, kaminoLegDeposit, 17, "0faa7b0899cb4c364463b6b1a2bad3e96a3d8a2df587f25e6f57082012e477ab", "96668df352391fa1dd9a33db96370d9274041377b576a3fbdb9099810d0df72d", 855},
		{"borrow", OpenRouteStep, kaminoLegBorrow, 15, "7cb728c8c1afd585fedb1d6c3e73503c40c4260fe60bd3b72380c208188cb262", "97b5b09f2b6b2a8df24ee1503ff207ea0bcd0fe9ba0aaa22f2e8a0d358769c07", 821},
		{"repay", DeleverRouteStep, kaminoLegRepay, 13, "4a695b9b2faffffaaf6ead209b3c35cdf8a595a75db6d63da64daf2d5013e0aa", "d96bd8c8105519881c0c3dbfe1980d02a4ac2393d9ddd51a5cf9a69f3618ffee", 787},
		{"withdraw", DeleverRouteStep, kaminoLegWithdraw, 17, "4fd91c12a98f1cc3a21737c8f0071eae965143d58e07a0c3329b4b928bacffb7", "b53482d3e6bb6e8f3f30a9f0a266d65337ed8e5b595466298a0ca44d290efe91", 856},
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := kaminoTestRequest(test.action, test.leg)
			inner, leg, err := kaminoPrimeUSDCInstruction(request)
			if err != nil {
				t.Fatal(err)
			}
			if leg != test.leg || inner.program != publicKey(kamino.ProgramID) || len(inner.accounts) != test.count {
				t.Fatal("did not build the pinned Kamino PRIME/USDC leg")
			}
			refresh := kaminoPrimeUSDCRefreshInstructionsForRoute(rwa, leg, PhaseOneLaneID)
			if len(refresh) != 3 || !bytes.Equal(refresh[0].data, kamino.RefreshReserveDiscriminator[:]) ||
				!bytes.Equal(refresh[1].data, kamino.RefreshReserveDiscriminator[:]) || !bytes.Equal(refresh[2].data, kamino.RefreshObligationDiscriminator[:]) {
				t.Fatal("canonical KLend refresh prefix drifted")
			}
			signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
			if err != nil {
				t.Fatal(err)
			}
			unsigned, err := compileKaminoMessageForDelegate(rwa, request, delegate)
			if err != nil || !bytes.Equal(unsigned, signed.message) {
				t.Fatalf("unsigned Kamino fee message differs: %v", err)
			}
			if len(signed.signedWire) > maxTransactionBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, wireSig(signed.signedWire)) {
				t.Fatal("Kamino exact wire signature or packet boundary is invalid")
			}
			if signed.messageSHA256 != test.messageSHA256 || signed.signedWireSHA256 != test.wireSHA256 || len(signed.signedWire) != test.packetBytes {
				t.Fatalf("Kamino fixture fingerprint drifted: message=%s wire=%s bytes=%d", signed.messageSHA256, signed.signedWireSHA256, len(signed.signedWire))
			}
			result, err := signed.BuildResult(123)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.validateForDelegate(delegate); err != nil {
				t.Fatalf("exact four-instruction Kamino wire was rejected before persistence: %v", err)
			}
		})
	}
}

func TestKaminoRefreshUsesConfirmedObligationTopologyForRedeposit(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	refresh := kaminoPrimeUSDCRefreshInstructionsForRequest(rwa, kaminoLegDeposit, request)
	if len(refresh) != 3 || len(refresh[2].accounts) != 4 ||
		encodeBase58(refresh[2].accounts[2].key[:]) != route.Kamino.CollateralReserve ||
		encodeBase58(refresh[2].accounts[3].key[:]) != route.Kamino.DebtReserve {
		t.Fatal("re-deposit refresh did not carry the confirmed collateral/debt reserve topology")
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signed.BuildResult(123)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatalf("confirmed-topology re-deposit wire was rejected: %v", err)
	}
	request.ObligationReserves = []string{route.Kamino.DebtReserve}
	if got := kaminoPrimeUSDCRefreshInstructionsForRequest(rwa, kaminoLegDeposit, request); got != nil {
		t.Fatal("non-canonical obligation reserve order was accepted")
	}
}

func TestKaminoWithdrawWireAcceptsDebtBearingObligationTopology(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), DeleverRouteStep, kaminoLegWithdraw, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signed.BuildResult(123)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatalf("debt-bearing collateral-withdraw wire was rejected: %v", err)
	}
}

func TestKaminoPrimeUSDCBuilderFailsClosedOnEveryAuthorityBoundary(t *testing.T) {
	t.Parallel()
	request := kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Accounts[2].Address = bridgeSettings
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lending market accepted")
	}
	request = kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Accounts[16].Address = kamino.ProgramID.String()
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong farms program accepted")
	}
	request = kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Accounts[9].Writable = false
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong custody role accepted")
	}
	request = kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Data[8]++
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("amount mutation accepted")
	}
	request = kaminoTestRequest(DeleverRouteStep, kaminoLegDeposit)
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("open deposit accepted as delever")
	}
	request = kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Data[0] = 129
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("historical non-v2 discriminator accepted")
	}
	request = kaminoTestRequest(OpenRouteStep, kaminoLegDeposit)
	request.Policy = ""
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	if _, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate); err == nil {
		t.Fatal("a request naming no policy was signed")
	}
}
