package backyardrwa

import (
	"encoding/binary"
	"fmt"
)

// AUTO execution resources. The deployed Squads ELF cannot parse or execute
// the eight-constraint candidate policy under the 32 KiB default heap — the
// LiteSVM semantic proof observed an access violation during create payload
// parsing and again during execute — so every compiled AUTO execution leg
// carries the canonical ComputeBudget heap request before its payload. 64 KiB
// is the tested working value, not a proven minimum: only 32 and 64 KiB were
// exercised. Installed Maple and selector lanes compile byte-identical
// messages and never gain this instruction.
const (
	autoExecutionHeapBytes = 65536
	// computeBudgetProgram is the exact ComputeBudget program identity from the
	// authoritative SDK sources (@solana/web3.js src/programs/compute-budget.ts
	// and solana-sdk-ids compute_budget declare_id!). Tests pin its decoded
	// bytes independently; base58 validity alone never proves identity.
	computeBudgetProgram          = "ComputeBudget111111111111111111111111111111"
	requestHeapFrameDiscriminator = 1
)

// autoComputeBudgetHeapInstruction is the canonical ComputeBudget
// requestHeapFrame(autoExecutionHeapBytes) instruction: discriminator byte 1
// followed by the requested frame size as a little-endian u32, with no
// accounts. The identical five bytes every AUTO leg must carry.
func autoComputeBudgetHeapInstruction() compiledInstruction {
	data := []byte{requestHeapFrameDiscriminator, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(data[1:], autoExecutionHeapBytes)
	return compiledInstruction{program: mustKey(computeBudgetProgram), data: data}
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
