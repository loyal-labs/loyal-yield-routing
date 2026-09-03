package backyardrwa

import "fmt"

// RuntimeRoute is the small, typed surface shared by observation, decision,
// and execution. It describes one reviewed route; it is not a user-provided
// route request and has no ranking or fallback behavior.
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
	PolicyHashes              map[Action]string
	PolicyAccounts            map[Action]string
}

var mapleSyrupUSDCUSDC = RuntimeRoute{
	Lane: SelectedRouteID, Protocol: "Maple", CollateralSymbol: "syrupUSDC", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kaminoProgram, Market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
		Obligation:        "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT",
		CollateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
		DebtReserve:       "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
		Vault:             bridgeVault, MarketAuthority: "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g", CollateralMint: "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj", DebtMint: bridgeUSDC,
	},
	CollateralCustody:         "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN",
	DebtCustody:               bridgeSquadsATA,
	CollateralLiquiditySupply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i",
	CollateralReceiptMint:     "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu",
	CollateralReceiptSupply:   "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
	DebtLiquiditySupply:       "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
	DebtFeeReceiver:           "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
	PolicyHashes: map[Action]string{
		OpenRouteStep:              "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f",
		DeleverRouteStep:           "da76b5a0e4ca7bbc3e69a978ed34467623854e9c99d7ce6b8b844a80db675e09",
		SwapStableToCollateralStep: "04b40a67014385131f562116473e41026aa7108fea9081b14e13c1204528ced2",
		SwapCollateralToStableStep: "7401b4714f788c8f96ca4b7a207ff58e072b7e3261ec12c739ac9697c4409782",
	},
	PolicyAccounts: map[Action]string{
		OpenRouteStep:              "5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c",
		DeleverRouteStep:           "CNjRx4kgrAvk6nRGN8NbH8uvSTzoC9zBK1vuiAh6DcYZ",
		SwapStableToCollateralStep: "DYDidUg6uEX5YK7d5UBXL7v6P5BXkkMZQneATe3mpS3t",
		SwapCollateralToStableStep: "Esg2ZrtwkkdzTJyiUfPZ3H3HFVq6x8diMGXSypMPXB89",
	},
}

func mapleKaminoPolicyAccounts() []string {
	return []string{
		"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c",
		"DTVaAQuhRGLrgbopmutf8ePhQbZgpouYDp2YfW8GBUWf",
		"CNjRx4kgrAvk6nRGN8NbH8uvSTzoC9zBK1vuiAh6DcYZ",
		"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY",
	}
}

func mapleKaminoPolicyHashes() map[string]string {
	return map[string]string{
		"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c": "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f",
		"DTVaAQuhRGLrgbopmutf8ePhQbZgpouYDp2YfW8GBUWf": "ec28bbdd6239985c3675e730262ce6c8ddab2ac3c47f54a2e1bb651b851f116f",
		"CNjRx4kgrAvk6nRGN8NbH8uvSTzoC9zBK1vuiAh6DcYZ": "da76b5a0e4ca7bbc3e69a978ed34467623854e9c99d7ce6b8b844a80db675e09",
		"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY": "e994455d6351a4f615ae57dd0b0b65287e8c6af10457e70383307bb43c762a7e",
	}
}

func runtimeRoute(lane string) (RuntimeRoute, error) {
	switch lane {
	case RouteID, PhaseOneLaneID:
		config, err := pinnedKaminoObservationConfig()
		if err != nil {
			return RuntimeRoute{}, err
		}
		return RuntimeRoute{Lane: RouteID, Protocol: "Prime", CollateralSymbol: FixedCollateral, DebtSymbol: FixedDebt, Kamino: config, CollateralCustody: kaminoPrimeCustody, DebtCustody: bridgeSquadsATA}, nil
	case SelectedRouteID:
		return mapleSyrupUSDCUSDC, nil
	default:
		return RuntimeRoute{}, fmt.Errorf("runtime lane %q is not installed", lane)
	}
}

func neutralizeRouteAction(decision Decision) Decision {
	switch decision.Action {
	case SwapUSDCToPrimeStep:
		decision.Action = SwapStableToCollateralStep
	case SwapPrimeToUSDCStep:
		decision.Action = SwapCollateralToStableStep
	case OpenPrimeUSDCStep:
		decision.Action = OpenRouteStep
	case DeleverPrimeUSDCStep:
		decision.Action = DeleverRouteStep
	}
	return decision
}

func fixedRouteAction(action Action, lane string) (Action, error) {
	if lane == RouteID || lane == "" {
		return action, nil
	}
	if lane != SelectedRouteID {
		return "", fmt.Errorf("runtime lane %q is not installed", lane)
	}
	switch action {
	case SwapStableToCollateralStep:
		return SwapUSDCToPrimeStep, nil
	case SwapCollateralToStableStep:
		return SwapPrimeToUSDCStep, nil
	case OpenRouteStep:
		return OpenPrimeUSDCStep, nil
	case DeleverRouteStep:
		return DeleverPrimeUSDCStep, nil
	default:
		return action, nil
	}
}

func decisionsEqual(left, right Decision) bool {
	if left.AmountRaw != right.AmountRaw || left.Reason != right.Reason || left.IdempotencyKey != right.IdempotencyKey {
		return false
	}
	leftAction, leftErr := fixedRouteAction(left.Action, left.StrategyKey)
	rightAction, rightErr := fixedRouteAction(right.Action, right.StrategyKey)
	return leftErr == nil && rightErr == nil && leftAction == rightAction
}
