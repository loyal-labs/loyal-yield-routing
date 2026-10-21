package backyard

import "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"

type RuntimeRoute struct {
	Lane                      string
	Protocol                  string
	CollateralSymbol          string
	DebtSymbol                string
	Kamino                    KaminoObservationConfig
	CollateralCustody         string
	DebtCustody               string
	CollateralLiquiditySupply string
	CollateralReceiptMint     string
	CollateralReceiptSupply   string
	DebtLiquiditySupply       string
	DebtFeeReceiver           string
	CollateralTokenProgram    string
	DebtTokenProgram          string
	CollateralFarm            string
	ObligationCollateralFarm  string
	DebtFarm                  string
	ObligationDebtFarm        string
}

func sharedUSDCDebt(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.route.Kamino.DebtMint == fleet.USDCMint && l.route.DebtTokenProgram == classicTokenProgram
}
