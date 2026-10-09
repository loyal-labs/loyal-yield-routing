package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// AUTO execution resources. The deployed Squads ELF cannot parse or execute
// the eight-constraint candidate policy under the 32 KiB default heap — the
// LiteSVM semantic proof observed an access violation during create payload
// parsing and again during execute — so every compiled AUTO execution leg
// carries the canonical ComputeBudget heap request before its payload. 64 KiB
// is the tested working value, not a proven minimum: only 32 and 64 KiB were
// exercised. Installed Maple and selector lanes compile byte-identical
// messages and never gain this instruction. AUTO Jupiter swaps additionally
// carry the canonical setComputeUnitLimit frame after the heap frame: the live
// pre-send simulation of the AUTO USDC swap consumed 202842 of 202850 compute
// units through an already-successful Jupiter CPI, so the default meter, not
// the heap, is the binding constraint there. The initializer and Kamino legs
// keep the heap-only envelope.
const (
	autoExecutionHeapBytes = 65536
	// autoSwapComputeUnitLimit sets a fixed compute budget for AUTO Jupiter
	// swaps above the 202842 units consumed before the default meter failed.
	autoSwapComputeUnitLimit = 300000
)

// autoComputeBudgetHeapInstruction is the canonical ComputeBudget
// requestHeapFrame(autoExecutionHeapBytes) instruction with no accounts: the
// identical five bytes every AUTO leg must carry.
func autoComputeBudgetHeapInstruction() compiledInstruction {
	return sdkInstruction(spl.RequestHeapFrame(autoExecutionHeapBytes))
}

// isAutoExecutionHeapInstruction reports whether an instruction is exactly the
// canonical heap request — same program, no accounts, five-byte data with the
// requestHeapFrame discriminator and the exact reviewed frame size. Decode-side
// validation and tests reuse this so a drifted frame cannot pass as the
// reviewed resource.
func isAutoExecutionHeapInstruction(instruction compiledInstruction) bool {
	expected := autoComputeBudgetHeapInstruction()
	return instruction.program == expected.program && len(instruction.accounts) == 0 &&
		bytesEqual(instruction.data, expected.data)
}

// isCanonicalHeapFrame is the decode-shape form of the same check: program
// identity, zero accounts, exact payload bytes.
func isCanonicalHeapFrame(program publicKey, accountCount int, data []byte) bool {
	expected := autoComputeBudgetHeapInstruction()
	return program == expected.program && accountCount == 0 && bytesEqual(data, expected.data)
}

// autoComputeBudgetUnitLimitInstruction is the canonical ComputeBudget
// setComputeUnitLimit(autoSwapComputeUnitLimit) instruction with no accounts.
// It is only ever emitted on AUTO Jupiter swap legs, immediately behind the
// heap frame.
func autoComputeBudgetUnitLimitInstruction() compiledInstruction {
	return sdkInstruction(spl.SetComputeUnitLimit(autoSwapComputeUnitLimit))
}

// isCanonicalUnitLimitFrame is the decode-shape form of the same check,
// mirroring isCanonicalHeapFrame: program identity, zero accounts, exact
// payload bytes.
func isCanonicalUnitLimitFrame(program publicKey, accountCount int, data []byte) bool {
	expected := autoComputeBudgetUnitLimitInstruction()
	return program == expected.program && accountCount == 0 && bytesEqual(data, expected.data)
}

