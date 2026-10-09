package backyard

import (
	"encoding/binary"
	"testing"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

func testPublicKey(seed byte) string {
	value := make([]byte, 32)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return encodeBase58(value)
}

func receiptFixture(t *testing.T, vault, user string, amountLP uint64, amountBits uint64) (string, []byte) {
	t.Helper()
	vaultKey, userKey := solana.MustPublicKeyFromBase58(vault), solana.MustPublicKeyFromBase58(user)
	address, bump, err := voltr.WithdrawalReceiptAddress(vaultKey, userKey)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, voltr.WithdrawalReceiptSize)
	copy(data[:8], voltr.WithdrawalReceiptDiscriminator[:])
	copy(data[8:40], vaultKey[:])
	copy(data[40:72], userKey[:])
	binary.LittleEndian.PutUint64(data[72:80], amountLP)
	binary.LittleEndian.PutUint64(data[80:88], amountBits)
	binary.LittleEndian.PutUint64(data[96:104], 1_700_000_000)
	data[104], data[105] = bump, 0
	return address.String(), data
}

func TestDecodeVoltrWithdrawalReceiptRequiresExactDeployedLayoutAndPDA(t *testing.T) {
	program := voltr.ProgramID.String()
	vault, user := testPublicKey(11), testPublicKey(44)
	address, data := receiptFixture(t, vault, user, 7, 3<<48)
	decoded, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: address, Owner: program, Lamports: 1, Data: data}, address, vault, 10)
	if err != nil || decoded.User.String() != user || decoded.LPEscrowedRaw != 7 || decoded.UpperBoundAssetRaw != 3 || decoded.Version != 0 {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	badPadding := append([]byte(nil), data...)
	badPadding[111] = 1
	if _, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: address, Owner: program, Lamports: 1, Data: badPadding}, address, vault, 10); err == nil {
		t.Fatal("trailing deployed padding was accepted")
	}
	if _, err := DecodeVoltrWithdrawalReceipt(ConfirmedAccount{Address: testPublicKey(99), Owner: program, Lamports: 1, Data: data}, testPublicKey(99), vault, 10); err == nil {
		t.Fatal("noncanonical receipt address was accepted")
	}
}
