package multiply

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

// Synthetic ABI examples complement the independent policy-bank proof. They
// deliberately encode bytes without calling the production policy decoder.
func syntheticPolicy(t *testing.T, compact bool) []byte {
	t.Helper()
	settings, signer, program := solana.PublicKey{1}, solana.PublicKey{2}, solana.PublicKey{3}
	_, bump, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], {7, 0, 0, 0, 0, 0, 0, 0}}, mustKey(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	write := func(value any) {
		if err := binary.Write(&b, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	b.Write([]byte{222, 135, 7, 163, 235, 177, 33, 68})
	b.Write(settings[:])
	write(uint64(7))
	write(bump)
	write(uint64(4))
	write(uint64(3))
	write(uint32(1))
	b.Write(signer[:])
	write(uint8(7))
	write(uint16(1))
	write(uint32(0))
	write(uint8(3))
	write(uint8(0))
	if compact {
		write(uint8(1))
		b.Write(program[:])
		write(uint8(1))
		write(uint8(0))
		write(uint8(0))
		write(uint8(1))
	} else {
		write(uint32(1))
		b.Write(program[:])
		write(uint32(0))
		write(uint32(1))
	}
	write(uint64(0))
	write(uint8(5))
	write(uint32(2))
	b.Write([]byte{9, 8})
	write(uint8(0))
	write(uint8(0))
	write(uint8(0))
	if compact {
		write(uint8(0))
	} else {
		write(uint32(0))
	}
	write(int64(0))
	write(uint8(0))
	b.Write(make([]byte, 32))
	return b.Bytes()
}

func TestPolicyDecoderSyntheticLayoutsAndAuthority(t *testing.T) {
	for _, compact := range []bool{false, true} {
		data := syntheticPolicy(t, compact)
		decoded, err := squadspolicy.DecodeProgramInteractionPolicyAccount(data)
		if err != nil || decoded == nil || len(decoded.Payload.Constraints) != 1 || !bytes.Equal(decoded.Payload.Constraints[0].DataConstraints[0].DataValue.Bytes, []byte{9, 8}) {
			t.Fatalf("compact=%v valid policy: %v %v", compact, decoded, err)
		}
		for name, mutate := range map[string]func([]byte){
			"bump":           func(b []byte) { b[48] ^= 1 },
			"permissions":    func(b []byte) { b[101] = 3 },
			"threshold":      func(b []byte) { b[102] = 2 },
			"timelock":       func(b []byte) { b[104] = 1 },
			"stale_index":    func(b []byte) { binary.LittleEndian.PutUint64(b[57:65], 5) },
			"negative_start": func(b []byte) { binary.LittleEndian.PutUint64(b[len(b)-41:len(b)-33], ^uint64(0)) },
		} {
			t.Run(name, policyMutationTest(compact, data, mutate))
		}
		if compact {
			loose := append([]byte(nil), data[:143]...)
			loose = append(loose, make([]byte, 32)...)
			loose = append(loose, data[143:]...)
			loose[110] = 2
			if decoded, err := squadspolicy.DecodeProgramInteractionPolicyAccount(loose); err == nil && decoded != nil {
				t.Fatal("unused compact key granted authority")
			}
		}
		tail := len(data) - 41
		expiration := append([]byte(nil), data[:tail+8]...)
		expiration = append(expiration, 1, 0)
		expiration = append(expiration, make([]byte, 8)...)
		expiration = append(expiration, data[tail+9:]...)
		if decoded, err := squadspolicy.DecodeProgramInteractionPolicyAccount(expiration); err == nil && decoded != nil {
			t.Fatal("expiring policy granted authority")
		}
		for cut := 0; cut < len(data)-32; cut++ {
			decoded, err := squadspolicy.DecodeProgramInteractionPolicyAccount(data[:cut])
			if err == nil && decoded != nil {
				t.Fatalf("compact=%v accepted truncated policy at %d", compact, cut)
			}
		}
	}
}
func policyMutationTest(compact bool, data []byte, mutate func([]byte)) func(*testing.T) {
	return func(t *testing.T) {
		bad := append([]byte(nil), data...)
		mutate(bad)
		decoded, err := squadspolicy.DecodeProgramInteractionPolicyAccount(bad)
		if err == nil && decoded != nil {
			t.Fatalf("compact=%v unauthorized policy accepted", compact)
		}
	}
}
