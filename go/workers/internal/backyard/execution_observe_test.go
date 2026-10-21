package backyard

import (
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

func TestStageExpectedEffectsMatchDirectTransferReceipt(t *testing.T) {
	t.Parallel()
	effects, _, strategyAfter, squadsAfter, err := bridgeExpectedEffects(rwa,
		Decision{Action: StageSquadsToVoltr, AmountRaw: 119}, 0, 0, 1_792_877,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strategyAfter != 119 || squadsAfter != 1_792_758 {
		t.Fatalf("unexpected poststate: strategy=%d squads=%d", strategyAfter, squadsAfter)
	}
	if len(effects.Accounts) != 2 || effects.Accounts[0].Address != bridgeStrategyATA || effects.Accounts[1].Address != bridgeSquadsATA {
		t.Fatalf("staging receipt contract includes unrelated accounts: %+v", effects.Accounts)
	}
}

func exactAdaptorConfigAccount(t *testing.T) ConfirmedAccount {
	t.Helper()
	data := make([]byte, adaptorConfigLength)
	copy(data[:8], adaptorConfigDiscriminator)
	data[8] = 2
	bindings := []string{
		voltr.ProgramID.String(), bridgeVoltrVault, bridgeStrategy, bridgeStrategyAuth,
		squads.ProgramID.String(), backyardSettings, bridgeSettingsSigner, bridgeVault,
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
	t.Parallel()
	account := exactAdaptorConfigAccount(t)
	if _, err := decodeObservedAdaptorConfig(rwa, account); err != nil {
		t.Fatal(err)
	}
	account.Data[176] ^= 1
	if _, err := decodeObservedAdaptorConfig(rwa, account); err == nil {
		t.Fatal("drifted Squads Settings binding was accepted")
	}
	account = exactAdaptorConfigAccount(t)
	account.Data[416] = 1
	if _, err := decodeObservedAdaptorConfig(rwa, account); err == nil {
		t.Fatal("mutable report state appeared in immutable adaptor config")
	}
}

func TestBridgeExpectedEffectsAreExactAndConserved(t *testing.T) {
	t.Parallel()
	effects, _, strategyAfter, squadsAfter, err := bridgeExpectedEffects(rwa,
		Decision{Action: VoltrAllocateToSquads, AmountRaw: 4}, 10, 7, 3,
	)
	if err != nil || strategyAfter != 7 || squadsAfter != 7 || len(effects.Accounts) != 3 {
		t.Fatalf("effects=%+v strategy=%d squads=%d err=%v", effects, strategyAfter, squadsAfter, err)
	}
	if effects.Accounts[0].AfterRaw != 6 || effects.Accounts[1].AfterRaw != 7 || effects.Accounts[2].AfterRaw != 7 {
		t.Fatal("allocation effect graph drifted")
	}
	if _, _, _, _, err := bridgeExpectedEffects(rwa, Decision{Action: VoltrRestoreIdle, AmountRaw: 8}, 10, 7, 3); err == nil {
		t.Fatal("bridge observer accepted an underfunded restore")
	}
}

func TestExpectedAdaptorReturnDataIsExactNAVLittleEndian(t *testing.T) {
	t.Parallel()
	expected := expectedAdaptorReturnData(rwa, 0x0807060504030201)
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
	if _, err := DecodeExpectedEffects(rwa, encoded); err != nil {
		t.Fatalf("production bridge return contract was rejected: %v", err)
	}
}

func TestKaminoLegSelectionAdvancesOneReviewedStateTransition(t *testing.T) {
	t.Parallel()
	borrowPosition := KaminoPosition{CollateralDepositedRaw: 9, RedeemablePrimeRaw: 8, CollateralDecimals: 6, DebtDecimals: 6}
	binary.LittleEndian.PutUint64(borrowPosition.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(borrowPosition.DebtPriceSF[:8], uint64(1)<<60)
	tests := []struct {
		name         string
		decision     Decision
		position     KaminoPosition
		leg          kaminoPrimeUSDCLeg
		wire, effect uint64
	}{
		{"deposit", Decision{Action: OpenRouteStep, AmountRaw: 10}, KaminoPosition{}, kaminoLegDeposit, 10, 10},
		{"topup deposit", Decision{Action: OpenRouteStep, Reason: topupDepositReason, AmountRaw: 4}, borrowPosition, kaminoLegDeposit, 4, 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leg, wire, effect, err := selectKaminoLeg(test.decision, test.position)
			if err != nil || leg != test.leg || wire != test.wire || effect != test.effect {
				t.Fatalf("leg=%d wire=%d effect=%d err=%v", leg, wire, effect, err)
			}
		})
	}
	// Borrow, repay, a withdrawal beside debt and an exit's withdrawal run
	// only inside atomic steps.
	for _, lone := range []struct {
		decision Decision
		position KaminoPosition
	}{
		{Decision{Action: OpenRouteStep, AmountRaw: 99}, borrowPosition},
		{Decision{Action: OpenRouteStep, Reason: "prime_collateral_ready", AmountRaw: 1}, KaminoPosition{CollateralDepositedRaw: 1, DebtRaw: 1}},
		{Decision{Action: DeleverRouteStep, AmountRaw: 4}, KaminoPosition{CollateralDepositedRaw: 9, RedeemablePrimeRaw: 8, DebtRaw: 4}},
		{Decision{Action: DeleverRouteStep, Reason: exitWithdrawReason, AmountRaw: 1}, KaminoPosition{CollateralDepositedRaw: 9, RedeemablePrimeRaw: 8}},
	} {
		if leg, _, _, err := selectKaminoLeg(lone.decision, lone.position); err == nil {
			t.Fatalf("a lone leg %d was selected for %+v", leg, lone.decision)
		}
	}
}
