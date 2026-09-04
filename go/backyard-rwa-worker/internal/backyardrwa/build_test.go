package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// forwardedAdaptorWire models the pinned Voltr CPI framing evidenced by its
// SDK args: the selected instructionDiscriminator Vec is unwrapped, while
// amount and the Borsh Option<Vec<u8>> additionalArgs envelope are forwarded.
// It is intentionally test-only; production constructs the Voltr outer wire.
func forwardedAdaptorWire(t *testing.T, outer []byte) []byte {
	t.Helper()
	if len(outer) != 91 || outer[16] != 1 || binary.LittleEndian.Uint32(outer[17:21]) != 8 ||
		outer[29] != 1 || binary.LittleEndian.Uint32(outer[30:34]) != 57 {
		t.Fatal("outer Voltr option envelope drifted")
	}
	inner := append([]byte(nil), outer[21:29]...)
	inner = append(inner, outer[8:16]...)
	return append(inner, outer[29:]...)
}

func bridgeTestRequest(action Action, amount uint64) BridgeBuildRequest {
	return BridgeBuildRequest{
		Action: action, AmountRaw: amount,
		Report:          BridgeReport{Sequence: 1, ObservedSlot: 1, NAVAfterRaw: 0, SnapshotDigest: hex.EncodeToString(bytes.Repeat([]byte{1}, 32))},
		AdaptorConfig:   bridgeStrategy,
		Settings:        bridgeSettings,
		RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
	}
}

