package backyard

import (
	"encoding/binary"
	"testing"
)

func testPublicKey(seed byte) string {
	value := make([]byte, 32)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return encodeBase58(value)
}

func receiptFixture(t *testing.T, program, vault, user string, amountLP uint64, amountBits uint64) (string, []byte) {
	t.Helper()
	address, bump, err := deriveVoltrWithdrawalReceiptPDA(program, vault, user)
	if err != nil {
		t.Fatal(err)
	}
	vaultKey, _ := decodeBase58PublicKey(vault)
	userKey, _ := decodeBase58PublicKey(user)
	data := make([]byte, voltrWithdrawalReceiptDataLength)
	copy(data[:8], voltrWithdrawalReceiptDiscriminator[:])
	copy(data[8:40], vaultKey[:])
	copy(data[40:72], userKey[:])
	binary.LittleEndian.PutUint64(data[72:80], amountLP)
	binary.LittleEndian.PutUint64(data[80:88], amountBits)
	binary.LittleEndian.PutUint64(data[96:104], 1_700_000_000)
	data[104], data[105] = bump, 0
	return address, data
}

func TestDecodeVoltrWithdrawalReceiptRequiresExactDeployedLayoutAndPDA(t *testing.T) {
	program := "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
	vault, user := testPublicKey(11), testPublicKey(44)
	address, data := receiptFixture(t, program, vault, user, 7, 3<<48)
	decoded, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: address, Owner: program, Lamports: 1, Data: data}, address, program, vault, 10)
	if err != nil || decoded.User != user || decoded.AmountLPEscrowed != 7 || decoded.UpperBoundAssetRaw != 3 || decoded.Version != 0 {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	badPadding := append([]byte(nil), data...)
	badPadding[111] = 1
	if _, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: address, Owner: program, Lamports: 1, Data: badPadding}, address, program, vault, 10); err == nil {
		t.Fatal("trailing deployed padding was accepted")
	}
	if _, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: testPublicKey(99), Owner: program, Lamports: 1, Data: data}, testPublicKey(99), program, vault, 10); err == nil {
		t.Fatal("noncanonical receipt address was accepted")
	}
}

func TestVoltrWithdrawalReceiptPDAMatchesSDKVector(t *testing.T) {
	address, bump, err := deriveVoltrWithdrawalReceiptPDA(
		"vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8",
		"9pnHBxUqgspqQjeVtFj9qHPGPGXRvc1qC5SDjMChSuuW",
		"4vJ9JU1bJJ34wKnjrFqrGd5bdDhxFqSMozMDeM4V5UuQ",
	)
	if err != nil || address != "BbpPz4dapzgmaZ28jwZRYwF4ZePgj7wXqK6FDaDDYpEz" || bump != 255 {
		t.Fatalf("PDA=%s bump=%d err=%v", address, bump, err)
	}
}
