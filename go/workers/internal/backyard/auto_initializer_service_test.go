package backyard

import (
	"strings"
	"testing"
)

func upsertConfirmedAccount(accounts *[]ConfirmedAccount, account ConfirmedAccount) {
	for i := range *accounts {
		if (*accounts)[i].Address == account.Address {
			(*accounts)[i] = account
			return
		}
	}
	*accounts = append(*accounts, account)
}

// The candidate lane joins the same confirmed batch on both sides of the
// observation seam: the requested address inventory and the ownership scan
// are scoped by the same reviewed manifest, the installed closure is
// unchanged, and a simultaneously funded candidate and Maple tranche holds
// instead of picking one.
func TestCandidateObservationInventoryCoversTheCandidateLane(t *testing.T) {
	t.Parallel()
	requested := map[string]bool{}
	for _, address := range routeFixedAddresses(rwa) {
		requested[address] = true
	}
	if !requested[autoAUTOPYUSD.Kamino.Obligation] || !requested[autoAUTOPYUSD.CollateralCustody] {
		t.Fatal("preferred-Maple batch inventory misses the candidate AUTO ownership accounts")
	}

	_, _, accounts := autoObservationBatch(t, 77, nil)
	// The installed lanes join the batch exactly as the production inventory
	// requests them: a present obligation envelope and a valid zero custody.
	for _, lane := range earnLaneIDs(true) {
		if lane == autoAUTOPYUSD.Lane {
			continue
		}
		laneRoute, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: laneRoute.Kamino.Obligation})
		if accountAt(accounts, laneRoute.CollateralCustody).Address == "" { // AUTO's overflow lane shares its custody
			upsertConfirmedAccount(&accounts, tokenAccountFixture(t, laneRoute.CollateralCustody, laneRoute.Kamino.CollateralMint, bridgeVault, 0))
		}
	}
	// Candidate-only ownership holds the candidate lane even with a Maple
	// preference.
	held, err := observedSelectorHeldLanes(rwa, accounts, earnLaneIDs(true), MapleSyrupUSDCLaneID)
	if err != nil || strings.Join(held, ",") != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate ownership was not held: %v %v", held, err)
	}
	// Combined candidate and Maple exposure is one held set.
	mapleRoute, err := runtimeRoute(MapleSyrupUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	upsertConfirmedAccount(&accounts, kaminoObligationImage(t, mapleRoute, 77, autoFixtureDepositReceiptRaw, autoFixtureDebtRaw))
	held, err = observedSelectorHeldLanes(rwa, accounts, earnLaneIDs(true), MapleSyrupUSDCLaneID)
	if err != nil || strings.Join(held, ",") != MapleSyrupUSDCLaneID+","+autoAUTOPYUSD.Lane {
		t.Fatalf("combined candidate and Maple exposure: held %v err %v", held, err)
	}
}
