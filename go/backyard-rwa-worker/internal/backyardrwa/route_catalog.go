package backyardrwa

// These two exact bindings were resolved against installed policy bytes and
// chain account identities in setup-feasibility-2026-09-04.json. Neither lane
// requires a farm-account substitution. Registration is construction capability,
// not live eligibility: the worker's selected manifest is unchanged, and full
// observation, admission, exit and lifecycle proof remain separate gates.
type kaminoPolicyBinding struct {
	Policy     string
	DataSHA256 string
}

var autoAUTOPYUSD = RuntimeRoute{
	Lane: "AUTO/AUTO/PYUSD", Protocol: "AUTO", CollateralSymbol: "AUTO", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kaminoProgram, Vault: bridgeVault,
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
	KaminoPolicies: map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{
		kaminoLegDeposit:  {"651fFC9yEuWmSjKKswxeW8xe9HWpVyoDLqk6J3uRTiVh", "891ddc8e03d8152309949070a61e9da58f055bfbfb168691bfe1853c7b7031d0"},
		kaminoLegBorrow:   {"GxCcNhSyMVRoYeqmiD4ywvRqFhoXpETTexg3DWK7rREY", "49bf05b2f45d78c59e4cbaa8585ceb7b5bd7081d8c6325b5871255c6095a4c57"},
		kaminoLegRepay:    {"RhqE9EKoQFAAf7agE4mY47Yjto7e4CwUqNx9Pkc45YT", "b8eddeb02c7bb820640618d3ea2df3daa679abafbe058cd896e28c8ffce746c7"},
		kaminoLegWithdraw: {"EAZLxy1c4xCFwgTYKsT2YQAy84omiKm3KLCe4wD9aisA", "15a7cd1b8aa61b0f44cc5dfbeab19bed563cb8852ceed194feba7575c933e066"},
	},
}

var ethenaUSDePYUSD = RuntimeRoute{
	Lane: "Ethena/USDe/PYUSD", Protocol: "Ethena", CollateralSymbol: "USDe", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kaminoProgram, Vault: bridgeVault,
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
	KaminoPolicies: map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{
		kaminoLegDeposit:  {"7Q3TRF5BwisPytbixMtJC2Nuo1Uzkb7XdvguwdNNe2ck", "89cf13995ac1160c09e584485ff7c831f686ca204880ad7ff0603859a0b3a94a"},
		kaminoLegBorrow:   {"8u93AVSty7WCC5k7Td5m5kaxdwLPEHGSYUqutFSporqU", "57c8edee6318ecc7bf76ea2b04208c42bf7b3b8a6b0340e854b4190802eb94fd"},
		kaminoLegRepay:    {"4xmuxRvrZ3VfFE3HdTNoYKi5ys1Q1wGuTK2b69wkmcdG", "c2f29cb655a61152f317c19208d5e297fd76d140a9dc35855da25b8289ae1fcd"},
		kaminoLegWithdraw: {"BntbpNWFsmzTftxjWNcfZgWwyj1KHhW7C1v6mNUXMoUa", "1ec005205eeb5f8b40effa8dc2af6a1a6f2a7c8b5576dd42ca28ecbd545f05a4"},
	},
}

// Finalized account review at slot 444525169 matches all four installed
// policy vectors for each Prime sibling, with no farm substitution. These are
// existing catalog identities, not new on-chain authority or funded canaries.
// Selected manifest, three-family budget scope and deployment remain unchanged.
var primePRIMEPYUSD = RuntimeRoute{
	Lane: "Prime/PRIME/PYUSD", Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kaminoProgram, Vault: bridgeVault,
		Market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		Obligation:        "GAnakFSJAhNMrH3B8PRLxHcEtWVL21xyALRiWx3baS5t",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", DebtMint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
	},
	CollateralCustody: "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct", DebtCustody: "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn",
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt",
	DebtFeeReceiver: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: token2022Program,
	KaminoPolicies: map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{
		kaminoLegDeposit:  {"7QDj4Cz1sWv8XvELx3vBokbMaG2mjfy7rri31sJrJzgq", "d3d819f122f5827db26f1804ee3afa56f4ce53081dbdf9918ca574a230f99ddb"},
		kaminoLegBorrow:   {"5TDCpmUFr2pHrfDQwG5CnGvbEgHQRUjYysCKZTLh55DL", "ee7c4bec16995b4a39329cfc50c2be9a86dfac31142bd16acca33062c25365f8"},
		kaminoLegRepay:    {"Hy2JX1PKp3DC5Z93b9JDq9t4vjKxoYecPkv3NP1YVhVD", "03286241d8e991efef99e8210c4d056e7a2c0a826ba9638659ad5b599c047c10"},
		kaminoLegWithdraw: {"xByUVqMPnyFfBDtmNHYeYjNoWabvGPJaudTaCSTNWNg", "00982e4a51ee914e3bc2831a8675df53b0bbde38cedf74a33a86987bb71b76f1"},
	},
}

var primePRIMEUSDS = RuntimeRoute{
	Lane: "Prime/PRIME/USDS", Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "USDS",
	Kamino: KaminoObservationConfig{
		Program: kaminoProgram, Vault: bridgeVault,
		Market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		Obligation:        "6aqRhAxxjxdoAzgsEMrCCKCEEYMoLDLRKTu5t8nRuyYu",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", DebtMint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
	},
	CollateralCustody: "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct", DebtCustody: "5LR9AdS7XwJjQXWkKNBXNibGNkFXqe7T2JXU2oBBwknV",
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5",
	DebtFeeReceiver: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	KaminoPolicies: map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{
		kaminoLegDeposit:  {"FgT2cdShhQNuk1QiYibeZXCYhm3ZoYVYGcwm2UeQ2PEh", "855886ceb35059b41297727c4b20c39e0fcec8fb03847ea4f4053b86733835e7"},
		kaminoLegBorrow:   {"GsUJYgJYH5SSAoQC9kUfQV79JnLVjoJgpT89X5fo1KKd", "f455eba9bcd5e78de8264fb11f6d660dc25a47e848e4c221dcd9f4880f843064"},
		kaminoLegRepay:    {"GG3Sfg69KesDUrRu3P44K66VrUCJr8Dnu2qrBkFqvtJ7", "d3c654b56fb318e8b017bec5035774119c23cd3b8bde40dc10126080491bba27"},
		kaminoLegWithdraw: {"6axY2HyxjWKXfsaGQq85NLmVv8wggEbTS9wTgFjtMGqX", "fa6f83ae80a2ce6fc6b27bac9e3fc6711beb4ea4b544aa6f048bb7a6464e8549"},
	},
}
