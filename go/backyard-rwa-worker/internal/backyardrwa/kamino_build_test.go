package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func manifestAccounts(metas []accountMeta) KaminoPrimeUSDCAccounts {
	accounts := make(KaminoPrimeUSDCAccounts, len(metas))
	for index, value := range metas {
		accounts[index].Address = encodeBase58(value.key[:])
		accounts[index].Signer = value.signer
		accounts[index].Writable = value.writable
	}
	return accounts
}

func kaminoTestRequest(action Action, leg kaminoPrimeUSDCLeg) KaminoPrimeUSDCRequest {
	var discriminator []byte
	var metas []accountMeta
	switch leg {
	case kaminoLegDeposit:
		discriminator, metas = kaminoDepositCollateral, kaminoDepositMetas()
	case kaminoLegBorrow:
		discriminator, metas = kaminoBorrowUSDC, kaminoBorrowMetas()
	case kaminoLegRepay:
		discriminator, metas = kaminoRepayUSDC, kaminoRepayMetas()
	case kaminoLegWithdraw:
		discriminator, metas = kaminoWithdrawCollateral, kaminoWithdrawMetas()
	default:
		panic("unknown test leg")
	}
	data := append([]byte(nil), discriminator...)
	data = appendU64(data, 1_000_000)
	return KaminoPrimeUSDCRequest{
		Action: action, AmountRaw: 1_000_000, Policy: bridgeAllocationPolicy,
		PolicyConstraintIndex: kaminoConstraintIndex(leg), PolicyAccountDataSHA256: hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		Accounts: manifestAccounts(metas), Data: data, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
	}
}

func TestKaminoPrimeUSDCBuilderPinsAllFourV2SDKLegsAndRefreshes(t *testing.T) {
	tests := []struct {
		name          string
		action        Action
		leg           kaminoPrimeUSDCLeg
		count         int
		messageSHA256 string
		wireSHA256    string
		packetBytes   int
	}{
		{"deposit", OpenPrimeUSDCStep, kaminoLegDeposit, 17, "8e46df1a45b3fbd9b01b64b3c93a62d333f919b256d977a3a8ec04fa74e6cfa7", "dabd065420a8b37f1fae9ac6a61eac8abea3a45a5f8440c1d5de95e12b1a613a", 839},
		{"borrow", OpenPrimeUSDCStep, kaminoLegBorrow, 15, "44eabb6ba27542bb25d49fdb513bafa8cc921cd983a0b8e14048fc065010bc0d", "d6cd4445c732dea1b9936cee33220ff5a8333a377fe2bf6e386d9082aafca0a6", 805},
		{"repay", DeleverPrimeUSDCStep, kaminoLegRepay, 13, "14629c60baefa79a743e9f3b8b1fed6a9b1717049668c196a2c41b1e8fe9c7f9", "cdf7a5c3e138644b10959d401ce917a3b801d87cd98a3405c0c0f45a38291c00", 771},
		{"withdraw", DeleverPrimeUSDCStep, kaminoLegWithdraw, 17, "6f70c3d20ee0bd9e7aca20b514229ec957e3b3037f6059b4c406d55af32b654b", "7cb7a6f6f7231c1eda85606ec7b3c94c3e03ba55307637af3004a6871eb28697", 840},
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := kaminoTestRequest(test.action, test.leg)
			inner, leg, err := kaminoPrimeUSDCInstruction(request)
			if err != nil {
				t.Fatal(err)
			}
			if leg != test.leg || inner.program != mustKey(kaminoPrimeUSDCProgram) || len(inner.accounts) != test.count {
				t.Fatal("did not build the pinned Kamino PRIME/USDC leg")
			}
			refresh := kaminoPrimeUSDCRefreshInstructions(leg)
			if len(refresh) != 3 || !bytes.Equal(refresh[0].data, kaminoRefreshReserve) ||
				!bytes.Equal(refresh[1].data, kaminoRefreshReserve) || !bytes.Equal(refresh[2].data, kaminoRefreshObligation) {
				t.Fatal("canonical KLend refresh prefix drifted")
			}
			signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
			if err != nil {
				t.Fatal(err)
			}
			if len(signed.signedWire) > solanaPacketBytes || !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:1+ed25519.SignatureSize]) {
				t.Fatal("Kamino exact wire signature or packet boundary is invalid")
			}
			if signed.messageSHA256 != test.messageSHA256 || signed.signedWireSHA256 != test.wireSHA256 || len(signed.signedWire) != test.packetBytes {
				t.Fatalf("Kamino fixture fingerprint drifted: message=%s wire=%s bytes=%d", signed.messageSHA256, signed.signedWireSHA256, len(signed.signedWire))
			}
		})
	}
}

func TestKaminoPrimeUSDCBuilderFailsClosedOnEveryAuthorityBoundary(t *testing.T) {
	request := kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[2].Address = bridgeSettings
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lending market accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[16].Address = kaminoPrimeUSDCProgram
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong farms program accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Accounts[9].Writable = false
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong custody role accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.PolicyConstraintIndex = 2
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lane constraint index accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Data[8]++
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("amount mutation accepted")
	}
	request = kaminoTestRequest(DeleverPrimeUSDCStep, kaminoLegDeposit)
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("open deposit accepted as delever")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.Data[0] = 129
	if _, _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("historical non-v2 discriminator accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep, kaminoLegDeposit)
	request.PolicyAccountDataSHA256 = "not-a-hash"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	if _, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate); err == nil {
		t.Fatal("unbound policy bytes accepted")
	}
}
