package backyardrwa

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// liveSquadsPolicy152Hex is the exact finalized mainnet account data of the
// allocation bridge policy at Squads seed 152, captured at finalized
// commitment (slot 448193729).
const liveSquadsPolicy152Hex = "de8707a3ebb121444379e0e134dbd2b80aa2aa8199739496b43794a41cdbb2a2217411e6d8efcae59800000000000000ff00000000000000000000000000000000010000004a9f9f54e872666d7c3d8dd6e06de67ec5f8953c0c6461fd593553d48d48fb4007010000000000030002000000d69aa5fd31edd7807c9b6602a74e09c229e70bcc5c62617458ca337793d1f38702000000000001000000b5533e4a117b87a7334998bc752855f8b58d005289d39352794e393e237eb6e90001000100000076c53dfd30a712428451510f7a2b9190a4a63ad618c390aee2edd817c4929c8e000500000000000000000000000509000000a4aff629b28c2303000009000000000000000300000000000000000209000000000000000300d0ed902e000000052700000000000000030010a5d4e80000000511000000000000000506000000013900000001000db4588c00282e3906bc90f790d4706811e8cc24b29cca991b4d70db3aa58d390b000000000001000000068513c48cf2381aa83b47182c9841a91f0f337d03cf1f887c1c308c1653c2fa00020001000000f5a4eb5618d09fd728fb93f7e0cd62d27104d8e3cad915c47d5b5658117d6f5700030001000000b5533e4a117b87a7334998bc752855f8b58d005289d39352794e393e237eb6e900080001000000c6fa7af3bedbad3a3d65f36aabc97431b1bbe4c2d2f6e0e47ca60203452f5d61000b0001000000c6d7b1fbdbf758db76f540a3aa9a48779b269a36fa9b5c4d9cef4155a3c575af000c000100000006ddf6e1d765a193d9cbe146ceeb79ac1cb485ed5f5b37913a8cf5857eff00a9000d0001000000d69aa5fd31edd7807c9b6602a74e09c229e70bcc5c62617458ca337793d1f387000e00010000004379e0e134dbd2b80aa2aa8199739496b43794a41cdbb2a2217411e6d8efcae5000f0001000000068513c48cf2381aa83b47182c9841a91f0f337d03cf1f887c1c308c1653c2fa00100001000000c3c8bb6e0945061658ed1241d10f3625b81bf0af8a1fca6990709eecc9fe44050011000100000076c53dfd30a712428451510f7a2b9190a4a63ad618c390aee2edd817c4929c8e000500000000000000000000000508000000f65239e283defdf90008000000000000000300000000000000000208000000000000000300d0ed902e000000053300000000000000030010a5d4e800000005100000000000000005130000000108000000f223c68952e1f2b601390000000100000001000000c6fa7af3bedbad3a3d65f36aabc97431b1bbe4c2d2f6e0e47ca60203452f5d6145a0ad6a0000000000010000d0ed902e00000000000000000000000000d0ed902e00000045a0ad6a0000000045a0ad6a0000000000971a24624a17f59e77b372ad6dcac49bc6c9b0fc0637028f5270205fd4977c080000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

// livePolicy152MaskedByteRanges is the mask the offline script derived for
// seed 152: the embedded limit's timeConstraints.start and the contiguous
// usage word pair (remainingInPeriod, lastReset).
var livePolicy152MaskedByteRanges = [][2]int64{{945, 953}, {973, 989}}

func liveSquadsPolicy152Bytes(t *testing.T) []byte {
	t.Helper()
	data, err := hex.DecodeString(liveSquadsPolicy152Hex)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMaskedDigestPinsLivePolicy152(t *testing.T) {
	data := liveSquadsPolicy152Bytes(t)
	if len(data) != 1548 {
		t.Fatalf("seed-152 fixture is %d bytes, want 1548", len(data))
	}
	if err := validatePolicyByteMask(livePolicy152MaskedByteRanges); err != nil {
		t.Fatalf("live seed-152 mask is invalid: %v", err)
	}
	if !maskedPolicyDigestMatches(data, livePolicy152MaskedByteRanges, strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads]) {
		t.Fatal("masked pin rejected the live seed-152 policy")
	}
	if maskedPolicyDigestMatches(data, livePolicy152MaskedByteRanges, strategyTwoBridgePolicyRawDigests[VoltrAllocateToSquads]) {
		t.Fatal("masked pin accepted the raw digest, which excludes nothing")
	}
}

