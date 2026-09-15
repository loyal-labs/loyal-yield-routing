package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"
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

func kaminoTestRequest(action Action, leg kaminoPrimeUSDCLeg) KaminoPrimeUSDCRequest {
	var discriminator []byte
	var metas []accountMeta
	switch leg {
	case kaminoLegDeposit:
		discriminator, metas = kaminoDepositCollateral, kaminoDepositMetas()
	case kaminoLegBorrow:
		discriminator, metas = kaminoBorrowUSDC, kaminoBorrowMetas()
	case kaminoLegRepay:
		discriminator, metas = kaminoRepayUSDC, kaminoRepayMetas()
	case kaminoLegWithdraw:
		discriminator, metas = kaminoWithdrawCollateral, kaminoWithdrawMetas()
	default:
		panic("unknown test leg")
	}
	data := append([]byte(nil), discriminator...)
	data = appendU64(data, 1_000_000)
	return KaminoPrimeUSDCRequest{
		Action: action, AmountRaw: 1_000_000, Policy: bridgeAllocationPolicy,
		PolicyConstraintIndex: 0, PolicyAccountDataSHA256: hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		Accounts: manifestAccounts(metas), Data: data, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
	}
}

func TestSharedKaminoTokenProgramAndLaneBoundary(t *testing.T) {
	route, err := runtimeRoute(RouteID)
	if err != nil {
		t.Fatal(err)
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	if !exactKaminoMetas(deposit, kaminoDepositMetas()) || !exactKaminoMetas(borrow, kaminoBorrowMetas()) || !exactKaminoMetas(repay, kaminoRepayMetas()) || !exactKaminoMetas(withdraw, kaminoWithdrawMetas()) {
		t.Fatal("shared layout changed the retained Prime account graph")
	}
	if _, ok := matchesKaminoStepForRoute(OpenPrimeUSDCStep, kaminoDepositCollateral, deposit, "uninstalled/lane"); ok {
		t.Fatal("unknown lane inherited the Prime account graph")
	}
	// Controlled layout variation, not an installed or executable new lane.
	route.CollateralTokenProgram, route.DebtTokenProgram = token2022Program, token2022Program
	deposit, borrow, repay, withdraw = kaminoMetasForRoute(route)
	for _, m := range [][]accountMeta{deposit, withdraw} {
		if m[11].key != mustKey(classicTokenProgram) || m[12].key != mustKey(token2022Program) {
			t.Fatal("receipt and underlying token programs were conflated")
		}
	}
	if borrow[10].key != mustKey(token2022Program) || repay[7].key != mustKey(token2022Program) {
		t.Fatal("debt transfer used the wrong token program")
	}
	if _, ok := matchesKaminoStepForRoute(OpenPrimeUSDCStep, kaminoDepositCollateral, deposit, RouteID); ok {
		t.Fatal("caller mutation changed an installed lane's token program")
	}
}

func TestKaminoPrimeUSDCBuilderPinsAllFourV2SDKLegsAndRefreshes(t *testing.T) {
	tests := []struct {
		name          string
		action        Action
		leg           kaminoPrimeUSDCLeg
		count         int
		messageSHA256 string
		wireSHA256    string
		packetBytes   int
	}{
		{"deposit", OpenPrimeUSDCStep, kaminoLegDeposit, 17, "8e3cd010c11a0dcb3ac892b97e820c07abb74462ab711115b12dadefc66d117b", "c84d63de9987d2acddc364c80dd17cfce7b38445faded2a83cec5a80ce71ed9e", 839},
		{"borrow", OpenPrimeUSDCStep, kaminoLegBorrow, 15, "d832174c74963a10bdda5b41d5cdb555bdd9ec0a94d21104118c1042d521905c", "12bab96e691d3466c066c3880fdee5ec4a98c13bb82a19e799c2c158a7154bd6", 805},
		{"repay", DeleverPrimeUSDCStep, kaminoLegRepay, 13, "fd3dc6d6026201813d85c140021c8a4c1426e7c36e2789f93f7ae898b040d06f", "b540a163ce17d978d5aa2af67bea39434e50448919e6bdba6f2afb013c736119", 771},
		{"withdraw", DeleverPrimeUSDCStep, kaminoLegWithdraw, 17, "1365481d948495951b194b769628197247270e302f567c858b2af6443cea8963", "c70fcf7d95a6f396d156f5890aaef093fd94cf3f8b74f51d149d6d88327e6107", 840},
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
			if leg != test.leg || inner.program != mustKey(kaminoPrimeUSDCProgram) || len(inner.accounts) != test.count {
				t.Fatal("did not build the pinned Kamino PRIME/USDC leg")
			}
			refresh := kaminoPrimeUSDCRefreshInstructions(leg)
			if len(refresh) != 3 || !bytes.Equal(refresh[0].data, kaminoRefreshReserve) ||
				!bytes.Equal(refresh[1].data, kaminoRefreshReserve) || !bytes.Equal(refresh[2].data, kaminoRefreshObligation) {
				t.Fatal("canonical KLend refresh prefix drifted")
			}
			signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
			if err != nil {
				t.Fatal(err)
			}
			unsigned, err := compileKaminoMessageForDelegate(request, delegate)
			if err != nil || !bytes.Equal(unsigned, signed.message) {
				t.Fatalf("unsigned Kamino fee message differs: %v", err)
			}
			if len(signed.signedWire) > solanaPacketBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:1+ed25519.SignatureSize]) {
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
	manifest := basicPolicyFixtureManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	refresh := kaminoPrimeUSDCRefreshInstructionsForRequest(kaminoLegDeposit, request)
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
	if got := kaminoPrimeUSDCRefreshInstructionsForRequest(kaminoLegDeposit, request); got != nil {
		t.Fatal("non-canonical obligation reserve order was accepted")
	}
}

func TestKaminoWithdrawWireAcceptsDebtBearingObligationTopology(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
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

type kaminoInstructionLayout struct {
	start, end, program, accounts, data int
}

func kaminoInstructionLayouts(t *testing.T, message []byte) (int, []kaminoInstructionLayout) {
	t.Helper()
	offset := 3
	accountCount, err := decodeShortVec(message, &offset)
	if err != nil || accountCount == 0 {
		t.Fatal("invalid fixture account vector")
	}
	offset += accountCount*32 + 32
	countOffset := offset
	instructionCount, err := decodeShortVec(message, &offset)
	if err != nil || instructionCount != 4 || offset != countOffset+1 {
		t.Fatal("fixture is not the exact four-instruction Kamino message")
	}
	layouts := make([]kaminoInstructionLayout, instructionCount)
	for index := range layouts {
		start := offset
		program := offset
		offset++
		accountCount := int(message[offset])
		offset++
		accounts := offset
		offset += accountCount
		dataLength, err := decodeShortVec(message, &offset)
		if err != nil || offset+dataLength > len(message) {
			t.Fatal("invalid fixture instruction")
		}
		data := offset
		offset += dataLength
		layouts[index] = kaminoInstructionLayout{start: start, end: offset, program: program, accounts: accounts, data: data}
	}
	if offset != len(message) {
		t.Fatal("fixture has trailing bytes")
	}
	return countOffset, layouts
}

func kaminoResultForMessage(message []byte, key ed25519.PrivateKey) BuildResult {
	signature := ed25519.Sign(key, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	messageHash := sha256.Sum256(message)
	wireHash := sha256.Sum256(wire)
	return BuildResult{
		MessageSHA256: hex.EncodeToString(messageHash[:]), SignedWire: wire,
		SignedWireSHA256: hex.EncodeToString(wireHash[:]), TransactionSignature: encodeBase58(signature),
		RecentBlockhash: bridgeVault, LastValidBlockHeight: 99, SimulationSlot: 123,
	}
}

func TestPersistedKaminoWireRejectsMutatedEnvelope(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(
		kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit), key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	countOffset, layouts := kaminoInstructionLayouts(t, signed.message)
	clone := func() []byte { return append([]byte(nil), signed.message...) }

	wrongCount := clone()
	wrongCount[countOffset] = 3
	wrongOrder := append([]byte(nil), signed.message[:layouts[0].start]...)
	wrongOrder = append(wrongOrder, signed.message[layouts[1].start:layouts[1].end]...)
	wrongOrder = append(wrongOrder, signed.message[layouts[0].start:layouts[0].end]...)
	wrongOrder = append(wrongOrder, signed.message[layouts[1].end:]...)
	wrongProgram := clone()
	wrongProgram[layouts[0].program] = wrongProgram[layouts[3].program]
	wrongData := clone()
	wrongData[layouts[0].data] ^= 1
	wrongAccount := clone()
	wrongAccount[layouts[0].accounts] = 0
	wrongOuterByte := clone()
	wrongOuterByte[layouts[3].data+8] ^= 1
	wrongOuterAccount := clone()
	wrongOuterAccount[layouts[3].accounts+1] = 0
	trailingTransactionByte := append(clone(), 0)

	for name, message := range map[string][]byte{
		"count": wrongCount, "order": wrongOrder, "program": wrongProgram,
		"data": wrongData, "accounts": wrongAccount, "outer-byte": wrongOuterByte,
		"outer-account": wrongOuterAccount, "transaction-suffix": trailingTransactionByte,
	} {
		t.Run(name, func(t *testing.T) {
			if err := kaminoResultForMessage(message, key).validateForDelegate(delegate); err == nil {
				t.Fatal("mutated Kamino envelope passed persisted-wire validation")
			}
		})
	}

	depositRefresh := kaminoPrimeUSDCRefreshInstructions(kaminoLegDeposit)
	borrowRequest := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegBorrow)
	borrowInner, _, err := kaminoPrimeUSDCInstruction(borrowRequest)
	if err != nil {
		t.Fatal(err)
	}
	borrowOuter, err := wrapSquadsKaminoPolicy(mustKey(borrowRequest.Policy), delegate, delegate,
		borrowRequest.PolicyConstraintIndex, borrowInner)
	if err != nil {
		t.Fatal(err)
	}
	crossLegMessage, err := compileKaminoLegacyMessage(delegate, mustKey(bridgeVault),
		append(depositRefresh, borrowOuter))
	if err != nil {
		t.Fatal(err)
	}
	if err := kaminoResultForMessage(crossLegMessage, key).validateForDelegate(delegate); err == nil {
		t.Fatal("borrow inner instruction was accepted with the deposit refresh prefix")
	}

	depositRequest := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	depositInner, _, err := kaminoPrimeUSDCInstruction(depositRequest)
	if err != nil {
		t.Fatal(err)
	}
	depositOuter, err := wrapSquadsKaminoPolicy(mustKey(depositRequest.Policy), delegate, delegate,
		depositRequest.PolicyConstraintIndex, depositInner)
	if err != nil {
		t.Fatal(err)
	}
	depositOuter.data = append(depositOuter.data, 0)
	outerSuffixMessage, err := compileKaminoLegacyMessage(delegate, mustKey(bridgeVault),
		append(kaminoPrimeUSDCRefreshInstructions(kaminoLegDeposit), depositOuter))
	if err != nil {
		t.Fatal(err)
	}
	if err := kaminoResultForMessage(outerSuffixMessage, key).validateForDelegate(delegate); err == nil {
		t.Fatal("arbitrary Squads payload suffix passed persisted-wire validation")
	}
}

func TestKaminoPrimeUSDCBuilderFailsClosedOnEveryAuthorityBoundary(t *testing.T) {
	request := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[2].Address = bridgeSettings
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lending market accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[16].Address = kaminoPrimeUSDCProgram
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong farms program accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[9].Writable = false
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong custody role accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.PolicyConstraintIndex = 2
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lane constraint index accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Data[8]++
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("amount mutation accepted")
	}
	request = kaminoTestRequest(DeleverPrimeUSDCStep, kaminoLegDeposit)
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("open deposit accepted as delever")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Data[0] = 129
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("historical non-v2 discriminator accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.PolicyAccountDataSHA256 = "not-a-hash"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	if _, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate); err == nil {
		t.Fatal("unbound policy bytes accepted")
	}
}
