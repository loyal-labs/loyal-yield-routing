package backyardrwa

import (
	"reflect"
	"testing"
)

// TestBasicRuntimeRoutePinnedAddressesDecode proves every pinned address of
// the installed basic lanes is a canonical 32-byte base58 public key. This
// rejects dropped, truncated, or non-canonical constants that no longer
// decode to exactly 32 bytes; a wrong-but-well-formed address still decodes,
// so the parity test below is what binds the values to the captured graph.
func TestBasicRuntimeRoutePinnedAddressesDecode(t *testing.T) {
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		addresses := []struct{ field, address string }{
			{"kamino program", route.Kamino.Program},
			{"kamino vault", route.Kamino.Vault},
			{"kamino market", route.Kamino.Market},
			{"kamino market authority", route.Kamino.MarketAuthority},
			{"kamino obligation", route.Kamino.Obligation},
			{"collateral reserve", route.Kamino.CollateralReserve},
			{"debt reserve", route.Kamino.DebtReserve},
			{"collateral mint", route.Kamino.CollateralMint},
			{"debt mint", route.Kamino.DebtMint},
			{"collateral custody", route.CollateralCustody},
			{"debt custody", route.DebtCustody},
			{"collateral liquidity supply", route.CollateralLiquiditySupply},
			{"collateral receipt mint", route.CollateralReceiptMint},
			{"collateral receipt supply", route.CollateralReceiptSupply},
			{"debt liquidity supply", route.DebtLiquiditySupply},
			{"debt fee receiver", route.DebtFeeReceiver},
			{"collateral token program", route.CollateralTokenProgram},
			{"debt token program", route.DebtTokenProgram},
			{"debt farm", route.DebtFarm},
			{"obligation debt farm", route.ObligationDebtFarm},
		}
		for _, pinned := range addresses {
			if pinned.address == "" {
				continue
			}
			if _, err := decodeKey(pinned.address); err != nil {
				t.Errorf("lane %s %s %q is not a 32-byte base58 public key: %v", lane, pinned.field, pinned.address, err)
			}
		}
	}
}

// TestOnReRuntimeLaneMatchesCapturedParityTable compares the installed OnRe
// runtime lane with the independently captured SDK/reserve table. Every
// constant here decodes to 32 bytes by the test above, so agreement with that
// capture is the guard against a well-formed address typo.
// basicLanePinnedFields projects the pinned OnRe constants that must agree
// between the installed runtime lane and the captured SDK/reserve table:
// market graph, custodies, supply vaults, receipt mints, fee receiver, token
// programs, and farms.
func basicLanePinnedFields(route RuntimeRoute) any {
	return struct {
		Protocol, CollateralSymbol, DebtSymbol         string
		Kamino                                         KaminoObservationConfig
		CollateralCustody, DebtCustody                 string
		CollateralLiquiditySupply                      string
		CollateralReceiptMint, CollateralReceiptSupply string
		DebtLiquiditySupply, DebtFeeReceiver           string
		CollateralTokenProgram, DebtTokenProgram       string
		DebtFarm, ObligationDebtFarm                   string
	}{
		route.Protocol, route.CollateralSymbol, route.DebtSymbol,
		route.Kamino,
		route.CollateralCustody, route.DebtCustody,
		route.CollateralLiquiditySupply,
		route.CollateralReceiptMint, route.CollateralReceiptSupply,
		route.DebtLiquiditySupply, route.DebtFeeReceiver,
		route.CollateralTokenProgram, route.DebtTokenProgram,
		route.DebtFarm, route.ObligationDebtFarm,
	}
}

func TestOnReRuntimeLaneMatchesCapturedParityTable(t *testing.T) {
	installed, err := runtimeRoute("OnRe/ONyc/USDC")
	if err != nil {
		t.Fatal(err)
	}
	captured := onreLendingParityRoute()
	if !reflect.DeepEqual(basicLanePinnedFields(installed), basicLanePinnedFields(captured)) {
		t.Fatalf("installed OnRe runtime lane drifted from the captured SDK/reserve table:\ninstalled %+v\ncaptured  %+v", basicLanePinnedFields(installed), basicLanePinnedFields(captured))
	}
}
