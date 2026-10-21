package backyard

import (
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// This fixture is deliberately test-only. It describes the independently
// captured SDK/reserve graph, not a production registration or signing path.
func onreLendingParityRoute() RuntimeRoute {
	return RuntimeRoute{
		Lane: "OnRe/ONyc/USDC", Protocol: "OnRe", CollateralSymbol: "ONyc", DebtSymbol: "USDC",
		Kamino: KaminoObservationConfig{
			Program: kamino.ProgramID.String(), Vault: bridgeVault,
			Market:            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
			MarketAuthority:   "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
			Obligation:        "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei",
			CollateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
			DebtReserve:       "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
			CollateralMint:    "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5", DebtMint: fleet.USDCMint,
		},
		CollateralCustody: "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3", DebtCustody: bridgeSquadsATA,
		CollateralLiquiditySupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG",
		CollateralReceiptMint:     "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd",
		CollateralReceiptSupply:   "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
		DebtLiquiditySupply:       "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
		DebtFeeReceiver:           "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
		CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
		DebtFarm:           "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",
		ObligationDebtFarm: "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD",
	}
}