func TestMaskedDigestIgnoresMaskedBytes(t *testing.T) {
	data := liveSquadsPolicy152Bytes(t)
	// Charging the limit or restamping its window must never break the pin:
	// flip every byte of each masked range, including the raw digest's own
	// volatile content.
	for index, bounds := range livePolicy152MaskedByteRanges {
		mutated := append([]byte(nil), data...)
		for offset := int(bounds[0]); offset < int(bounds[1]); offset++ {
			mutated[offset] ^= 0xff
		}
		if !maskedPolicyDigestMatches(mutated, livePolicy152MaskedByteRanges, strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads]) {
			t.Fatalf("masked range %d mutation broke the normalized pin", index)
		}
	}
}

func TestMaskedDigestRejectsUnmaskedMutation(t *testing.T) {
	data := liveSquadsPolicy152Bytes(t)
	normalized := strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads]
	// Discriminator, instruction constraints, the limit's own quantity
	// constraint, the first byte after the mask, and the trailing zero
	// padding all stay pinned.
	for name, offset := range map[string]int{
		"discriminator":    0,
		"constraint":       300,
		"executor":         700,
		"maxPerPeriod":     953,
		"afterMask":        989,
		"trailingPadding":  1040,
		"trailingBytePast": len(data) - 1,
	} {
		mutated := append([]byte(nil), data...)
		mutated[offset] ^= 0x01
		if maskedPolicyDigestMatches(mutated, livePolicy152MaskedByteRanges, normalized) {
			t.Fatalf("mutated %s (offset %d) still matched the masked pin", name, offset)
		}
	}
}

func TestMaskedDigestEmptyMaskEqualsRawDigest(t *testing.T) {
	data := liveSquadsPolicy152Bytes(t)
	// A policy without embedded limits has an empty mask, so its normalized
	// digest is the raw digest.
	if !maskedPolicyDigestMatches(data, nil, strategyTwoBridgePolicyRawDigests[VoltrAllocateToSquads]) {
		t.Fatal("empty mask did not reduce the pin to the raw digest")
	}
	if maskedPolicyDigestMatches(data, nil, strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads]) {
		t.Fatal("empty mask accepted a normalized digest of a non-empty mask")
	}
}

func TestMaskedDigestFailsClosedOnBadMasks(t *testing.T) {
	data := liveSquadsPolicy152Bytes(t)
	normalized := strategyTwoBridgePolicyNormalizedDigests[VoltrAllocateToSquads]
	cases := map[string][][2]int64{
		"endPastAccount":  {{940, int64(len(data) + 1)}},
		"negativeStart":   {{-8, 16}},
		"emptyRange":      {{900, 900}},
		"overlapping":     {{945, 953}, {950, 981}},
		"unsorted":        {{973, 981}, {945, 953}},
		"overCap":         {{0, 33}, {100, 133}, {200, 233}},
		"exactOverCapRun": {{0, int64(maxMaskedPolicyBytes) + 1}},
	}
	for name, ranges := range cases {
		if err := validatePolicyByteMask(ranges); err == nil {
			t.Fatalf("%s mask was accepted", name)
		}
		if maskedPolicyDigestMatches(data, ranges, normalized) {
			t.Fatalf("%s mask still matched the pin", name)
		}
	}
	if err := validatePolicyByteMask(livePolicy152MaskedByteRanges); err != nil {
		t.Fatalf("live seed-152 mask was rejected: %v", err)
	}
	// A mask that is structurally valid but out of bounds for this account
	// fails closed at compare time, not just at manifest validation time.
	if maskedPolicyDigestMatches(data[:1024], [][2]int64{{1024, 1032}}, normalized) {
		t.Fatal("out-of-bounds mask matched a truncated account")
	}
}

func TestAllocationDailyLimitGuardHolds(t *testing.T) {
	if err := evaluateAllocationDailyLimit(0, strategyTwoDailyAllocationCapRaw); err != nil {
		t.Fatalf("exact daily capacity was rejected: %v", err)
	}
	if err := evaluateAllocationDailyLimit(200_000_000, 100_000_000); err != nil {
		t.Fatalf("split daily capacity was rejected: %v", err)
	}
	for _, sent := range []uint64{0, 200_000_000, strategyTwoDailyAllocationCapRaw, strategyTwoDailyAllocationCapRaw + 1} {
		var hold *BudgetHold
		if err := evaluateAllocationDailyLimit(sent, strategyTwoDailyAllocationCapRaw+1); !errors.As(err, &hold) || hold.Reason != allocationDailyLimitReason {
			t.Fatalf("sent=%d accepted an amount above the daily cap: %v", sent, err)
		}
	}
	if err := evaluateAllocationDailyLimit(strategyTwoDailyAllocationCapRaw, 1); err == nil {
		t.Fatal("exhausted daily window accepted another allocation")
	}
}

