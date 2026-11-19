package referral

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func harvestTestQuote(t *testing.T) (HarvestQuote, solana.PrivateKey) {
	t.Helper()
	seed := sha256.Sum256([]byte("local referral harvest compatibility fixture"))
	key := solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:]))
	a, err := harvestAccounts()
	if err != nil {
		t.Fatal(err)
	}
	q := HarvestQuote{Slot: 101, AccruedRaw: 74063, payer: key.PublicKey(), addresses: a, state: feeState{Accrued: 74063, Minted: 210000001, Dead: 1000},
		balances: [3]uint64{11, 22, 33}}
	return q, key
}

func TestHarvestSignedWireUsesDeployedProtocolTreasuryAndSingleOrdinarySigner(t *testing.T) {
	q, key := harvestTestQuote(t)
	q.missing = [3]bool{true, false, true}
	hash := solana.Hash{12, 34}
	w, err := q.wire(hash, 300, 101)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := key.Sign(w.Message)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := w.Signed(sig)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifySignatures(); err != nil {
		t.Fatal(err)
	}
	if len(tx.Signatures) != 1 || tx.Message.Header.NumRequiredSignatures != 1 || tx.Message.AccountKeys[0] != q.payer || len(tx.Message.Instructions) != 3 {
		t.Fatal("unexpected signers or harvest instructions")
	}
	// Addresses independently derived from the SDK 2.1.1 seeds and
	// finalized mainnet Protocol account at slot 471449340.
	want := []string{q.payer.String(), harvestManager.String(), feeAdmin.String(), "C7sE3MjSAqqF7TgXn1VsNQPWem1gdhqv3ZYV9TNfSjY9",
		"4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y", earnmax.Vault.String(), earnmax.LPMint.String(),
		"HHM86gQUM7rN8bz2VPWhqNn7ZcfznLC5KyT7kLs569yq", "5hBYmhggBC1kjojG6BAyoSsUfBFi6S142oM6SCnwqb1o",
		"ZvsW29zAXZwMayzP9jAVBryRi5rt6X7em5vYdhKGbvZ", "EZyfaXz4VjM4iESubC9U6NPT8fbQy1zbNDC2URpNa4em", solana.TokenProgramID.String()}
	harvest := tx.Message.Instructions[2]
	if tx.Message.AccountKeys[harvest.ProgramIDIndex] != voltr.ProgramID || !bytes.Equal(harvest.Data, []byte{32, 59, 42, 128, 246, 73, 255, 47}) ||
		len(harvest.Accounts) != len(want) {
		t.Fatal("unexpected harvest instruction")
	}
	for i, idx := range harvest.Accounts {
		if got := tx.Message.AccountKeys[idx].String(); got != want[i] {
			t.Fatalf("harvest account %d = %s, want %s", i, got, want[i])
		}
	}
	for i, recipient := range []int{8, 10} {
		ix := tx.Message.Instructions[i]
		if tx.Message.AccountKeys[ix.ProgramIDIndex] != solana.SPLAssociatedTokenAccountProgramID || !bytes.Equal(ix.Data, []byte{1}) ||
			tx.Message.AccountKeys[ix.Accounts[1]].String() != want[recipient] {
			t.Fatalf("unexpected ATA creation %d", i)
		}
	}
	if err := ValidateHarvestWire(wire, q.payer, solana.Hash{99}); err == nil {
		t.Fatal("persisted mismatched blockhash accepted")
	}
	for _, privileged := range []solana.PublicKey{feeAdmin, harvestManager, protocolAdmin, protocolTreasury, harvestWorkerDelegate} {
		if ordinaryHarvestPayer(privileged) {
			t.Fatalf("accepted privileged payer %s", privileged)
		}
	}
	// Retain a valid signature while redirecting the admin mint account.
	tx.Message.Instructions[2].Accounts[9] = harvest.Accounts[10]
	changed, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	otherSig, err := key.Sign(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHarvestWire(harvestPacket(otherSig, changed), q.payer, hash); err == nil {
		t.Fatal("accepted re-signed treasury redirection")
	}
	if _, err := w.Signed(otherSig); err == nil {
		t.Fatal("accepted signature over another message")
	}
}

func TestHarvestRequiresFullAccumulatorInOwnEventAndLoyalLPAccount(t *testing.T) {
	q, _ := harvestTestQuote(t)
	// The quote combines an opening reserve of 73,763 and 300 newly assessed shares.
	vault := &chain.Account{Key: earnmax.Vault, Owner: voltr.ProgramID, Lamports: 1, Data: make([]byte, voltr.VaultSize)}
	copy(vault.Data, voltr.VaultDiscriminator[:])
	copy(vault.Data[104:], earnmax.AssetMint[:])
	copy(vault.Data[272:], earnmax.LPMint[:])
	copy(vault.Data[368:], harvestManager[:])
	copy(vault.Data[400:], feeAdmin[:])
	binary.LittleEndian.PutUint16(vault.Data[514:], 1500)
	binary.LittleEndian.PutUint16(vault.Data[522:], 40)
	binary.LittleEndian.PutUint64(vault.Data[616:], q.state.Dead)
	mint := &chain.Account{Key: earnmax.LPMint, Owner: solana.TokenProgramID, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[0] = 1
	copy(mint.Data[4:], q.addresses.mintAuth[:])
	binary.LittleEndian.PutUint64(mint.Data[36:], q.state.Minted+q.AccruedRaw)
	mint.Data[45] = 1
	accounts := []*chain.Account{vault, mint}
	for i, owner := range harvestOwners {
		ata := &chain.Account{Key: q.addresses.atas[i], Owner: solana.TokenProgramID, Lamports: 1, Data: make([]byte, 165)}
		copy(ata.Data, earnmax.LPMint[:])
		copy(ata.Data[32:], owner[:])
		ata.Data[108] = 1
		amount := q.balances[i]
		if owner == feeAdmin {
			amount += q.AccruedRaw
		}
		binary.LittleEndian.PutUint64(ata.Data[64:], amount)
		accounts = append(accounts, ata)
	}
	if err := q.checkEffects(accounts); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accounts[3].Data[64:], q.balances[1]+300)
	if err := q.checkEffects(accounts); err == nil {
		t.Fatal("accepted short admin LP credit")
	}
	binary.LittleEndian.PutUint64(accounts[3].Data[64:], q.balances[1]+q.AccruedRaw)
	binary.LittleEndian.PutUint16(vault.Data[514:], 1000)
	if err := q.checkEffects(accounts); err == nil {
		t.Fatal("accepted changed vault fee")
	}
	event := make([]byte, 192)
	d := sha256.Sum256([]byte("event:HarvestFeeEvent"))
	copy(event, d[:8])
	copy(event[8:], q.payer[:])
	copy(event[40:], earnmax.Vault[:])
	copy(event[104:], feeAdmin[:])
	copy(event[136:], harvestManager[:])
	logs := []string{"Program " + voltr.ProgramID.String() + " invoke [1]", "", "Program " + voltr.ProgramID.String() + " success"}
	binary.LittleEndian.PutUint64(event[168:], q.AccruedRaw)
	logs[1] = "Program data: " + base64.StdEncoding.EncodeToString(event)
	if err := q.checkHarvestEvent(logs); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(event[168:], 0)
	logs[1] = "Program data: " + base64.StdEncoding.EncodeToString(event)
	if err := q.checkHarvestEvent(logs); err == nil {
		t.Fatal("accepted another collector's treasury credit with zero mint from our harvest")
	}
}
