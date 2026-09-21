package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

type resourceTestInstruction struct {
	program  string
	accounts []string
	data     []byte
}

// decodeResourceTestMessage reads a compiled legacy message back into its key
// list and full instruction list, failing on any truncation or trailing bytes.
func decodeResourceTestMessage(t *testing.T, message []byte) ([]string, []resourceTestInstruction) {
	t.Helper()
	if len(message) < 4 || message[0] != 1 {
		t.Fatalf("not a legacy message: %d bytes", len(message))
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count == 0 || offset+count*32+32 > len(message) {
		t.Fatalf("legacy message truncates its keys: %v", err)
	}
	keys := make([]string, 0, count)
	for index := 0; index < count; index++ {
		keys = append(keys, encodeBase58(message[offset:offset+32]))
		offset += 32
	}
	offset += 32 // recent blockhash
	instructions, err := decodeShortVec(message, &offset)
	if err != nil {
		t.Fatalf("legacy message truncates its instruction count: %v", err)
	}
	out := make([]resourceTestInstruction, 0, instructions)
	for index := 0; index < instructions; index++ {
		if offset >= len(message) {
			t.Fatalf("legacy message truncates instruction %d", index)
		}
		programIndex := int(message[offset])
		offset++
		if programIndex >= len(keys) {
			t.Fatalf("instruction %d program index %d out of range", index, programIndex)
		}
		accountCount, err := decodeShortVec(message, &offset)
		if err != nil {
			t.Fatal(err)
		}
		accounts := make([]string, 0, accountCount)
		for account := 0; account < accountCount; account++ {
			if offset >= len(message) {
				t.Fatalf("instruction %d truncates its account list", index)
			}
			accountIndex := int(message[offset])
			offset++
			if accountIndex >= len(keys) {
				t.Fatalf("instruction %d account index %d out of range", index, accountIndex)
			}
			accounts = append(accounts, keys[accountIndex])
		}
		dataLength, err := decodeShortVec(message, &offset)
		if err != nil || offset+dataLength > len(message) {
			t.Fatalf("instruction %d truncates its data: %v", index, err)
		}
		out = append(out, resourceTestInstruction{program: keys[programIndex], accounts: accounts,
			data: append([]byte(nil), message[offset:offset+dataLength]...)})
		offset += dataLength
	}
	if offset != len(message) {
		t.Fatalf("legacy message carries %d trailing bytes", len(message)-offset)
	}
	return keys, out
}

func resourceInstructionIsCanonicalHeap(t *testing.T, instruction resourceTestInstruction) bool {
	t.Helper()
	key, err := decodeKey(instruction.program)
	if err != nil {
		t.Fatalf("instruction program %s does not decode: %v", instruction.program, err)
	}
	return isAutoExecutionHeapInstruction(compiledInstruction{program: key, data: instruction.data})
}

// The SDK identity bytes are pinned independently of the Go literal: base58
// validity alone never proves identity. Several 1s-counts also decode to 32
// bytes, and the first draft of this constant decoded to 33 bytes and would
// have panicked; these bytes come from solana-sdk-ids-3.1.0 compute_budget
// declare_id! and @solana/web3.js src/programs/compute-budget.ts programId.
const computeBudgetProgramSDKBytesHex = "0306466fe5211732ffecadba72c39be7bc8ce5bbc5f7126b2c439b3a40000000"

func TestComputeBudgetProgramIdentityMatchesTheSDK(t *testing.T) {
	key, err := decodeKey(computeBudgetProgram)
	if err != nil {
		t.Fatalf("ComputeBudget program literal does not decode: %v", err)
	}
	sdk, err := hex.DecodeString(computeBudgetProgramSDKBytesHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key[:], sdk) {
		t.Fatalf("ComputeBudget literal decodes to %x, but the SDK identity is %x", key[:], sdk)
	}
	if encodeBase58(key[:]) != computeBudgetProgram {
		t.Fatalf("ComputeBudget literal is not the canonical base58 form of the SDK identity: %s", encodeBase58(key[:]))
	}
}

func TestAutoExecutionHeapInstructionIsCanonical(t *testing.T) {
	heap := autoComputeBudgetHeapInstruction()
	if heap.program != mustKey(computeBudgetProgram) {
		t.Fatal("heap instruction program drifted from the ComputeBudget identity")
	}
	if len(heap.accounts) != 0 {
		t.Fatalf("heap instruction carries %d accounts", len(heap.accounts))
	}
	// requestHeapFrame: discriminator byte 1, then 65536 as little-endian u32.
	if !bytesEqual(heap.data, []byte{1, 0, 0, 1, 0}) {
		t.Fatalf("heap instruction data drifted: %v", heap.data)
	}
	if !isAutoExecutionHeapInstruction(heap) {
		t.Fatal("canonical heap instruction failed its own check")
	}
}

func TestAutoExecutionHeapInstructionRejectsDrift(t *testing.T) {
	canonical := autoComputeBudgetHeapInstruction()
	drifts := map[string]compiledInstruction{
		"wrong discriminator": {program: canonical.program, data: []byte{2, 0, 0, 1, 0}},
		"wrong heap size":     {program: canonical.program, data: []byte{1, 0, 0, 4, 0}},
		"short data":          {program: canonical.program, data: []byte{1, 0, 0, 1}},
		"extra byte":          {program: canonical.program, data: []byte{1, 0, 0, 1, 0, 0}},
		"wrong program":       {program: mustKey(bridgeSquadsProgram), data: []byte{1, 0, 0, 1, 0}},
		"with account":        {program: canonical.program, accounts: []accountMeta{meta(bridgeVault, false, false)}, data: []byte{1, 0, 0, 1, 0}},
	}
	for name, drifted := range drifts {
		if isAutoExecutionHeapInstruction(drifted) {
			t.Fatalf("%s passed as the canonical heap frame", name)
		}
	}
}

// TestLegacyPublicCompilerGatesArePreserved pins the granted fifth encoder slot
// behind the closed AUTO wrappers: the bridge compiler still admits exactly one
// payload instruction, the installed Kamino compiler still admits exactly the
// refresh-plus-policy four, and the AUTO legacy wrapper carries the canonical
// frame ahead of exactly one payload.
func TestLegacyPublicCompilerGatesArePreserved(t *testing.T) {
	delegate, hash := mustKey(bridgeDelegate), mustKey(bridgeSettings)
	outer := compiledInstruction{program: mustKey(bridgeSquadsProgram), data: append([]byte(nil), squadsExecuteSyncDiscriminator...)}
	if _, err := compileLegacyMessage(delegate, hash, nil); err == nil {
		t.Fatal("bridge compiler admitted an empty instruction list")
	}
	if _, err := compileLegacyMessage(delegate, hash, []compiledInstruction{outer, autoComputeBudgetHeapInstruction()}); err == nil {
		t.Fatal("bridge compiler admitted two instructions")
	}
	if _, err := compileKaminoLegacyMessage(delegate, hash, make([]compiledInstruction, 5)); err == nil {
		t.Fatal("installed Kamino compiler admitted a five-instruction list")
	}
	if _, err := compileAutoKaminoLegacyMessage(delegate, hash, make([]compiledInstruction, 5)); err == nil {
		t.Fatal("AUTO Kamino compiler admitted an unvalidated five-instruction list")
	}
	message, err := compileAutoResourceLegacyMessage(delegate, hash, outer)
	if err != nil {
		t.Fatal(err)
	}
	keys, instructions := decodeResourceTestMessage(t, message)
	if len(instructions) != 2 || !resourceInstructionIsCanonicalHeap(t, instructions[0]) || instructions[1].program != bridgeSquadsProgram {
		t.Fatalf("AUTO legacy wrapper wire drifted: %d instructions, keys %v", len(instructions), keys)
	}
	if _, err := compileAutoResourceLegacyMessage(delegate, hash, compiledInstruction{}); err == nil {
		t.Fatal("AUTO legacy wrapper admitted an empty payload")
	}
}

// TestAutoKaminoExecutionMessageCarriesTheReviewedHeapFrame compiles a real
// candidate lifecycle leg through the manifest compiler and reads the wire
// back: legacy envelope, canonical heap frame first, then the exact
// refresh-plus-policy payload, still inside the 1232-byte signed packet.
func TestAutoKaminoExecutionMessageCarriesTheReviewedHeapFrame(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegate := mustKey(bridgeDelegate)
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := manifest.compileKaminoMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if compiled[0] != 1 {
		t.Fatal("AUTO Kamino leg left the legacy envelope")
	}
	keys, instructions := decodeResourceTestMessage(t, compiled)
	if len(instructions) != 5 {
		t.Fatalf("AUTO Kamino leg carries %d instructions, want heap + exact refresh-plus-policy", len(instructions))
	}
	if !resourceInstructionIsCanonicalHeap(t, instructions[0]) {
		t.Fatalf("AUTO Kamino heap frame drifted: program %s data %v", instructions[0].program, instructions[0].data)
	}
	heapKeys := 0
	for _, key := range keys {
		heapKeys += map[bool]int{true: 1, false: 0}[key == computeBudgetProgram]
	}
	if heapKeys != 1 {
		t.Fatalf("AUTO Kamino message references ComputeBudget %d times", heapKeys)
	}
	for index := 1; index <= 3; index++ {
		if instructions[index].program != kaminoPrimeUSDCProgram {
			t.Fatalf("AUTO refresh %d program drifted: %s", index, instructions[index].program)
		}
	}
	if instructions[4].program != bridgeSquadsProgram {
		t.Fatalf("AUTO policy outer program drifted: %s", instructions[4].program)
	}
	if len(compiled)+65 > solanaPacketBytes {
		t.Fatalf("AUTO Kamino packet %d exceeds %d", len(compiled)+65, solanaPacketBytes)
	}
	t.Logf("AUTO Kamino execution message = %d bytes (+65 signature = %d of %d packet bytes)", len(compiled), len(compiled)+65, solanaPacketBytes)
	again, err := manifest.compileKaminoMessage(request, delegate)
	if err != nil || !bytes.Equal(compiled, again) {
		t.Fatal("AUTO Kamino compile is not deterministic")
	}
}

// TestAutoJupiterExecutionMessageCarriesTheReviewedHeapFrame pins both AUTO
// Jupiter transports: the legacy packet and the v0 lookup-table escape hatch
// both lead with the canonical heap frame, and the oversized edge stays
// fail-closed without hints.
func TestAutoJupiterExecutionMessageCarriesTheReviewedHeapFrame(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegate := mustKey(bridgeDelegate)
	request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	legacy, err := manifest.compileJupiterMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if legacy[0] != 1 {
		t.Fatal("AUTO Jupiter legacy leg left the legacy envelope")
	}
	_, instructions := decodeResourceTestMessage(t, legacy)
	if len(instructions) != 2 || !resourceInstructionIsCanonicalHeap(t, instructions[0]) || instructions[1].program != bridgeSquadsProgram {
		t.Fatalf("AUTO Jupiter legacy wire drifted: %d instructions", len(instructions))
	}
	t.Logf("AUTO Jupiter legacy execution message = %d bytes (+65 signature = %d of %d packet bytes)", len(legacy), len(legacy)+65, solanaPacketBytes)

	oversized := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-10)
	if _, err := manifest.compileJupiterMessage(oversized, delegate); err == nil || !strings.Contains(err.Error(), "unsigned message does not fit") {
		t.Fatalf("oversized hintless AUTO edge compiled: %v", err)
	}
	fillers := make([]string, 0, len(oversized.Instruction.Accounts))
	for index, account := range oversized.Instruction.Accounts {
		if index < 10 {
			continue // reviewed boundaries and authorities stay static
		}
		fillers = append(fillers, account.Pubkey)
	}
	table := autoFixtureLookupTable(t, fillers)
	oversized.Instruction.LookupTableAddresses = []string{table.Address}
	oversized.LookupTables = []LookupTableSnapshot{table}
	v0, err := manifest.compileJupiterMessage(oversized, delegate)
	if err != nil || v0[0] != 0x80 || v0[1] != 1 {
		t.Fatalf("AUTO v0 leg failed: %v", err)
	}
	if len(v0)+65 > solanaPacketBytes {
		t.Fatalf("AUTO v0 packet %d exceeds %d", len(v0)+65, solanaPacketBytes)
	}
	staticKeys, _, outerData := decodeV0OuterInstruction(t, v0)
	if !bytes.Equal(outerData[:8], squadsExecuteSyncDiscriminator) {
		t.Fatal("AUTO v0 outer left the Squads execute")
	}
	heapOnWire := false
	for _, key := range staticKeys {
		heapOnWire = heapOnWire || key == computeBudgetProgram
	}
	if !heapOnWire {
		t.Fatal("AUTO v0 message dropped the ComputeBudget heap frame")
	}
	t.Logf("AUTO Jupiter v0 execution message = %d bytes (+65 signature = %d of %d packet bytes)", len(v0), len(v0)+65, solanaPacketBytes)
}

