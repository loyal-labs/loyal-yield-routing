package backyardrwa

import (
	"errors"
	"testing"
)

func TestKaminoFarmSetupInstructionUsesPayerOnlySigner(t *testing.T) {
	request := KaminoFarmSetupRequest{
		Payer:                   bridgeVault,
		Owner:                   bridgeVault,
		Obligation:              "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei",
		LendingMarket:           "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
		LendingMarketAuthority:  "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
		Reserve:                 "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
		ReserveFarmState:        onreDebtFarmState,
		ObligationFarmUserState: "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD",
	}
	instruction, err := BuildKaminoFarmSetupInstruction(request)
	if err != nil {
		t.Fatal(err)
	}
	if instruction.Program != kaminoProgram || len(instruction.Data) != 9 || instruction.Data[8] != kaminoFarmSetupMode {
		t.Fatalf("unexpected farm setup instruction: %+v", instruction)
	}
	if len(instruction.Accounts) != 11 || !instruction.Accounts[0].Signer || !instruction.Accounts[0].Writable {
		t.Fatalf("payer meta drifted: %+v", instruction.Accounts)
	}
	for index, account := range instruction.Accounts[1:] {
		if account.Signer {
			t.Fatalf("account %d unexpectedly signs", index+1)
		}
	}
	if err := kaminoFarmSetupHold("OnRe/ONyc/USDC", request.ObligationFarmUserState, false); err == nil {
		t.Fatal("missing farm user state did not produce a typed HOLD")
	} else {
		var hold *KaminoFarmSetupHold
		if !errors.As(err, &hold) || hold.Lane != "OnRe/ONyc/USDC" {
			t.Fatalf("unexpected farm setup hold: %v", err)
		}
	}
	if err := kaminoFarmSetupHold("OnRe/ONyc/USDC", request.ObligationFarmUserState, true); err != nil {
		t.Fatal(err)
	}
}