func TestSquadsSpendingLimitExceededClassification(t *testing.T) {
	logs := []string{
		"Program " + bridgeSquadsProgram + " invoke [1]",
		"Program log: Instruction: ExecuteTransactionSyncV2",
		"Program " + bridgeSquadsProgram + " failed: custom program error: 0x17b9",
	}
	rawErr := json.RawMessage(`{"InstructionError":[0,{"Custom":6073}]}`)
	if !squadsSpendingLimitExceeded(rawErr, logs) {
		t.Fatal("Squads 6073 was not classified as a spending-limit refusal")
	}
	classification := ClassifyConfirmedReportFailure(rawErr, logs)
	if !classification.Retryable || classification.Reason != squadsSpendingLimitReason {
		t.Fatalf("6073 must terminate in failed with its own reason, got %+v", classification)
	}
	otherFailed := []string{
		"Program " + bridgeSquadsProgram + " invoke [1]",
		"Program 11111111111111111111111111111111111111111 invoke [2]",
		"Program 11111111111111111111111111111111111111111 failed: custom program error: 0x17b9",
	}
	if squadsSpendingLimitExceeded(rawErr, otherFailed) {
		t.Fatal("6073 raised by another program was attributed to the Squads policy")
	}
	wrongCode := json.RawMessage(`{"InstructionError":[0,{"Custom":6000}]}`)
	if squadsSpendingLimitExceeded(wrongCode, logs) {
		t.Fatal("unrelated custom error was classified as a spending-limit refusal")
	}
	if squadsSpendingLimitExceeded(rawErr, logs[:2]) {
		t.Fatal("truncated logs without a failing frame were classified")
	}
}

