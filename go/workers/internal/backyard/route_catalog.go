package backyard

import (
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

var autoAUTOPYUSD = RuntimeRoute{
	Lane: "AUTO/AUTO/PYUSD", Protocol: "AUTO", CollateralSymbol: "AUTO", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk",
		MarketAuthority:   "2eyLWowHqsWNuRavNc5g6e8NZiypgJTHEvW2hMum3BNS",
		Obligation:        "DJhTPmvAh5xf4X3Cwfchn43psfCgXozRoNXUMJAiDS41",
		CollateralReserve: "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX",
		CollateralMint:    "GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm",
		DebtReserve:       "6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM",
		DebtMint:          "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
	},
	CollateralCustody:         "9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G",
	DebtCustody:               "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn",
	CollateralLiquiditySupply: "7W7sahTJjE7D4UPqmp8i4fd6AUm1hFjnUqNSjY78sN5F",
	CollateralReceiptMint:     "Cjh2wuFnuSiFH6j6Q8f8YvkrHgt3LYhAZhz6JbtN7xfQ",
	CollateralReceiptSupply:   "8no8zpNDwCXLjhPr5Gb9UWHfEMUxmNFUHAjbR1WDDdvc",
	DebtLiquiditySupply:       "FZPPq1U7BqpqAnV7naHThsdumZiUWJY4Q4784HQ8a3KM",
	DebtFeeReceiver:           "HFYjFt3AYazic97CdXwZctGGeAuarCud6h6baEStg8Nh",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}

var ethenaUSDePYUSD = RuntimeRoute{
	Lane: "Ethena/USDe/PYUSD", Protocol: "Ethena", CollateralSymbol: "USDe", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
		MarketAuthority:   "GuWEkEJb5bh8Ai2gaYmZWMTUq8MrFeoaDZ89BrQfB1FZ",
		Obligation:        "5CDZVkkC9wH3FTo4xy679qorb4xMt5cHf2nhsRcTUsQr",
		CollateralReserve: "2erD9GTGcaQbLsVSQweg3HvMpfKxScmz95raWv8H4iPN",
		CollateralMint:    "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT",
		DebtReserve:       "EDf6dGbVnCCABbNhE3mp5i1jV2JhDAVmTWb1ztij1Yhs",
		DebtMint:          "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
	},
	CollateralCustody:         "6wSE9RKCReDiDiW4tSRiieFHSaVS2gTMFCVBB8mimtVk",
	DebtCustody:               "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn",
	CollateralLiquiditySupply: "BcqeM19i3njWEVPmQse2NAZGnTsTVdfuNYXWfmujoLff",
	CollateralReceiptMint:     "8DQhJtZVPLLSqFkaQqZDoRFRPD68xeHQkEQ69Gz7VMaK",
	CollateralReceiptSupply:   "2KHE9hunJFBhkjT355cBDfXZmptk1mx2W5NUgc27N68k",
	DebtLiquiditySupply:       "8Am2NKsHozvH9J1Ub5ntdLhGySUsD5P4yYBkcoPQ6NXQ",
	DebtFeeReceiver:           "GtXzva7jBAk2Khs8h6fFXLw6vjb82vQaSZRUz6FWMrUv",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}

// Finalized account review at slot 444525169 matches all four installed
// policy vectors for each Prime sibling, with no farm substitution. These are
// existing catalog identities, not new on-chain authority or funded canaries.
// Selected manifest, three-family budget scope and deployment remain unchanged.
var primePRIMEPYUSD = RuntimeRoute{
	Lane: "Prime/PRIME/PYUSD", Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		Obligation:        "GAnakFSJAhNMrH3B8PRLxHcEtWVL21xyALRiWx3baS5t",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", DebtMint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
	},
	CollateralCustody: "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct", DebtCustody: "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn",
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt",
	DebtFeeReceiver: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: token2022Program,
}

var primePRIMEUSDS = RuntimeRoute{
	Lane: "Prime/PRIME/USDS", Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "USDS",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		Obligation:        "6aqRhAxxjxdoAzgsEMrCCKCEEYMoLDLRKTu5t8nRuyYu",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", DebtMint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
	},
	CollateralCustody: "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct", DebtCustody: "5LR9AdS7XwJjQXWkKNBXNibGNkFXqe7T2JXU2oBBwknV",
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5",
	DebtFeeReceiver: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
}