func TestBridgeInstructionMatchesPinnedVoltrAndAdaptorEnvelopes(t *testing.T) {
	request := bridgeTestRequest(VoltrAllocateToSquads, 1_000_000)
	inner, policy, constraintIndex, err := bridgeInstruction(request)
	if err != nil {
		t.Fatal(err)
	}
	if policy != mustKey(bridgeAllocationPolicy) || constraintIndex != 0 || inner.program != mustKey(bridgeVoltrProgram) || len(inner.accounts) != 17 {
		t.Fatalf("unexpected allocation identity")
	}
	// This is the independently generated SDK wire for 1 USDC and ReportV1
	// sequence=slot=1, NAV=0, digest=01*32. It catches accidental ABI drift.
	const want = "f65239e283defdf940420f00000000000108000000f223c68952e1f2b60139000000010100000000000000010000000000000000000000000000000101010101010101010101010101010101010101010101010101010101010101"
	if got := hex.EncodeToString(inner.data); got != want {
		t.Fatalf("allocation envelope drifted\n got %s\nwant %s", got, want)
	}
	if !inner.accounts[0].signer || inner.accounts[0].key != mustKey(bridgeVault) || inner.accounts[3].writable || !inner.accounts[16].writable {
		t.Fatalf("allocation inner signer/custody roles drifted")
	}
	forwarded := forwardedAdaptorWire(t, inner.data)
	if len(forwarded) != 78 || !bytes.Equal(forwarded[:8], adaptorDepositDiscriminator) ||
		binary.LittleEndian.Uint64(forwarded[8:16]) != 1_000_000 || forwarded[16] != 1 ||
		binary.LittleEndian.Uint32(forwarded[17:21]) != 57 || forwarded[21] != 1 {
		t.Fatalf("allocation adaptor CPI wire drifted: %s", hex.EncodeToString(forwarded))
	}

	stage, stagePolicy, stageConstraint, err := bridgeInstruction(bridgeTestRequest(StageSquadsToVoltr, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if stagePolicy != mustKey(bridgeStagePolicy) || stageConstraint != 0 || stage.program != mustKey(bridgeTokenProgram) || hex.EncodeToString(stage.data) != "0c40420f000000000006" {
		t.Fatalf("staging template drifted")
	}
	withdraw, withdrawPolicy, withdrawConstraint, err := bridgeInstruction(bridgeTestRequest(VoltrRestoreIdle, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if withdrawPolicy != mustKey(bridgeWithdrawPolicy) || withdrawConstraint != 0 || hex.EncodeToString(withdraw.data[:8]) != "1f2da205c1d986bc" || len(withdraw.accounts) != 17 {
		t.Fatalf("restore template drifted")
	}
	if withdraw.accounts[5].writable {
		t.Fatal("immutable adaptor config was forwarded writable on restore")
	}
	forwardedWithdraw := forwardedAdaptorWire(t, withdraw.data)
	if len(forwardedWithdraw) != 78 || !bytes.Equal(forwardedWithdraw[:8], adaptorWithdrawDiscriminator) ||
		binary.LittleEndian.Uint64(forwardedWithdraw[8:16]) != 1_000_000 || forwardedWithdraw[16] != 1 ||
		binary.LittleEndian.Uint32(forwardedWithdraw[17:21]) != 57 || forwardedWithdraw[21] != 1 {
		t.Fatalf("withdraw adaptor CPI wire drifted: %s", hex.EncodeToString(forwardedWithdraw))
	}
	nav, navPolicy, navConstraint, err := bridgeInstruction(bridgeTestRequest(ReportNAV, 0))
	if err != nil || navPolicy != mustKey(bridgeWithdrawPolicy) || navConstraint != 0 || !bytes.Equal(nav.data[:8], voltrWithdrawDiscriminator) {
		t.Fatalf("NAV refresh must select the installed zero-value withdrawal constraint: %v", err)
	}
	forwardedNAV := forwardedAdaptorWire(t, nav.data)
	if len(forwardedNAV) != 78 || !bytes.Equal(forwardedNAV[:8], adaptorWithdrawDiscriminator) ||
		binary.LittleEndian.Uint64(forwardedNAV[8:16]) != 0 || forwardedNAV[16] != 1 ||
		binary.LittleEndian.Uint32(forwardedNAV[17:21]) != 57 || forwardedNAV[21] != 1 {
		t.Fatalf("NAV adaptor CPI wire drifted: %s", hex.EncodeToString(forwardedNAV))
	}
}

func TestBridgeTransactionSignsExactLegacyWireAndPersistsOnlyAfterSimulation(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignBridgeTransactionForDelegate(bridgeTestRequest(ReportNAV, 0), key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if len(signed.signedWire) <= ed25519.SignatureSize || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:1+ed25519.SignatureSize]) {
		t.Fatal("legacy wire did not contain a valid signature over its exact message")
	}
	if len(signed.signedWire) != 1027 || signed.messageSHA256 != "5af2fbd3d2d15cb0cb8803a7dbc1b6469219acccbb6a3ac250a9ad4f9d8bca53" ||
		signed.signedWireSHA256 != "e94102d36cd58d40184ea4526dc21b438a2f7806727d3567887556cedda4ecd5" {
		t.Fatalf("ticketed NAV packet fingerprint drifted: bytes=%d message=%s wire=%s", len(signed.signedWire), signed.messageSHA256, signed.signedWireSHA256)
	}
	if signed.transactionSignature != encodeBase58(signed.signedWire[1:1+ed25519.SignatureSize]) || len(signed.messageSHA256) != 64 || len(signed.signedWireSHA256) != 64 {
		t.Fatal("transaction evidence was not bound to exact signed bytes")
	}
	// The exact ProgramInteraction Borsh envelope carries the dedicated NAV
	// withdrawal policy's only constraint.
	inner, policy, constraints, err := ticketedBridgeInstructions(bridgeTestRequest(ReportNAV, 0))
	if err != nil {
		t.Fatal(err)
	}
	outer, err := wrapSquadsPolicyForDelegate(policy, delegate, delegate, constraints, inner)
	if err != nil {
		t.Fatal(err)
	}
	if len(outer.data) < 19 || !bytes.Equal(outer.data[17:19], []byte{0, 1}) {
		t.Fatalf("wrong ticketed policy constraint indexes: %v", outer.data[17:19])
	}
	if _, err := signed.BuildResult(0); err == nil {
		t.Fatal("unsigned simulation slot was accepted")
	}
	result, err := signed.BuildResult(123)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil || result.SimulationSlot != 123 {
		t.Fatalf("persistable exact transaction invalid: %v", err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatalf("exact delegated signer was rejected: %v", err)
	}
	if err := result.validateForDelegate(mustKey(bridgeDelegate)); err == nil {
		t.Fatal("wrong delegated signer was accepted")
	}
	tampered := result
	tampered.SignedWire = append([]byte(nil), result.SignedWire...)
	tampered.SignedWire[len(tampered.SignedWire)-1] ^= 1
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered persisted wire passed validation")
	}

	// Production refuses a locally generated key even though the test-only
	// builder above can use one to prove wire/signature mechanics.
	if _, err := BuildAndSignBridgeTransaction(bridgeTestRequest(ReportNAV, 0), key); err == nil {
		t.Fatal("unpinned executor key was accepted")
	}
}

func TestBridgeBuilderRejectsCapitalAndReportMutations(t *testing.T) {
	bad := bridgeTestRequest(ReportNAV, 1)
	if _, _, _, err := bridgeInstruction(bad); err == nil {
		t.Fatal("NAV refresh capital movement accepted")
	}
	bad = bridgeTestRequest(VoltrAllocateToSquads, 1)
	bad.Report.Sequence = 0
	if _, _, _, err := bridgeInstruction(bad); err == nil {
		t.Fatal("zero report sequence accepted")
	}
	bad = bridgeTestRequest(VoltrAllocateToSquads, 1)
	bad.Report.Sequence = bad.Report.ObservedSlot + 1
	if _, err := buildAndSignBridgeTransactionForDelegate(bad, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)), publicKeyFromBytes(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey))); err == nil {
		t.Fatal("report sequence not bound to observed slot was accepted")
	}
	bad = bridgeTestRequest(VoltrRestoreIdle, 1)
	bad.Report.SnapshotDigest = "00" + bad.Report.SnapshotDigest[2:]
	if _, _, _, err := bridgeInstruction(bad); err != nil {
		t.Fatal("nonzero digest was rejected")
	}
	bad.Report.SnapshotDigest = strings.Repeat("00", 32)
	if _, _, _, err := bridgeInstruction(bad); err == nil {
		t.Fatal("zero digest accepted")
	}
	bad = bridgeTestRequest(StageSquadsToVoltr, bridgeCapRaw+1)
	if _, _, _, err := bridgeInstruction(bad); err == nil {
		t.Fatal("over-cap staging accepted")
	}
}
