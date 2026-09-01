package backyardrwa

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
)

func exactAdaptorConfigAccount(t *testing.T) ConfirmedAccount {
	t.Helper()
	data := make([]byte, adaptorConfigLength)
	copy(data[:8], adaptorConfigDiscriminator)
	data[8] = 2
	bindings := []string{
		bridgeVoltrProgram, bridgeVoltrVault, bridgeStrategy, bridgeStrategyAuth,
		bridgeSquadsProgram, bridgeSettings, bridgeSettingsSigner, bridgeVault,
		bridgeUSDC, bridgeTokenProgram, bridgeSquadsATA,
	}
	for index, binding := range bindings {
		key, err := decodeBase58PublicKey(binding)
		if err != nil {
			t.Fatal(err)
		}
		copy(data[16+index*32:48+index*32], key[:])
	}
	binary.LittleEndian.PutUint64(data[400:408], bridgeMaxNAV)
	binary.LittleEndian.PutUint64(data[408:416], 32)
	return ConfirmedAccount{Address: bridgeStrategy, Owner: bridgeAdaptorProgram, Lamports: 1, Data: data}
}

func TestExecutionObserverDecodesOnlyExactAdaptorV2Bindings(t *testing.T) {
	account := exactAdaptorConfigAccount(t)
	if _, err := decodeObservedAdaptorConfig(account); err != nil {
		t.Fatal(err)
	}
	account.Data[176] ^= 1
	if _, err := decodeObservedAdaptorConfig(account); err == nil {
		t.Fatal("drifted Squads Settings binding was accepted")
	}
	account = exactAdaptorConfigAccount(t)
	account.Data[416] = 1
	if _, err := decodeObservedAdaptorConfig(account); err == nil {
		t.Fatal("mutable report state appeared in immutable adaptor config")
	}
}

func TestBridgeExpectedEffectsAreExactAndConserved(t *testing.T) {
	effects, strategyAfter, squadsAfter, err := bridgeExpectedEffects(
		Decision{Action: VoltrAllocateToSquads, AmountRaw: 4}, 10, 7, 3,
	)
	if err != nil || strategyAfter != 7 || squadsAfter != 7 || len(effects.Accounts) != 3 {
		t.Fatalf("effects=%+v strategy=%d squads=%d err=%v", effects, strategyAfter, squadsAfter, err)
	}
	if effects.Accounts[0].AfterRaw != 6 || effects.Accounts[1].AfterRaw != 7 || effects.Accounts[2].AfterRaw != 7 {
		t.Fatal("allocation effect graph drifted")
	}
	if _, _, _, err := bridgeExpectedEffects(Decision{Action: VoltrRestoreIdle, AmountRaw: 8}, 10, 7, 3); err == nil {
		t.Fatal("bridge observer accepted an underfunded restore")
	}
}

func TestExpectedAdaptorReturnDataIsExactNAVLittleEndian(t *testing.T) {
	expected := expectedAdaptorReturnData(0x0807060504030201)
	decoded, err := base64.StdEncoding.DecodeString(expected.DataBase64)
	if err != nil || expected.ProgramID != bridgeAdaptorProgram ||
		len(decoded) != 8 || binary.LittleEndian.Uint64(decoded) != 0x0807060504030201 {
		t.Fatalf("expected=%+v decoded=%x err=%v", expected, decoded, err)
	}
	effects := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "bridge", Conserved: true,
		Accounts: []ExpectedAccountEffect{{
			Address: bridgeIdleATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC,
			Authority: bridgeIdleAuthority,
		}},
		ReturnData: expected,
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeExpectedEffects(encoded); err != nil {
		t.Fatalf("production bridge return contract was rejected: %v", err)
	}
}

func TestKaminoLegSelectionAdvancesOneReviewedStateTransition(t *testing.T) {
	borrowPosition := KaminoPosition{CollateralDepositedRaw: 9, RedeemablePrimeRaw: 8}
	binary.LittleEndian.PutUint64(borrowPosition.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(borrowPosition.DebtPriceSF[:8], uint64(1)<<60)
	tests := []struct {
		name         string
		decision     Decision
		position     KaminoPosition
		leg          kaminoPrimeUSDCLeg
		wire, effect uint64
	}{
		{"deposit", Decision{Action: OpenPrimeUSDCStep, AmountRaw: 10}, KaminoPosition{}, kaminoLegDeposit, 10, 10},
		{"borrow", Decision{Action: OpenPrimeUSDCStep, AmountRaw: 99}, borrowPosition, kaminoLegBorrow, 3, 3},
		{"single redeposit", Decision{Action: OpenPrimeUSDCStep, Reason: "single_loop_redeposit", AmountRaw: 4}, KaminoPosition{CollateralDepositedRaw: 9, DebtRaw: 4}, kaminoLegDeposit, 4, 4},
		{"repay", Decision{Action: DeleverPrimeUSDCStep, AmountRaw: 0}, KaminoPosition{CollateralDepositedRaw: 9, DebtRaw: 4}, kaminoLegRepay, 4, 4},
		{"withdraw", Decision{Action: DeleverPrimeUSDCStep, AmountRaw: 5}, KaminoPosition{CollateralDepositedRaw: 9, RedeemablePrimeRaw: 8}, kaminoLegWithdraw, 9, 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leg, wire, effect, err := selectKaminoLeg(test.decision, test.position)
			if err != nil || leg != test.leg || wire != test.wire || effect != test.effect {
				t.Fatalf("leg=%d wire=%d effect=%d err=%v", leg, wire, effect, err)
			}
		})
	}
	if _, _, _, err := selectKaminoLeg(Decision{Action: OpenPrimeUSDCStep, Reason: "prime_collateral_ready", AmountRaw: 1}, KaminoPosition{CollateralDepositedRaw: 1, DebtRaw: 1}); err == nil {
		t.Fatal("complete position accepted without the exact one-redeposit reason")
	}
}

func TestUnwindWithdrawsOnlyConservativeCollateralExcess(t *testing.T) {
	position := KaminoPosition{CollateralDepositedRaw: 150, RedeemablePrimeRaw: 150, DebtRaw: 50}
	binary.LittleEndian.PutUint64(position.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(position.DebtPriceSF[:8], uint64(1)<<60)
	receipt, prime, err := withdrawExcessForRepayment(position)
	if err != nil || receipt != 38 || prime != 38 {
		t.Fatalf("receipt=%d prime=%d err=%v", receipt, prime, err)
	}
	leg, wire, effect, err := selectKaminoLeg(
		Decision{Action: DeleverPrimeUSDCStep, Reason: "withdrawal_release_repayment_collateral", AmountRaw: 1},
		position,
	)
	if err != nil || leg != kaminoLegWithdraw || wire != receipt || effect != prime {
		t.Fatalf("leg=%d wire=%d effect=%d err=%v", leg, wire, effect, err)
	}
}