func TestSpendingLimitRefusalIsANamedHoldThatKeepsTheLoopRunning(t *testing.T) {
	if !isPureHold(budgetHold(squadsSpendingLimitReason)) {
		t.Fatal("the journaled spending-limit refusal was not recognized as a recoverable hold")
	}
	for name, other := range map[string]error{
		"otherHold":   budgetHold("bridge_admission_unavailable"),
		"allocation":  budgetHold(allocationDailyLimitReason),
		"observation": errConfirmedObservationUnavailable,
		"plain":       errors.New("incoherent observation"),
	} {
		if isPureHold(other) {
			t.Fatalf("%s was treated as the pure spending-limit hold: %v", name, other)
		}
	}
	if isPureHold(nil) {
		t.Fatal("a nil error was treated as the pure spending-limit hold")
	}
	if !isPureHold(fmt.Errorf("bridge build: %w", budgetHold(squadsSpendingLimitReason))) {
		t.Fatal("a transparently wrapped pure hold stopped being recoverable")
	}
	// A hold joined with any other fault is a real fault: continuing would
	// loop on the hold while the store or wire error goes unseen.
	joined := errors.Join(budgetHold(squadsSpendingLimitReason), errors.New("persist failed"))
	var hold *BudgetHold
	if isPureHold(joined) || !errors.As(joined, &hold) {
		t.Fatalf("joined hold/store error misclassified: pure=%t asHold=%t", isPureHold(joined), errors.As(joined, &hold))
	}
	if !isPureHold(errors.Join(journaledBudgetHold(squadsSpendingLimitReason), journaledBudgetHold(squadsSpendingLimitReason))) {
		t.Fatal("two members of the same journaled refusal were treated as a foreign fault")
	}
	// The run loop must skip the held leg and retry on the next interval
	// instead of tearing the worker down mid-refusal.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	worker := &Worker{interval: time.Millisecond}
	ticks := 0
	err := worker.runTicks(ctx, make(chan error, 1), func(context.Context) error {
		ticks++
		if ticks == 1 {
			return journaledBudgetHold(squadsSpendingLimitReason)
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run loop exited on the spending-limit hold: %v", err)
	}
	if ticks < 3 {
		t.Fatalf("run loop stopped after the hold: %d ticks", ticks)
	}
	// A hold joined with a store failure stops the worker on the first tick
	// with both faults intact.
	joinedTicks := 0
	stopErr := worker.runTicks(context.Background(), make(chan error, 1), func(context.Context) error {
		joinedTicks++
		return errors.Join(budgetHold(squadsSpendingLimitReason), errors.New("persist failed"))
	})
	if joinedTicks != 1 || stopErr == nil || isPureHold(stopErr) || !errors.As(stopErr, &hold) {
		t.Fatalf("joined hold did not stop the worker exactly once: ticks=%d err=%v", joinedTicks, stopErr)
	}
	// Any other tick error still stops the worker.
	if plainErr := worker.runTicks(context.Background(), make(chan error, 1), func(context.Context) error {
		return errors.New("incoherent observation")
	}); plainErr == nil || isPureHold(plainErr) {
		t.Fatalf("unrelated tick error did not stop the worker: %v", plainErr)
	}
}

func TestAlreadyJournaledHoldIsNeverWrittenTwice(t *testing.T) {
	writes := 0
	worker := &Worker{runtime: tickRuntime{recordBudgetHold: func(context.Context, string, *BudgetHold) error {
		writes++
		return nil
	}}}
	// A fresh hold is journaled under the tick's operation exactly once.
	if err := worker.journalTickError(context.Background(), "op-1", budgetHold("bridge_admission_unavailable")); err == nil {
		t.Fatal("fresh hold was dropped")
	}
	if writes != 1 {
		t.Fatalf("fresh hold was journaled %d times, want 1", writes)
	}
	// The pre-broadcast refusal was already recorded by MarkPreBroadcastFailed:
	// the operation row is failed, so a second write would be rejected by the
	// store's never-submitted transition and the join would mask the hold.
	refusal := journaledBudgetHold(squadsSpendingLimitReason)
	if err := worker.journalTickError(context.Background(), "op-2", refusal); !errors.Is(err, refusal) {
		t.Fatalf("already-journaled hold was altered: %v", err)
	}
	if writes != 1 {
		t.Fatalf("already-journaled hold produced %d extra writes", writes-1)
	}
	// A refusal joined with a store failure is returned untouched so the run
	// loop sees both members, and it is not journaled a second time either.
	joined := errors.Join(refusal, errors.New("persist failed"))
	if err := worker.journalTickError(context.Background(), "op-2", joined); !errors.Is(err, joined) {
		t.Fatalf("joined error was rewritten: %v", err)
	}
	if writes != 1 {
		t.Fatalf("joined error produced %d extra writes", writes-1)
	}
}

func TestCatalogReadinessAcceptsChargedStrategyTwoPolicy(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	// The installed catalog lanes share the bridge policy bindings with the
	// active lane, so exercise the real catalog readiness set directly on an
	// installed lane. AUTO readiness is bound to the reviewed candidate
	// binding instead (see auto_policy_readiness_test.go).
	route, err := runtimeRoute("Ethena/USDe/PYUSD")
	if err != nil {
		t.Fatal(err)
	}
	if !catalogJupiterRoute(route.Lane) {
		t.Fatalf("%s did not resolve to a Jupiter catalog lane", route.Lane)
	}
	pins, err := catalogRoutePolicyPins(route, manifest)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := manifest.bridgePolicy(VoltrAllocateToSquads)
	if err != nil {
		t.Fatal(err)
	}
	pin, ok := pins[binding.Account]
	if !ok || pin.digest != binding.NormalizedDigest || len(pin.mask) == 0 {
		t.Fatalf("catalog readiness set lost the masked bridge pin: %+v", pin)
	}
	// Charge the limit to the floor and re-stamp its window over the live
	// account: exactly the healthy post-allocation state a raw sha256 compare
	// used to reject as drift.
	charged := liveSquadsPolicy152Bytes(t)
	for _, bounds := range binding.MaskedByteRanges {
		for offset := int(bounds[0]); offset < int(bounds[1]); offset++ {
			charged[offset] = 0x7f
		}
	}
	if sha256Bytes(charged) == pin.digest {
		t.Fatal("charged fixture unexpectedly matched the pin before masking")
	}
	if !maskedPolicyDigestMatches(charged, pin.mask, pin.digest) {
		t.Fatal("catalog readiness comparator rejected a charged strategy-two policy")
	}
}
