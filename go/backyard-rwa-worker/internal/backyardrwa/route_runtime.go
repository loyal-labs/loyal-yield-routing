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
	CollateralTokenProgram    string
	DebtTokenProgram          string
	CollateralFarm            string
	ObligationCollateralFarm  string
	DebtFarm                  string
	ObligationDebtFarm        string
	BasicPolicy               bool
	KaminoPolicies            map[kaminoPrimeUSDCLeg]kaminoPolicyBinding
	PolicyHashes              map[Action]string
	PolicyAccounts            map[Action]string
}

func basicRuntimeRoute(lane string) (RuntimeRoute, error) {
	base := RuntimeRoute{
		Lane: lane, BasicPolicy: true, DebtCustody: bridgeSquadsATA,
		CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
		Kamino: KaminoObservationConfig{Program: kaminoProgram, Vault: bridgeVault, DebtMint: bridgeUSDC},
	}
	switch lane {
	case PhaseOneLaneID:
		base.Protocol, base.CollateralSymbol, base.DebtSymbol = "Prime", FixedCollateral, FixedDebt
		base.Kamino.Market = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
		base.Kamino.MarketAuthority = "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM"
		base.Kamino.Obligation = "9suFBUhW7D7jN141mKR49Hn1WYDHEsRnPiGhxxm7RFkv"
		base.Kamino.CollateralReserve = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
		base.Kamino.DebtReserve = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"
		base.Kamino.CollateralMint = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
		base.CollateralCustody = "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct"
		base.CollateralLiquiditySupply = "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352"
		base.CollateralReceiptMint = "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW"
		base.CollateralReceiptSupply = "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN"
		base.DebtLiquiditySupply, base.DebtFeeReceiver = "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC", "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6"
	case SelectedRouteID:
		base.Protocol, base.CollateralSymbol, base.DebtSymbol = "Maple", "syrupUSDC", "USDC"
		base.Kamino.Market = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"
		base.Kamino.MarketAuthority = "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g"
		base.Kamino.Obligation = "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT"
		base.Kamino.CollateralReserve = "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY"
		base.Kamino.DebtReserve = "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"
		base.Kamino.CollateralMint = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"
		base.CollateralCustody = "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN"
		base.CollateralLiquiditySupply = "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i"
		base.CollateralReceiptMint = "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu"
		base.CollateralReceiptSupply = "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58"
		base.DebtLiquiditySupply, base.DebtFeeReceiver = "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM", "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV"
		base.DebtFarm, base.ObligationDebtFarm = mapleDebtFarm, mapleObligationDebtFarm
	case "OnRe/ONyc/USDC":
		base.Protocol, base.CollateralSymbol, base.DebtSymbol = "OnRe", "ONyc", "USDC"
		base.Kamino.Market = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"
		base.Kamino.MarketAuthority = "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"
		base.Kamino.Obligation = "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"
		base.Kamino.CollateralReserve = "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"
		base.Kamino.DebtReserve = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"
		base.Kamino.CollateralMint = "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"
		base.CollateralCustody = "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"
		base.CollateralLiquiditySupply = "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG"
		base.CollateralReceiptMint = "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd"
		base.CollateralReceiptSupply = "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo"
		base.DebtLiquiditySupply, base.DebtFeeReceiver = "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB", "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj"
		base.DebtFarm, base.ObligationDebtFarm = onreDebtFarmState, "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD"
	default:
		return RuntimeRoute{}, fmt.Errorf("runtime lane %q is not installed", lane)
	}
	return base, nil
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
	CollateralTokenProgram:    classicTokenProgram,
	DebtTokenProgram:          classicTokenProgram,
	DebtFarm:                  mapleDebtFarm,
	ObligationDebtFarm:        mapleObligationDebtFarm,
	KaminoPolicies: map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{
		kaminoLegDeposit:  {"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c", "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f"},
		kaminoLegBorrow:   {"2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV", "6f97d7928d7927d65b588644d2e0506bc86b2173f2f525edf087474e28631a94"},
		kaminoLegRepay:    {"AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo", "4bb7136fdeaa094aaf7e39cd0595434e1e9e09586c496303236f5d4ecc169f11"},
		kaminoLegWithdraw: {"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY", "e994455d6351a4f615ae57dd0b0b65287e8c6af10457e70383307bb43c762a7e"},
	},
	PolicyHashes: map[Action]string{
		OpenRouteStep:              "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f",
		DeleverRouteStep:           "4bb7136fdeaa094aaf7e39cd0595434e1e9e09586c496303236f5d4ecc169f11",
		SwapStableToCollateralStep: "04b40a67014385131f562116473e41026aa7108fea9081b14e13c1204528ced2",
		SwapCollateralToStableStep: "1f2392e28fb96d92fb1b98a46609b8b7f4f0d38e6ddf943cd2d473a6e799589d",
	},
	PolicyAccounts: map[Action]string{
		OpenRouteStep:              "5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c",
		DeleverRouteStep:           "AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo",
		SwapStableToCollateralStep: "DYDidUg6uEX5YK7d5UBXL7v6P5BXkkMZQneATe3mpS3t",
		SwapCollateralToStableStep: "FhvEZNhKwF3dPZL36rrcbo5TCvTZTRBadE4YFNWxxwVR",
	},
}

