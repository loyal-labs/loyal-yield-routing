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
		{"deposit", OpenPrimeUSDCStep, kaminoLegDeposit, 17, "30e7488b04dfbbaee06c04eaf847763e8c8cd63df6c5625581014f3c4b423e14", "646fa285176a4c0a84679bf382eee6060e345294dd76deca4147279ea88ceb09", 839},
		{"borrow", OpenPrimeUSDCStep, kaminoLegBorrow, 15, "fd18dd1a112f3d58a5ca97cd9916a3d5526ea8cb1a8aab13b235ca78e53dd7b9", "6fc1b3d770f26be60dea2dca64c0d7c77cdf7c73e505fc4876f95da36cd6b615", 805},
		{"repay", DeleverPrimeUSDCStep, kaminoLegRepay, 13, "0c66ccfa074661083ea5e0bc00900d7b01d326c850ff9e49de758b62c02eb4dd", "d8ffa509898674be1d3da091ba46dfcabaf62cba1e8e9d4e3bd0aa2fb93d6309", 771},
		{"withdraw", DeleverPrimeUSDCStep, kaminoLegWithdraw, 17, "a6c9fdfd7cb0c65bc1fd7e6e3d7969624e0d7d4acd981189a92b76f14db18dc2", "2e38a54a73fcd925e8746c2a3c2374de34bccf144c824b1734241fb6efa8689c", 840},
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
