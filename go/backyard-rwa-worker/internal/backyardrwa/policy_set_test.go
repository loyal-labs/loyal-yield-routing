package backyardrwa

import "testing"

func TestBasicPolicySetDerivesRustInstallSeeds(t *testing.T) {
	set, err := basicPolicySet()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []BasicPolicyFamily{
		BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB,
	} {
		binding := set[family]
		t.Logf("BASIC_POLICY family=%s seed=%d pda=%s indexes=%v", binding.Family, binding.Seed, binding.Policy, binding.Index)
	}
	if set[BasicCollateralLifecycle].Index["deposit"] != 0 || set[BasicCollateralLifecycle].Index["withdraw"] != 1 {
		t.Fatal("collateral lifecycle indexes drifted")
	}
	if set[BasicDebtLifecycle].Index["borrow"] != 0 || set[BasicDebtLifecycle].Index["repay"] != 1 {
		t.Fatal("debt lifecycle indexes drifted")
	}
}

func TestBasicSwapPolicyResolvesApprovedCustodyPairs(t *testing.T) {
	forward, index, err := resolveBasicSwapPolicy(bridgeSquadsATA, kaminoPrimeCustody)
	if err != nil || forward.Family != BasicSwapRoutesA || index != 0 {
		t.Fatalf("prime entry=%+v index=%d err=%v", forward, index, err)
	}
	reverse, index, err := resolveBasicSwapPolicy(kaminoPrimeCustody, bridgeSquadsATA)
	if err != nil || reverse.Family != BasicSwapRoutesB || index != 0 {
		t.Fatalf("prime return=%+v index=%d err=%v", reverse, index, err)
	}
	if _, _, err := resolveBasicSwapPolicy(bridgeSettings, bridgeVault); err == nil {
		t.Fatal("uninstalled custody pair was accepted")
	}
}

func TestKaminoFarmUserStateDerivationMatchesRustPairs(t *testing.T) {
	maple, err := deriveKaminoObligationFarmUserState(
		"87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y",
		"Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT",
	)
	if err != nil {
		t.Fatal(err)
	}
	if maple != "CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr" {
		t.Fatalf("Maple farm user state=%s", maple)
	}
	onreFarm, onreUser, err := onreDebtFarmPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FARM_PAIR maple reserve=87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y obligation=Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT user=%s", maple)
	t.Logf("FARM_PAIR onre reserve=%s obligation=4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei user=%s", onreFarm, onreUser)
	if onreUser != "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD" {
		t.Fatalf("OnRe farm user state=%s", onreUser)
	}
}