func mapleKaminoPolicyAccounts() []string {
	return []string{
		"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c",
		"2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV",
		"AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo",
		"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY",
	}
}

func mapleKaminoPolicyHashes() map[string]string {
	return map[string]string{
		"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c": "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f",
		"2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV": "6f97d7928d7927d65b588644d2e0506bc86b2173f2f525edf087474e28631a94",
		"AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo": "4bb7136fdeaa094aaf7e39cd0595434e1e9e09586c496303236f5d4ecc169f11",
		"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY": "e994455d6351a4f615ae57dd0b0b65287e8c6af10457e70383307bb43c762a7e",
	}
}

func runtimeRoute(lane string) (RuntimeRoute, error) {
	switch lane {
	case RouteID:
		config, err := pinnedKaminoObservationConfig()
		if err != nil {
			return RuntimeRoute{}, err
		}
		return RuntimeRoute{Lane: RouteID, Protocol: "Prime", CollateralSymbol: FixedCollateral, DebtSymbol: FixedDebt, Kamino: config,
			CollateralCustody: kaminoPrimeCustody, DebtCustody: bridgeSquadsATA,
			CollateralLiquiditySupply: kaminoPrimeLiquiditySupply, CollateralReceiptMint: kaminoPrimeReceiptMint,
			CollateralReceiptSupply: kaminoPrimeReceiptSupply, DebtLiquiditySupply: kaminoUSDCLiquiditySupply,
			DebtFeeReceiver: kaminoUSDCFeeVault, CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram}, nil
	case PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC":
		return basicRuntimeRoute(lane)
	case "AUTO/AUTO/PYUSD":
		return autoAUTOPYUSD, nil
	case "Ethena/USDe/PYUSD":
		return ethenaUSDePYUSD, nil
	case "Prime/PRIME/PYUSD":
		return primePRIMEPYUSD, nil
	case "Prime/PRIME/USDS":
		return primePRIMEUSDS, nil
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

// legacyUSDCAction preserves historical PRIME action names wire contract.
// Typed planner actions remain canonical; shared-custody admission must be
// implemented before replacing this compatibility boundary.
func legacyUSDCAction(action Action) Action {
	switch action {
	case SwapStableToCollateralStep, SwapDebtToCollateralStep:
		return SwapUSDCToPrimeStep
	case SwapCollateralToStableStep, SwapCollateralToDebtStep:
		return SwapPrimeToUSDCStep
	case OpenRouteStep:
		return OpenPrimeUSDCStep
	case DeleverRouteStep:
		return DeleverPrimeUSDCStep
	default:
		return action
	}
}

func fixedRouteAction(action Action, lane string) (Action, error) {
	if lane == "" || lane == RouteID {
		return legacyUSDCAction(action), nil
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return "", err
	}
	if route.Lane == RouteID {
		return legacyUSDCAction(action), nil
	}
	return neutralizeRouteAction(Decision{Action: action}).Action, nil
}

func decisionsEqual(left, right Decision) bool {
	// Preparation re-observes confirmed state and journals that refreshed
	// observation's idempotency identity. Accruing reserves may change the
	// identity while leaving the exact executable decision unchanged, so this
	// guard compares execution semantics rather than the superseded identity.
	if left.AmountRaw != right.AmountRaw || left.Reason != right.Reason || left.StrategyKey != right.StrategyKey {
		return false
	}
	leftAction, leftErr := fixedRouteAction(left.Action, left.StrategyKey)
	rightAction, rightErr := fixedRouteAction(right.Action, right.StrategyKey)
	return leftErr == nil && rightErr == nil && leftAction == rightAction
}

// The basic USDC lanes and catalog debt lanes share the same bounded position
// return recipe. This does not change Jupiter wire dialects or installed policy
// authority; each builder still resolves its own exact route binding.
func positionReturnRoute(lane string) bool {
	return catalogJupiterRoute(lane) || selectorLane(lane)
}

func sharedUSDCDebt(lane string) bool {
	route, err := runtimeRoute(lane)
	return err == nil && route.Kamino.DebtMint == bridgeUSDC && route.DebtCustody == bridgeSquadsATA
}

// USDC debt cash and bridge cash are one account. Never copy that balance into
// DebtIdleRaw: NAV would count it twice. Other debt mints have distinct custody.
func debtCashRaw(s Snapshot) int64 {
	if sharedUSDCDebt(s.RouteLane) {
		if s.DebtIdleRaw != 0 {
			return -1
		}
		return s.SquadsIdleRaw
	}
	return s.DebtIdleRaw
}

func setDebtCashRaw(s *Snapshot, raw int64) {
	if sharedUSDCDebt(s.RouteLane) {
		s.SquadsIdleRaw, s.DebtIdleRaw = raw, 0
	} else {
		s.DebtIdleRaw = raw
	}
}
