package spl

import (
	"bytes"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func key(b byte) solana.PublicKey { return solana.PublicKey{31: b} }

// Each emitted instruction must keep the program, account flags and data the
// workers signed before these builders moved here.
func TestInstructionsKeepTheirWireLayout(t *testing.T) {
	payer, owner, mint, source, destination := key(1), key(2), key(3), key(4), key(5)
	ata, err := AssociatedTokenAddress(owner, mint, solana.Token2022ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	meta := func(k solana.PublicKey, signer, writable bool) *solana.AccountMeta {
		return &solana.AccountMeta{PublicKey: k, IsSigner: signer, IsWritable: writable}
	}
	for name, c := range map[string]struct {
		got      *solana.GenericInstruction
		program  solana.PublicKey
		accounts []*solana.AccountMeta
		data     []byte
	}{
		"create idempotent ATA": {CreateIdempotentATA(payer, owner, mint, solana.Token2022ProgramID), solana.SPLAssociatedTokenAccountProgramID,
			[]*solana.AccountMeta{meta(payer, true, true), meta(ata, false, true), meta(owner, false, false), meta(mint, false, false), meta(solana.SystemProgramID, false, false), meta(solana.Token2022ProgramID, false, false)}, []byte{1}},
		"transfer checked": {TransferChecked(solana.TokenProgramID, source, mint, destination, owner, 0x0102030405060708, 6), solana.TokenProgramID,
			[]*solana.AccountMeta{meta(source, false, true), meta(mint, false, false), meta(destination, false, true), meta(owner, true, false)}, []byte{12, 8, 7, 6, 5, 4, 3, 2, 1, 6}},
		"system transfer": {SystemTransfer(payer, owner, 0x0102), solana.SystemProgramID,
			[]*solana.AccountMeta{meta(payer, true, true), meta(owner, false, true)}, []byte{2, 0, 0, 0, 2, 1, 0, 0, 0, 0, 0, 0}},
		"compute unit limit": {SetComputeUnitLimit(300000), solana.ComputeBudget, nil, []byte{2, 0xe0, 0x93, 0x04, 0}},
		"compute unit price": {SetComputeUnitPrice(900), solana.ComputeBudget, nil, []byte{3, 0x84, 0x03, 0, 0, 0, 0, 0, 0}},
		"heap frame":         {RequestHeapFrame(65536), solana.ComputeBudget, nil, []byte{1, 0, 0, 1, 0}},
	} {
		if c.got.ProgID != c.program || !bytes.Equal(c.got.DataBytes, c.data) || len(c.got.AccountValues) != len(c.accounts) {
			t.Fatalf("%s: program %s data %x accounts %d", name, c.got.ProgID, c.got.DataBytes, len(c.got.AccountValues))
		}
		for i, want := range c.accounts {
			if *c.got.AccountValues[i] != *want {
				t.Fatalf("%s: account %d is %+v, want %+v", name, i, *c.got.AccountValues[i], *want)
			}
		}
	}
	if _, err := AssociatedTokenAddress(owner, mint, solana.SystemProgramID); err == nil {
		t.Fatal("an ATA under a non-token program was derived")
	}
}
