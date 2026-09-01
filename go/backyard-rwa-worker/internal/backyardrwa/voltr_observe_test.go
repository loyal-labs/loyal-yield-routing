package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func TestScanVoltrWithdrawalDemandRetriesUntilReceiptsAndCustodyShareSlot(t *testing.T) {
	program := "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
	vault, user, mint, authority, idle := testPublicKey(11), testPublicKey(44), testPublicKey(77), testPublicKey(101), testPublicKey(133)
	receiptAddress, receiptData := receiptFixture(t, program, vault, user, 7, 3<<48)
	custody := custodyFixture(testKey(t, mint), testKey(t, authority), 4, false)
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		requestBody := string(body)
		switch {
		case strings.Contains(requestBody, `"method":"getSlot"`):
			return response(`{"jsonrpc":"2.0","id":1,"result":90}`), nil
		case strings.Contains(requestBody, `"method":"getProgramAccounts"`):
			if !strings.Contains(requestBody, `"commitment":"confirmed"`) || !strings.Contains(requestBody, `"withContext":true`) || !strings.Contains(requestBody, `"bytes":"`) {
				t.Fatalf("receipt scan lost canonical confirmed filters: %s", requestBody)
			}
			slot := 91
			if strings.Contains(requestBody, `"minContextSlot":92`) {
				slot = 93
			}
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":[{"pubkey":"%s","account":{"owner":"%s","lamports":1,"data":["%s","base64"],"executable":false}}]}}`, slot, receiptAddress, program, base64.StdEncoding.EncodeToString(receiptData))), nil
		case strings.Contains(requestBody, `"method":"getMultipleAccounts"`):
			if !strings.Contains(requestBody, `"commitment":"confirmed"`) {
				t.Fatalf("custody read lost confirmed commitment: %s", requestBody)
			}
			slot := 92
			if strings.Contains(requestBody, `"minContextSlot":92`) {
				slot = 93
			}
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":[{"owner":"%s","lamports":1,"data":["%s","base64"],"executable":false}]}}`, slot, classicTokenProgram, base64.StdEncoding.EncodeToString(custody))), nil
		default:
			t.Fatalf("unexpected RPC request: %s", requestBody)
			return nil, nil
		}
	})
	result, err := client.ScanVoltrWithdrawalDemand(context.Background(), VoltrObservationConfig{
		VoltrProgram: program, Vault: vault, IdleCustody: idle,
		Custodies:    []TokenCustodySpec{{Address: idle, TokenProgram: classicTokenProgram, Mint: mint, Authority: authority}},
		IdleFloorRaw: 2, VaultCapRaw: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Slot != 93 || result.ConfirmedIdleRaw != 4 || result.PendingWithdrawalUpperBound != 3 || result.RequiredIdleRaw != 5 || result.IdleShortfallRaw != 1 || len(result.Receipts) != 1 {
		t.Fatalf("unexpected aligned demand result: %+v", result)
	}
}

func testKey(t *testing.T, encoded string) [32]byte {
	t.Helper()
	value, err := decodeBase58PublicKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
