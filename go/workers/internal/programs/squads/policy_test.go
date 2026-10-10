package squads

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

// Synthetic ABI examples complement the independent policy-bank proof. They
// deliberately encode bytes without calling the production policy decoder.
func syntheticPolicy(t *testing.T, compact bool) []byte {
	t.Helper()
	settings, signer, program := solana.PublicKey{1}, solana.PublicKey{2}, solana.PublicKey{3}
	_, bump, err := PolicyAddress(settings, 7)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	write := func(value any) {
		if err := binary.Write(&b, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	b.Write(PolicyDiscriminator[:])
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
		decoded, err := DecodeCanonicalPolicy(&chain.Account{Owner: ProgramID, Data: data})
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
			if decoded, err := DecodeCanonicalPolicy(&chain.Account{Owner: ProgramID, Data: loose}); err == nil && decoded != nil {
				t.Fatal("unused compact key granted authority")
			}
		}
		tail := len(data) - 41
		expiration := append([]byte(nil), data[:tail+8]...)
		expiration = append(expiration, 1, 0)
		expiration = append(expiration, make([]byte, 8)...)
		expiration = append(expiration, data[tail+9:]...)
		if decoded, err := DecodeCanonicalPolicy(&chain.Account{Owner: ProgramID, Data: expiration}); err == nil && decoded != nil {
			t.Fatal("expiring policy granted authority")
		}
		for cut := 0; cut < len(data)-32; cut++ {
			decoded, err := DecodeCanonicalPolicy(&chain.Account{Owner: ProgramID, Data: data[:cut]})
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
		decoded, err := DecodeCanonicalPolicy(&chain.Account{Owner: ProgramID, Data: bad})
		if err == nil && decoded != nil {
			t.Fatalf("compact=%v unauthorized policy accepted", compact)
		}
	}
}

// A policy may carry spending limits: the decoder returns them with their
// allowance rules, and FindPolicy matches it only by a literal that states the
// same vault index, constraints and limits. Squads' usage counters are running
// state, so an overspent window is still the same policy.
func TestFindPolicyComparesVaultIndexAndSpendingLimits(t *testing.T) {
	data := syntheticPolicy(t, false)
	mint := solana.PublicKey{9}
	limit := func(accumulate uint8, maxPerUse uint64, exact uint8, remaining uint64) []byte {
		var b bytes.Buffer
		b.Write(mint[:])
		for _, value := range []any{int64(5), uint8(0), uint8(1), accumulate, uint64(100), maxPerUse, exact, remaining, int64(5)} {
			if err := binary.Write(&b, binary.LittleEndian, value); err != nil {
				t.Fatal(err)
			}
		}
		return b.Bytes()
	}
	limited := func(limit []byte) []Installed {
		count := len(data) - 41 - 4 // the u32 limit count, before the account tail
		out := append(append([]byte(nil), data[:count]...), 1, 0, 0, 0)
		out = append(append(out, limit...), data[count+4:]...)
		account := &chain.Account{Key: solana.PublicKey{10}, Owner: ProgramID, Data: out}
		decoded, err := DecodeCanonicalPolicy(account)
		if err != nil || decoded == nil {
			t.Fatalf("limited policy does not decode: %v", err)
		}
		return []Installed{{Account: account.Key, View: decoded}}
	}

	installed := limited(limit(0, 0, 0, 40))
	view := installed[0].View
	plain := []SpendingLimitView{{Mint: mint, Period: 1, MaxPerPeriod: 100}}
	want := Policy{VaultIndex: 0, Constraints: view.Payload.Constraints, SpendingLimits: plain}
	if found, n := FindPolicy(installed, view.Settings, view.DelegatedSigner, want); n != 1 || found.Account != installed[0].Account {
		t.Fatal("the limited literal did not find its policy")
	}
	if _, n := FindPolicy(installed, view.Settings, view.DelegatedSigner, Policy{Constraints: want.Constraints}); n != 0 {
		t.Fatal("a literal without limits matched a limited policy")
	}
	if _, n := FindPolicy(installed, view.Settings, view.DelegatedSigner, Policy{VaultIndex: 1, Constraints: want.Constraints, SpendingLimits: plain}); n != 0 {
		t.Fatal("a literal for another vault matched")
	}
	if _, n := FindPolicy(installed, solana.PublicKey{11}, view.DelegatedSigner, want); n != 0 {
		t.Fatal("a policy on other settings matched")
	}
	if _, n := FindPolicy(limited(limit(0, 0, 0, 101)), view.Settings, view.DelegatedSigner, want); n != 1 {
		t.Fatal("an overspent window changed what the policy authorizes")
	}
	twice := append(limited(limit(0, 0, 0, 40)), Installed{Account: solana.PublicKey{12}, View: view})
	if found, n := FindPolicy(twice, view.Settings, view.DelegatedSigner, want); n != 2 || found != (Installed{}) {
		t.Fatal("a policy installed twice was found")
	}
	for name, c := range map[string]struct {
		account []byte
		limit   SpendingLimitView
	}{
		"accumulate":     {limit(1, 0, 0, 40), SpendingLimitView{Mint: mint, Period: 1, MaxPerPeriod: 100, Accumulate: true}},
		"per_use":        {limit(0, 10, 0, 40), SpendingLimitView{Mint: mint, Period: 1, MaxPerPeriod: 100, MaxPerUse: 10}},
		"exact_quantity": {limit(0, 10, 1, 40), SpendingLimitView{Mint: mint, Period: 1, MaxPerPeriod: 100, MaxPerUse: 10, ExactQuantity: true}},
	} {
		policy := limited(c.account)
		if _, n := FindPolicy(policy, view.Settings, view.DelegatedSigner, want); n != 0 {
			t.Fatalf("%s: a plain literal matched", name)
		}
		stated := want
		stated.SpendingLimits = []SpendingLimitView{c.limit}
		if _, n := FindPolicy(policy, view.Settings, view.DelegatedSigner, stated); n != 1 {
			t.Fatalf("%s: the literal stating the limit did not match", name)
		}
	}
}

// Admits checks every predicate kind Squads has: integers of each width by
// each operator, bytes by (in)equality, and an account constrained by its
// data and owner, which it reads from the account's state.
func TestAdmitsReadsEveryPredicate(t *testing.T) {
	owner, program, account := solana.PublicKey{4}, solana.PublicKey{3}, solana.PublicKey{5}
	data := []byte{7, 0x34, 0x12, 1, 0, 0, 0, 9, 9}
	ix := Instruction{ProgramID: program, Accounts: []solana.AccountMeta{{PublicKey: account}}, Data: data}
	state := func(key solana.PublicKey) *chain.Account {
		if key != account {
			return nil
		}
		return &chain.Account{Key: key, Owner: owner, Data: []byte{0, 0, 42}}
	}
	ownerKey := owner
	admitted := InstructionConstraintView{ProgramID: program,
		AccountConstraints: []AccountConstraintView{{AccountIndex: 0, AccountData: []DataConstraintView{DataU8(2, OpEquals, 42)}, Owner: &ownerKey}},
		DataConstraints: []DataConstraintView{DataU8(0, OpEquals, 7), DataU16(1, OpGreaterThan, 0x1233), DataU16(1, OpLessThanOrEqualTo, 0x1234),
			{DataOffset: 3, DataValue: DataValueView{Kind: 2, U32: 2}, Operator: OpLessThan}, DataBytes(7, []byte{9, 9}),
			{DataOffset: 7, DataValue: DataValueView{Kind: 5, Bytes: []byte{9, 8}}, Operator: OpNotEquals}}}
	if !Admits(admitted, ix, state) {
		t.Fatal("an admitted instruction was refused")
	}
	for name, refuse := range map[string]func(*InstructionConstraintView){
		"u16 bound": func(c *InstructionConstraintView) { c.DataConstraints[2] = DataU16(1, OpLessThan, 0x1234) },
		"past data": func(c *InstructionConstraintView) { c.DataConstraints[4] = DataBytes(8, []byte{9, 9}) },
		"owner": func(c *InstructionConstraintView) {
			other := solana.PublicKey{6}
			c.AccountConstraints[0].Owner = &other
		},
		"account data": func(c *InstructionConstraintView) {
			c.AccountConstraints[0].AccountData = []DataConstraintView{DataU8(2, OpNotEquals, 42)}
		},
		"pinned key": func(c *InstructionConstraintView) {
			c.AccountConstraints[0] = AccountConstraintView{AccountIndex: 0, Pubkeys: []solana.PublicKey{owner}}
		},
	} {
		c := admitted
		c.AccountConstraints = append([]AccountConstraintView(nil), admitted.AccountConstraints...)
		c.DataConstraints = append([]DataConstraintView(nil), admitted.DataConstraints...)
		refuse(&c)
		if Admits(c, ix, state) {
			t.Fatalf("%s: a refused instruction was admitted", name)
		}
	}
	if Admits(admitted, ix, nil) {
		t.Fatal("an account data predicate admitted an account of unknown state")
	}
}