// TestAutoInitializerExecutionMessageCarriesTheReviewedHeapFrame compiles the
// candidate initializer admission through the reviewed binding and reads the
// wire back; the public compiler above it keeps the exact installed
// selector-lane bytes with no resource instruction.
func TestAutoInitializerExecutionMessageCarriesTheReviewedHeapFrame(t *testing.T) {
	manifest, request := autoInitializerRequestFixture(t)
	message, err := manifest.compileKaminoInitializationMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	keys, instructions := decodeResourceTestMessage(t, message)
	if len(instructions) != 2 || !resourceInstructionIsCanonicalHeap(t, instructions[0]) || instructions[1].program != bridgeSquadsProgram {
		t.Fatalf("AUTO initializer wire drifted: %d instructions", len(instructions))
	}
	for _, key := range keys {
		if key == computeBudgetProgram && instructions[1].program != bridgeSquadsProgram {
			t.Fatal("AUTO initializer wire misordered the heap frame")
		}
	}
	t.Logf("AUTO initializer execution message = %d bytes (+65 signature = %d of %d packet bytes)", len(message), len(message)+65, solanaPacketBytes)

	// The public initializer compiler keeps the installed selector bytes: one
	// Squads instruction, no ComputeBudget key anywhere.
	installed := request
	installed.RouteLane = PhaseOneLaneID
	installed.PolicySeed = 141
	installed.PolicyAccountDataSHA256 = sha256Bytes([]byte("local candidate policy; hash does not enter wire"))
	public, err := CompileKaminoInitializationMessage(installed)
	if err != nil {
		t.Fatal(err)
	}
	publicKeys, publicInstructions := decodeResourceTestMessage(t, public)
	if len(publicInstructions) != 1 || publicInstructions[0].program != bridgeSquadsProgram {
		t.Fatalf("installed initializer wire drifted: %d instructions", len(publicInstructions))
	}
	for _, key := range publicKeys {
		if key == computeBudgetProgram {
			t.Fatal("installed initializer lane gained the ComputeBudget key")
		}
	}
}

