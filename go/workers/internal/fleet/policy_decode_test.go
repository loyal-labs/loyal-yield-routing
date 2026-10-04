package fleet

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

func TestPolicyPrefixRetainsByteLimitAndTailIndependence(t *testing.T) {
	// Fleet's retained prefix ABI has no hooks, limits or account tail.
	for _, size := range []int{128, 256, 257} {
		instruction := RouteInstruction{Program: KLendProgram, Data: bytes.Repeat([]byte{7}, size)}
		data, err := BuildExactPolicyFixture(testMarket, testVault, 1, []RouteInstruction{instruction})
		if err != nil {
			t.Fatal(err)
		}
		policy, err := DecodeSquadsPolicy(data)
		if size <= 256 {
			if err != nil {
				t.Fatalf("retained byte size %d refused: %v", size, err)
			}
			if _, err = validateDelegatedInstructions(policy, testVault, []RouteInstruction{instruction}); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("accepted 257-byte fleet constraint")
		}
		if size == 256 {
			h, offset, err := squadspolicy.DecodeHeader(data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = squadspolicy.DecodePayload(data, offset, h.VaultIndex, false); err == nil {
				t.Fatal("full payload decoder accepted prefix-only policy")
			}
		}
	}
}
func TestPolicyTypedValuesPreserveNumericWidthAndByteOperators(t *testing.T) {
	cases := []struct {
		value   squadspolicy.DataValueView
		equal   []byte
		greater []byte
	}{
		{squadspolicy.DataValueView{Kind: 0, U8: 1}, []byte{1}, []byte{2}},
		{squadspolicy.DataValueView{Kind: 1, U16: 255}, []byte{255, 0}, []byte{0, 1}},
		{squadspolicy.DataValueView{Kind: 2, U32: 65535}, []byte{255, 255, 0, 0}, []byte{0, 0, 1, 0}},
		{squadspolicy.DataValueView{Kind: 3, U64: 1<<32 - 1}, []byte{255, 255, 255, 255, 0, 0, 0, 0}, []byte{0, 0, 0, 0, 1, 0, 0, 0}},
		{squadspolicy.DataValueView{Kind: 4, U128: [16]byte{255}}, append([]byte{255}, make([]byte, 15)...), append([]byte{0, 1}, make([]byte, 14)...)},
	}
	for _, c := range cases {
		constraint := squadspolicy.DataConstraintView{DataOffset: 1, DataValue: c.value, Operator: squadspolicy.OpEquals}
		if !policyDataMatches(constraint, append([]byte{0}, c.equal...)) || policyDataMatches(constraint, append([]byte{0}, c.equal[:len(c.equal)-1]...)) {
			t.Fatalf("variant %d width/equality changed", c.value.Kind)
		}
		constraint.Operator = squadspolicy.OpGreaterThan
		if !policyDataMatches(constraint, append([]byte{0}, c.greater...)) {
			t.Fatalf("variant %d numeric ordering changed", c.value.Kind)
		}
	}
	constraint := squadspolicy.DataConstraintView{DataValue: squadspolicy.DataValueView{Kind: 5, Bytes: []byte{1}}, Operator: squadspolicy.OpGreaterThan}
	if policyDataMatches(constraint, []byte{2}) {
		t.Fatal("byte vector accepted numeric ordering")
	}
}
func TestPolicyPrefixRejectsMalformedVectorsAndAuthority(t *testing.T) {
	ix := RouteInstruction{Program: KLendProgram, Data: []byte{7}}
	data, err := BuildExactPolicyFixture(testMarket, testVault, 1, []RouteInstruction{ix})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){func(b []byte) { binary.LittleEndian.PutUint32(b[65:69], 33) }, func(b []byte) { binary.LittleEndian.PutUint32(b[110:114], 129) }, func(b []byte) { b[0] ^= 1 }} {
		bad := append([]byte(nil), data...)
		mutate(bad)
		if _, err = DecodeSquadsPolicy(bad); err == nil {
			t.Fatal("malformed policy accepted")
		}
	}
	for _, offset := range []int{101, 104, 57} {
		bad := append([]byte(nil), data...)
		bad[offset] = 8
		p, err := DecodeSquadsPolicy(bad)
		if err == nil {
			if _, err = validateDelegatedInstructions(p, testVault, []RouteInstruction{ix}); err == nil {
				t.Fatalf("changed authority at %d accepted", offset)
			}
		}
	}
}

func TestPolicyCompactPrefixBoundaries(t *testing.T) {
	legacy, err := BuildExactPolicyFixture(testMarket, testVault, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	program, err := decodePublicKey(KLendProgram)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 128, 129} {
		data := append([]byte(nil), legacy[:110]...)
		data = append(data, 1)
		data = append(data, program[:]...)
		data = append(data, byte(count))
		for i := 0; i < count; i++ {
			data = append(data, 0, 0, 1)
			data = appendU64x(data, 0)
			data = append(data, 5)
			data = appendU32x(data, 256)
			data = append(data, bytes.Repeat([]byte{7}, 256)...)
			data = append(data, 0)
		}
		policy, err := DecodeSquadsPolicy(data)
		if count <= 128 {
			if err != nil || len(policy.Constraints) != count {
				t.Fatalf("compact count=%d: %v", count, err)
			}
		} else if err == nil {
			t.Fatal("compact count 129 accepted")
		}
	}
}

func TestStrictSwapPolicyTailContinuationAndNegatives(t *testing.T) {
	_, plan, _ := crossMintPreparationFixture(t)
	data := connectedSwapPolicy(t, plan.Bindings, 3)
	policy, _, limits, err := decodeStrictSwapPolicy(data)
	if err != nil || len(limits) != 3 || len(policy.Constraints) != 2 {
		t.Fatalf("strict synthetic swap policy: %v", err)
	}
	h, offset, err := squadspolicy.DecodeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	_, end, err := squadspolicy.DecodeConstraints(data, offset, h.VaultIndex, false, 128, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){func(b []byte) { b[end] = 1 }, func(b []byte) { b[end+1] = 1 }, func(b []byte) { binary.LittleEndian.PutUint32(b[end+2:end+6], 4) }} {
		bad := append([]byte(nil), data...)
		mutate(bad)
		if _, _, _, err = decodeStrictSwapPolicy(bad); err == nil {
			t.Fatal("invalid hooks/spending count accepted")
		}
	}
}
