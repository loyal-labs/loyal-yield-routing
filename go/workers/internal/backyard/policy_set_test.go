package backyard

import "testing"

func TestKaminoFarmUserStateDerivationMatchesRustPairs(t *testing.T) {
	t.Parallel()
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
	onreFarm := onreDebtFarmState
	onreUser, err := deriveKaminoObligationFarmUserState(onreFarm, "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FARM_PAIR maple reserve=87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y obligation=Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT user=%s", maple)
	t.Logf("FARM_PAIR onre reserve=%s obligation=4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei user=%s", onreFarm, onreUser)
	if onreUser != "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD" {
		t.Fatalf("OnRe farm user state=%s", onreUser)
	}
}