// isAutoSwapResourceTransaction is the decode-side closure over the AUTO swap
// resource prefix: the canonical heap frame at index 0 and the canonical
// compute-unit frame at index 1, and the ComputeBudget program appearing
// nowhere else, so a duplicated, reordered or arbitrary budget instruction
// cannot slip through. The payload behind the prefix stays validated by the
// caller's existing sequence gates.
func isAutoSwapResourceTransaction(instructions []decodedLegacyInstruction) bool {
	if len(instructions) < 3 {
		return false
	}
	heap, unitLimit := instructions[0], instructions[1]
	if !isCanonicalHeapFrame(heap.program, len(heap.accountIndexes), heap.data) ||
		!isCanonicalUnitLimitFrame(unitLimit.program, len(unitLimit.accountIndexes), unitLimit.data) {
		return false
	}
	budget := autoComputeBudgetHeapInstruction()
	for _, instruction := range instructions[2:] {
		if instruction.program == budget.program {
			return false
		}
	}
	return true
}

// withAutoSwapExecutionResources prepends the canonical heap frame plus the
// compute-unit frame to a caller-validated AUTO swap payload without mutating
// it. The heap-only counterpart withAutoExecutionHeap is unchanged and stays
// the initializer and Kamino envelope.
func withAutoSwapExecutionResources(payload []compiledInstruction) []compiledInstruction {
	instructions := make([]compiledInstruction, 0, len(payload)+2)
	instructions = append(instructions, autoComputeBudgetHeapInstruction(), autoComputeBudgetUnitLimitInstruction())
	return append(instructions, payload...)
}

// compileAutoSwapResourceLegacyMessage is the closed legacy wrapper for AUTO
// Jupiter swap legs: the canonical heap frame at index 0, the canonical
// compute-unit frame at index 1, followed by exactly one caller-validated
// payload instruction. The initializer and legacy recovery wrapper
// compileAutoResourceLegacyMessage keeps its heap-only envelope so already
// persisted messages decode exactly as they were signed.
func compileAutoSwapResourceLegacyMessage(feePayer, blockhash publicKey, payload compiledInstruction) ([]byte, error) {
	if payload.program == (publicKey{}) {
		return nil, fmt.Errorf("empty AUTO payload instruction")
	}
	return encodeLegacyMessage(feePayer, blockhash, withAutoSwapExecutionResources([]compiledInstruction{payload}))
}

// isAutoResourceHeapTransaction is the decode-side closure over the AUTO heap
// resource: instruction[0] must be exactly the canonical reviewed frame, and
// the ComputeBudget program may appear nowhere else, so an arbitrary,
// duplicated or reordered compute instruction cannot slip through. The payload
// positions behind the frame stay validated by the caller's existing sequence
// gates.
func isAutoResourceHeapTransaction(instructions []decodedLegacyInstruction) bool {
	if len(instructions) == 0 {
		return false
	}
	first := instructions[0]
	if !isCanonicalHeapFrame(first.program, len(first.accountIndexes), first.data) {
		return false
	}
	heap := autoComputeBudgetHeapInstruction()
	for _, instruction := range instructions[1:] {
		if instruction.program == heap.program {
			return false
		}
	}
	return true
}

// withAutoExecutionHeap prepends the canonical heap request to a
// caller-validated payload without mutating it.
func withAutoExecutionHeap(payload []compiledInstruction) []compiledInstruction {
	instructions := make([]compiledInstruction, 0, len(payload)+1)
	instructions = append(instructions, autoComputeBudgetHeapInstruction())
	return append(instructions, payload...)
}

// compileAutoResourceLegacyMessage is the closed legacy wrapper for AUTO
// execution legs: the canonical heap frame at index 0 followed by exactly one
// caller-validated payload instruction, encoded through the shared legacy
// encoder. The payload's own exact-sequence validation stays in the owning
// compiler — Jupiter's single-policy-outer rule and the initializer's single
// wrapped Squads instruction — so no general multi-instruction legacy API is
// exposed.
func compileAutoResourceLegacyMessage(feePayer, blockhash publicKey, payload compiledInstruction) ([]byte, error) {
	if payload.program == (publicKey{}) {
		return nil, fmt.Errorf("empty AUTO payload instruction")
	}
	return encodeLegacyMessage(feePayer, blockhash, withAutoExecutionHeap([]compiledInstruction{payload}))
}