// TestInstalledExecutionMessagesStayByteIdenticalWithoutResources pins Maple
// byte identity: the installed production compile path — the same chain the
// signer used before the resource work — still emits the exact
// four-instruction installed wire with no resource instruction, even though
// the candidate AUTO binding is present on the same manifest.
func TestInstalledExecutionMessagesStayByteIdenticalWithoutResources(t *testing.T) {
	request := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	compiled, err := compileKaminoMessageForDelegate(request, mustKey(bridgeDelegate))
	if err != nil {
		t.Fatal(err)
	}
	keys, instructions := decodeResourceTestMessage(t, compiled)
	if len(instructions) != 4 {
		t.Fatalf("installed Kamino leg carries %d instructions, want the exact refresh-plus-policy four", len(instructions))
	}
	for _, key := range keys {
		if key == computeBudgetProgram {
			t.Fatal("installed Kamino lane gained the ComputeBudget key")
		}
	}
	for index := 0; index <= 2; index++ {
		if instructions[index].program != kaminoPrimeUSDCProgram {
			t.Fatalf("installed refresh %d program drifted: %s", index, instructions[index].program)
		}
	}
	if instructions[3].program != bridgeSquadsProgram {
		t.Fatalf("installed policy outer program drifted: %s", instructions[3].program)
	}
	t.Logf("installed Kamino execution message = %d bytes (+65 signature = %d of %d packet bytes)", len(compiled), len(compiled)+65, solanaPacketBytes)
}

