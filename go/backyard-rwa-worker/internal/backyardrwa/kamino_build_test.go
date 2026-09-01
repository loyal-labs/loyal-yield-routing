package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func kaminoTestRequest(action Action) KaminoPrimeUSDCRequest {
	accounts := KaminoPrimeUSDCAccounts{
		{Address: bridgeVault, Signer: true},
		{Address: bridgeSettings, Writable: true},
		{Address: kaminoPrimeMarket},
		{Address: bridgeSettings},
		{Address: kaminoPrimeReserve, Writable: true},
		{Address: kaminoPrimeUSDCCollateralMint},
		{Address: bridgeSettings, Writable: true},
		{Address: bridgeSettings, Writable: true},
		{Address: bridgeSettings, Writable: true},
		{Address: bridgeSettings, Writable: true},
		{Address: kaminoPrimeUSDCProgram},
		{Address: bridgeTokenProgram},
		{Address: bridgeTokenProgram},
		{Address: kaminoInstructions},
	}
	data := append([]byte(nil), kaminoDepositCollateral...)
	data = appendU64(data, 1_000_000)
	return KaminoPrimeUSDCRequest{
		Action: action, AmountRaw: 1_000_000, Policy: bridgeAllocationPolicy,
		PolicyAccountDataSHA256: hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		Accounts:                accounts, Data: data, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
	}
}

func TestKaminoPrimeUSDCBuilderSignsExactPolicyWrappedDeposit(t *testing.T) {
	request := kaminoTestRequest(OpenPrimeUSDCStep)
	inner, err := kaminoPrimeUSDCInstruction(request)
	if err != nil {
		t.Fatal(err)
	}
	if inner.program != mustKey(kaminoPrimeUSDCProgram) || len(inner.accounts) != 14 || !bytes.Equal(inner.data[:8], kaminoDepositCollateral) {
		t.Fatal("did not build the pinned Kamino PRIME collateral deposit")
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), signed.message, signed.signedWire[1:1+ed25519.SignatureSize]) {
		t.Fatal("Kamino exact wire signature is invalid")
	}
	build, err := signed.BuildResult(123)
	if err != nil || build.SimulationSlot != 123 {
		t.Fatalf("build result: %v", err)
	}
}

func TestKaminoPrimeUSDCBuilderFailsClosedOnRouteAndPacketMutation(t *testing.T) {
	request := kaminoTestRequest(OpenPrimeUSDCStep)
	request.Accounts[2].Address = bridgeSettings
	if _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("wrong lending market accepted")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep)
	request.Data[8]++
	if _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("amount mutation accepted")
	}
	request = kaminoTestRequest(DeleverPrimeUSDCStep)
	if _, err := kaminoPrimeUSDCInstruction(request); err == nil {
		t.Fatal("open deposit accepted as delever")
	}
	request = kaminoTestRequest(OpenPrimeUSDCStep)
	request.PolicyAccountDataSHA256 = "not-a-hash"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	if _, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate); err == nil {
		t.Fatal("unbound policy bytes accepted")
	}
}