// legacyInstructionSpan returns the byte span of one instruction in a compiled
// legacy message, plus the offset of the instruction-count shortvec, so
// mutation tests can operate on the exact wire the compiler emitted.
func legacyInstructionSpan(t *testing.T, message []byte, index int) (countOffset, start, end int) {
	t.Helper()
	if len(message) < 4 || message[0] != 1 {
		t.Fatalf("not a legacy message: %d bytes", len(message))
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count == 0 || offset+count*32+32 > len(message) {
		t.Fatalf("legacy message truncates its keys: %v", err)
	}
	offset += count*32 + 32
	countOffset = offset
	instructions, err := decodeShortVec(message, &offset)
	if err != nil || index >= instructions {
		t.Fatalf("legacy message truncates its instruction count: %v", err)
	}
	for position := 0; position <= index; position++ {
		start = offset
		offset++ // program index
		accountCount, err := decodeShortVec(message, &offset)
		if err != nil || offset+accountCount > len(message) {
			t.Fatalf("instruction %d truncates its accounts: %v", position, err)
		}
		offset += accountCount
		dataLength, err := decodeShortVec(message, &offset)
		if err != nil || offset+dataLength > len(message) {
			t.Fatalf("instruction %d truncates its data: %v", position, err)
		}
		offset += dataLength
		end = offset
	}
	return countOffset, start, end
}

func replaceSpan(src []byte, start, end int, insertion []byte) []byte {
	out := append([]byte(nil), src[:start]...)
	out = append(out, insertion...)
	return append(out, src[end:]...)
}

// signTestWire assembles the exact one-signature wire a delegate would
// broadcast, keyed by a generated test key — never a production signer.
func signTestWire(t *testing.T, key []byte, message []byte) []byte {
	t.Helper()
	wire := append(encodeShortVec(1), ed25519.Sign(key, message)...)
	return append(wire, message...)
}

// TestAutoSignedWireValidatesThroughTheDecodeGate signs real AUTO execution
// messages with a generated test key and drives the persisted-wire decoder:
// the resource envelope validates with the heap frame first and alone, and
// every mutation — stripped, reordered, duplicated, non-canonical, or an
// arbitrary compute instruction — fails closed.
func TestAutoSignedWireValidatesThroughTheDecodeGate(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{31}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(delegateKey.Public().(ed25519.PublicKey))

	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	message, err := manifest.compileKaminoMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	countOffset, heapStart, heapEnd := legacyInstructionSpan(t, message, 0)
	heapFrame := append([]byte(nil), message[heapStart:heapEnd]...)
	if len(heapFrame) != 8 { // program index, zero accounts, shortvec 5, five data bytes
		t.Fatalf("heap frame layout drifted: %d bytes", len(heapFrame))
	}

	signature, decoded, recentBlockhash, signer, err := decodeExactLegacyWire(signTestWire(t, delegateKey, message))
	if err != nil || signer != delegate || !ed25519.Verify(signer[:], decoded, signature) ||
		encodeBase58(recentBlockhash[:]) != bridgeSettings {
		t.Fatalf("resource-carrying AUTO Kamino wire rejected: %v signer %s", err, signer)
	}

	mutations := map[string][]byte{
		"stripped heap": func() []byte {
			stripped := replaceSpan(message, heapStart, heapEnd, nil)
			stripped[countOffset]--
			return stripped
		}(),
		"reordered heap to the end": func() []byte {
			moved := replaceSpan(message, heapStart, heapEnd, nil)
			moved[countOffset]--
			return append(moved, heapFrame...)
		}(),
		"duplicated heap": func() []byte {
			duplicated := replaceSpan(message, heapEnd, 0, heapFrame)
			duplicated[countOffset]++
			return duplicated
		}(),
		"non-canonical heap size": replaceSpan(message, heapStart, heapEnd,
			[]byte{heapFrame[0], heapFrame[1], heapFrame[2], 1, 0, 0, 4, 0}),
		"arbitrary compute instruction": replaceSpan(message, heapStart, heapEnd,
			[]byte{heapFrame[0], heapFrame[1], heapFrame[2], 2, 0, 0, 0, 0}),
	}
	for name, mutated := range mutations {
		if _, _, _, _, err := decodeExactLegacyWire(signTestWire(t, delegateKey, mutated)); err == nil {
			t.Fatalf("%s passed the persisted-wire decode gate", name)
		}
	}

	// Three instructions is still an unsupported count.
	_, refreshStart, refreshEnd := legacyInstructionSpan(t, message, 1)
	three := replaceSpan(replaceSpan(message, heapStart, heapEnd, nil), refreshStart-(heapEnd-heapStart), refreshEnd-(heapEnd-heapStart), nil)
	three[countOffset] -= 2
	if _, _, _, _, err := decodeExactLegacyWire(signTestWire(t, delegateKey, three)); err == nil || !strings.Contains(err.Error(), "unsupported instruction count") {
		t.Fatalf("three-instruction wire: %v", err)
	}

	// The initializer and AUTO Jupiter legacy legs decode the same way; the
	// initializer wire stripped of its resource degrades exactly to the
	// installed single-Squads-instruction shape, whose old gate stays open.
	initializerManifest, initializer := autoInitializerRequestFixture(t)
	initializerMessage, err := initializerManifest.compileKaminoInitializationMessage(initializer)
	if err != nil {
		t.Fatal(err)
	}
	// The initializer compiler pins the fixed delegate payer identity, so the
	// decoded signer is bridgeDelegate even though the test key signed the wire.
	if _, _, _, signer, err := decodeExactLegacyWire(signTestWire(t, delegateKey, initializerMessage)); err != nil || signer != mustKey(bridgeDelegate) {
		t.Fatalf("AUTO initializer wire rejected: %v", err)
	}
	initCountOffset, initStart, initEnd := legacyInstructionSpan(t, initializerMessage, 0)
	strippedInitializer := replaceSpan(initializerMessage, initStart, initEnd, nil)
	strippedInitializer[initCountOffset]--
	if _, _, _, _, err := decodeExactLegacyWire(signTestWire(t, delegateKey, strippedInitializer)); err != nil {
		t.Fatalf("stripped initializer wire is still an exact installed shape: %v", err)
	}

	jupiter := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	jupiterMessage, err := manifest.compileJupiterMessage(jupiter, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, signer, err := decodeExactLegacyWire(signTestWire(t, delegateKey, jupiterMessage)); err != nil || signer != delegate {
		t.Fatalf("AUTO Jupiter legacy wire rejected: %v", err)
	}
	jupiterCountOffset, jupStart, jupEnd := legacyInstructionSpan(t, jupiterMessage, 0)
	jupiterFrame := append([]byte(nil), jupiterMessage[jupStart:jupEnd]...)
	duplicatedJupiter := replaceSpan(jupiterMessage, jupEnd, 0, jupiterFrame)
	duplicatedJupiter[jupiterCountOffset]++
	if _, _, _, _, err := decodeExactLegacyWire(signTestWire(t, delegateKey, duplicatedJupiter)); err == nil {
		t.Fatal("duplicated heap passed the Jupiter legacy decode gate")
	}
}

// TestAutoVersionedWireValidatesThroughTheDecodeGate drives the narrow v0
// decoder over the oversized AUTO escape hatch: the canonical heap frame ahead
// of the static Squads outer validates, the installed single-outer shape stays
// admitted, and duplicated or drifted resource frames fail closed.
func TestAutoVersionedWireValidatesThroughTheDecodeGate(t *testing.T) {
	delegateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{32}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(delegateKey.Public().(ed25519.PublicKey))
	request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-10)
	inner, err := validateJupiterInstructionForRoute(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := wrapSquadsJupiterPolicy(mustKey(request.Policy), delegate, delegate, request.PolicyConstraintIndex, inner)
	if err != nil {
		t.Fatal(err)
	}
	fillers := make([]string, 0, len(request.Instruction.Accounts))
	for index, account := range request.Instruction.Accounts {
		if index < 10 {
			continue
		}
		fillers = append(fillers, account.Pubkey)
	}
	table := autoFixtureLookupTable(t, fillers)
	blockhash := mustKey(bridgeSettings)

	message, err := compileV0Message(delegate, blockhash, withAutoExecutionHeap([]compiledInstruction{outer}), []LookupTableSnapshot{table})
	if err != nil {
		t.Fatal(err)
	}
	signature, decoded, recentBlockhash, signer, err := decodeExactV0Wire(signTestWire(t, delegateKey, message))
	if err != nil || signer != delegate || !ed25519.Verify(signer[:], decoded, signature) ||
		encodeBase58(recentBlockhash[:]) != bridgeSettings {
		t.Fatalf("resource-carrying AUTO v0 wire rejected: %v", err)
	}

	installed, err := compileV0Message(delegate, blockhash, []compiledInstruction{outer}, []LookupTableSnapshot{table})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := decodeExactV0Wire(signTestWire(t, delegateKey, installed)); err != nil {
		t.Fatalf("installed single-outer v0 wire rejected: %v", err)
	}

	heap := autoComputeBudgetHeapInstruction()
	drifted := heap
	drifted.data = []byte{1, 0, 0, 4, 0}
	for name, instructions := range map[string][]compiledInstruction{
		"duplicated heap":  {heap, heap, outer},
		"drifted heap":     {drifted, outer},
		"heap after outer": {outer, heap},
	} {
		mutated, err := compileV0Message(delegate, blockhash, instructions, []LookupTableSnapshot{table})
		if err != nil {
			t.Fatalf("%s fixture failed to compile: %v", name, err)
		}
		if _, _, _, _, err := decodeExactV0Wire(signTestWire(t, delegateKey, mutated)); err == nil {
			t.Fatalf("%s passed the versioned decode gate", name)
		}
	}
}

// signedTestBuildResult wraps a compiled AUTO message into the exact persisted
// evidence shape BuildResult.validateForDelegate checks, signed by the given
// generated test key.
func signedTestBuildResult(t *testing.T, key ed25519.PrivateKey, message []byte) BuildResult {
	t.Helper()
	wire := signTestWire(t, key, message)
	return BuildResult{
		SignedWire:           wire,
		SignedWireSHA256:     sha256Bytes(wire),
		MessageSHA256:        sha256Bytes(message),
		TransactionSignature: encodeBase58(ed25519.Sign(key, message)),
		RecentBlockhash:      bridgeSettings,
		LastValidBlockHeight: 99,
		SimulationSlot:       42,
	}
}

// TestAutoPersistedBuildResultValidationRoutesByWireVersion drives the actual
// persisted-evidence gate end to end for both encodings: the legacy and v0 AUTO
// wires validate with the delegate pin, every heap mutation fails closed, an
// unknown message version is rejected, and retained signature/hash checks still
// fire on both routes.
func TestAutoPersistedBuildResultValidationRoutesByWireVersion(t *testing.T) {
	manifest := autoFixtureManifest(t)
	delegateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{33}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(delegateKey.Public().(ed25519.PublicKey))

	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	legacyMessage, err := manifest.compileKaminoMessage(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	legacy := signedTestBuildResult(t, delegateKey, legacyMessage)
	if err := legacy.validateForDelegate(delegate); err != nil {
		t.Fatalf("legacy AUTO build result rejected: %v", err)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("delegate-less validation broke: %v", err)
	}
	outsiderKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{34}, ed25519.SeedSize))
	outsider := publicKeyFromBytes(outsiderKey.Public().(ed25519.PublicKey))
	if err := legacy.validateForDelegate(outsider); err == nil || !strings.Contains(err.Error(), "pinned delegated executor") {
		t.Fatalf("wrong delegate accepted: %v", err)
	}

	countOffset, heapStart, heapEnd := legacyInstructionSpan(t, legacyMessage, 0)
	stripped := replaceSpan(legacyMessage, heapStart, heapEnd, nil)
	stripped[countOffset]--
	strippedResult := signedTestBuildResult(t, delegateKey, stripped)
	if err := strippedResult.validateForDelegate(delegate); err == nil {
		t.Fatal("stripped legacy heap passed the persisted gate")
	}

	v0Request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-10)
	inner, err := validateJupiterInstructionForRoute(v0Request.Instruction, v0Request.Action, v0Request.AmountRaw, v0Request.QuotedOutputRaw, v0Request.MinimumOutputRaw, v0Request.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := wrapSquadsJupiterPolicy(mustKey(v0Request.Policy), delegate, delegate, v0Request.PolicyConstraintIndex, inner)
	if err != nil {
		t.Fatal(err)
	}
	fillers := make([]string, 0, len(v0Request.Instruction.Accounts))
	for index, account := range v0Request.Instruction.Accounts {
		if index < 10 {
			continue
		}
		fillers = append(fillers, account.Pubkey)
	}
	table := autoFixtureLookupTable(t, fillers)
	v0Message, err := compileV0Message(delegate, mustKey(bridgeSettings), withAutoExecutionHeap([]compiledInstruction{outer}), []LookupTableSnapshot{table})
	if err != nil {
		t.Fatal(err)
	}
	versioned := signedTestBuildResult(t, delegateKey, v0Message)
	if err := versioned.validateForDelegate(delegate); err != nil {
		t.Fatalf("v0 AUTO build result rejected: %v", err)
	}
	if err := versioned.validateForDelegate(outsider); err == nil || !strings.Contains(err.Error(), "pinned delegated executor") {
		t.Fatalf("v0 wrong delegate accepted: %v", err)
	}
	heap := autoComputeBudgetHeapInstruction()
	duplicated, err := compileV0Message(delegate, mustKey(bridgeSettings), []compiledInstruction{heap, heap, outer}, []LookupTableSnapshot{table})
	if err != nil {
		t.Fatal(err)
	}
	if err := signedTestBuildResult(t, delegateKey, duplicated).validateForDelegate(delegate); err == nil {
		t.Fatal("duplicated v0 heap passed the persisted gate")
	}

	// The version byte routes dispatch: an unknown header fails closed, and a
	// legacy wire cannot pass itself off as versioned.
	unknownVersion := append([]byte(nil), legacyMessage...)
	unknownVersion[0] = 3
	if err := signedTestBuildResult(t, delegateKey, unknownVersion).validateForDelegate(delegate); err == nil {
		t.Fatal("unknown message version passed the persisted gate")
	}
	masquerade := append([]byte(nil), legacyMessage...)
	masquerade[0] = 0x80
	if err := signedTestBuildResult(t, delegateKey, masquerade).validateForDelegate(delegate); err == nil {
		t.Fatal("legacy wire with a versioned header passed the persisted gate")
	}

	// The retained persisted-evidence checks still fire on both routes.
	drifted := versioned
	drifted.TransactionSignature = encodeBase58(bytes.Repeat([]byte{7}, ed25519.SignatureSize))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "does not match persisted evidence") {
		t.Fatalf("v0 signature drift accepted: %v", err)
	}
	drifted = legacy
	drifted.MessageSHA256 = sha256Bytes([]byte("drifted"))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "message hash does not match") {
		t.Fatalf("legacy message hash drift accepted: %v", err)
	}
	drifted = legacy
	drifted.SimulationSlot = 0
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "incomplete simulated signed transaction") {
		t.Fatalf("unsimulated result accepted: %v", err)
	}
	drifted = legacy
	drifted.SignedWireSHA256 = sha256Bytes([]byte("drifted"))
	if err := drifted.validateForDelegate(delegate); err == nil || !strings.Contains(err.Error(), "transaction hash mismatch") {
		t.Fatalf("wire hash drift accepted: %v", err)
	}
}
