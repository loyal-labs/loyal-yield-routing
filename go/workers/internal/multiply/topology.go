package multiply

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

var (
	TokenProgram     = solana.TokenProgramID.String()
	Token2022Program = solana.Token2022ProgramID.String()
)

const (
	EarnMaxVaultIndex uint8 = 0

	MainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

	USDCMint = fleet.USDCMint

	onycMint  = "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"
	primeMint = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	syrupMint = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"

	commonOracle = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"

	onycMarketAuthority           = "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"
	onycCollateralReserve         = "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"
	onycCollateralLiquiditySupply = "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG"
	onycCollateralReceiptMint     = "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd"
	onycCollateralReceiptSupply   = "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo"

	primeMarketAuthority           = "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM"
	primeCollateralReserve         = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
	primeCollateralLiquiditySupply = "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352"
	primeCollateralReceiptMint     = "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW"
	primeCollateralReceiptSupply   = "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN"

	syrupMarketAuthority           = "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g"
	syrupCollateralReserve         = "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY"
	syrupCollateralLiquiditySupply = "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i"
	syrupCollateralReceiptMint     = "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu"
	syrupCollateralMintSupply      = "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58"
)

type PolicyConfig struct {
	Seed    uint64
	Account solana.PublicKey
}

type StrategyConfig struct {
	Key             StrategyKey
	Market          solana.PublicKey
	MarketAuthority solana.PublicKey
	Oracle          solana.PublicKey

	CollateralReserve         solana.PublicKey
	CollateralMint            string
	CollateralCustody         solana.PublicKey
	CollateralLiquiditySupply solana.PublicKey
	CollateralReceiptMint     solana.PublicKey
	CollateralMintSupply      solana.PublicKey
	CollateralFarmState       *solana.PublicKey
	CollateralFarmUser        *solana.PublicKey

	DebtReserve         solana.PublicKey
	DebtMint            string
	DebtTokenProgram    solana.PublicKey
	DebtCustody         solana.PublicKey
	DebtLiquiditySupply solana.PublicKey
	DebtFeeVault        solana.PublicKey
	DebtFarmState       *solana.PublicKey
	DebtFarmUser        *solana.PublicKey

	Obligation   solana.PublicKey
	TargetLTVBPS uint16

	CollateralPolicy PolicyConfig
	DebtPolicy       PolicyConfig
	SwapPolicy       PolicyConfig
}

func (s StrategyConfig) PolicyForAction(action MultiplyAction) (PolicyConfig, bool) {
	switch action {
	case ActionDepositCollateral, ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		return s.CollateralPolicy, true
	case ActionBorrowDebt, ActionRepayDebt:
		return s.DebtPolicy, true
	case ActionSwapClaimToCollateral, ActionSwapDebtToCollateral,
		ActionSwapCollateralToDebt, ActionSwapCollateralToClaim:
		return s.SwapPolicy, true
	}
	return PolicyConfig{}, false
}

type EarnMaxTopology struct {
	ManifestVersion   string
	Settings          solana.PublicKey
	VaultIndex        uint8
	Vault             solana.PublicKey
	ClaimCustody      solana.PublicKey
	CollateralCustody solana.PublicKey
	Strategies        map[StrategyKey]StrategyConfig
}

func mustKey(value string) solana.PublicKey {
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil {
		panic("multiply: constant key is invalid: " + value)
	}
	return key
}

func optionalFarm(value string) *solana.PublicKey {
	key := mustKey(value)
	return &key
}

func DeriveEarnMaxTopology(settings solana.PublicKey, policySeedBase uint64) (*EarnMaxTopology, error) {
	vault, _, err := squads.SmartAccountAddress(settings, EarnMaxVaultIndex)
	if err != nil {
		return nil, fmt.Errorf("derive squads vault: %w", err)
	}
	claimCustody, err := spl.AssociatedTokenAddress(vault, mustKey(USDCMint), solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	collateralCustody, err := spl.AssociatedTokenAddress(vault, mustKey(syrupMint), solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	strategies := make(map[StrategyKey]StrategyConfig, len(strategyKeys))
	for _, template := range strategyTemplates {
		config, err := deriveStrategy(settings, vault, template, policySeedBase)
		if err != nil {
			return nil, err
		}
		strategies[config.Key] = config
	}
	return &EarnMaxTopology{
		ManifestVersion:   ManifestVersion,
		Settings:          settings,
		VaultIndex:        EarnMaxVaultIndex,
		Vault:             vault,
		ClaimCustody:      claimCustody,
		CollateralCustody: collateralCustody,
		Strategies:        strategies,
	}, nil
}

func TopologyForRoute(state *RouteState) (*EarnMaxTopology, error) {
	settings, err := solana.PublicKeyFromBase58(state.Settings)
	if err != nil {
		return nil, fmt.Errorf("route settings is not a pubkey: %w", err)
	}
	topology, err := DeriveEarnMaxTopology(settings, state.PolicySeedBase)
	if err != nil {
		return nil, err
	}
	if state.VaultIndex != topology.VaultIndex || state.Vault != topology.Vault.String() {
		return nil, errors.New("route identity does not match the deterministic Earn MAX topology")
	}
	return topology, nil
}

func (t *EarnMaxTopology) Strategy(key StrategyKey) (StrategyConfig, error) {
	config, ok := t.Strategies[key]
	if !ok {
		return StrategyConfig{}, fmt.Errorf("unknown strategy %q", key)
	}
	return config, nil
}

func (t *EarnMaxTopology) StrategyCatalog() []StrategyConfig {
	catalog := make([]StrategyConfig, 0, len(strategyKeys))
	for _, key := range strategyKeys {
		catalog = append(catalog, t.Strategies[key])
	}
	return catalog
}

type strategyTemplate struct {
	key                       StrategyKey
	market                    string
	marketAuthority           string
	oracle                    string
	collateralReserve         string
	collateralMint            string
	collateralLiquiditySupply string
	collateralReceiptMint     string
	collateralMintSupply      string
	debtReserve               string
	debtMint                  string
	debtTokenProgram          string
	debtLiquiditySupply       string
	debtFeeVault              string
	debtFarmState             string
	targetLTVBPS              uint16
	staticCollateralSeed      uint64
}

var strategyTemplates = []strategyTemplate{
	{
		key: OnycUsdc, market: fleet.KaminoOnReMarket, marketAuthority: onycMarketAuthority, oracle: commonOracle,
		collateralReserve: onycCollateralReserve, collateralMint: onycMint,
		collateralLiquiditySupply: onycCollateralLiquiditySupply, collateralReceiptMint: onycCollateralReceiptMint,
		collateralMintSupply: onycCollateralReceiptSupply,
		debtReserve:          "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z", debtMint: USDCMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
		debtFeeVault:  "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
		debtFarmState: "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF", targetLTVBPS: 5000,
		staticCollateralSeed: 32,
	},
	{
		key: OnycUsds, market: fleet.KaminoOnReMarket, marketAuthority: onycMarketAuthority, oracle: commonOracle,
		collateralReserve: onycCollateralReserve, collateralMint: onycMint,
		collateralLiquiditySupply: onycCollateralLiquiditySupply, collateralReceiptMint: onycCollateralReceiptMint,
		collateralMintSupply: onycCollateralReceiptSupply,
		debtReserve:          "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", debtMint: fleet.USDSMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "21Skwocv5cJoftyejSTtXVaHJWTg88xcWGQtnRvUyKLx",
		debtFeeVault:  "CmMAn2UtLWHsQhwv31Trz4BZwVravs2jgxZYK2daTHaK",
		debtFarmState: "5piFMvvPonJM8zJbCGoPD2jZt59hNURDLDTpXQzgbydc", targetLTVBPS: 5000,
		staticCollateralSeed: 32,
	},
	{
		key: PrimeUsdc, market: fleet.KaminoFigureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", debtMint: USDCMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
		debtFeeVault: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: PrimePyusd, market: fleet.KaminoFigureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", debtMint: fleet.PYUSDMint,
		debtTokenProgram: Token2022Program, debtLiquiditySupply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt",
		debtFeeVault: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: PrimeUsds, market: fleet.KaminoFigureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", debtMint: fleet.USDSMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5",
		debtFeeVault: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: SyrupUsdcUsdc, market: fleet.KaminoMapleMarket, marketAuthority: syrupMarketAuthority, oracle: commonOracle,
		collateralReserve: syrupCollateralReserve, collateralMint: syrupMint,
		collateralLiquiditySupply: syrupCollateralLiquiditySupply, collateralReceiptMint: syrupCollateralReceiptMint,
		collateralMintSupply: syrupCollateralMintSupply,
		debtReserve:          "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", debtMint: USDCMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
		debtFeeVault:  "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
		debtFarmState: "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: SyrupUsdcPyusd, market: fleet.KaminoMapleMarket, marketAuthority: syrupMarketAuthority, oracle: commonOracle,
		collateralReserve: syrupCollateralReserve, collateralMint: syrupMint,
		collateralLiquiditySupply: syrupCollateralLiquiditySupply, collateralReceiptMint: syrupCollateralReceiptMint,
		collateralMintSupply: syrupCollateralMintSupply,
		debtReserve:          "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", debtMint: fleet.PYUSDMint,
		debtTokenProgram: Token2022Program, debtLiquiditySupply: "GUENeLN1ufX4K5622DbyYoQFhaWxMKoCFycvLSEYsykN",
		debtFeeVault:  "AwnzukUiajn7b3T9hXcwy19RLPZcHmLANUeqZnzXT6dU",
		debtFarmState: "9AUA7XZ1rynUsZcmVCgj8UFdQuDozFSMpaNGBZAtiPWj", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
}

func deriveStrategy(settings, vault solana.PublicKey, template strategyTemplate, policySeedBase uint64) (StrategyConfig, error) {
	market := mustKey(template.market)
	collateralMint := mustKey(template.collateralMint)
	debtMint := mustKey(template.debtMint)
	debtTokenProgram := mustKey(template.debtTokenProgram)
	obligation, err := kamino.ObligationAddress(1, 0, vault, market, collateralMint, debtMint)
	if err != nil {
		return StrategyConfig{}, err
	}
	collateralCustody, err := spl.AssociatedTokenAddress(vault, collateralMint, solana.TokenProgramID)
	if err != nil {
		return StrategyConfig{}, err
	}
	debtCustody, err := spl.AssociatedTokenAddress(vault, debtMint, debtTokenProgram)
	if err != nil {
		return StrategyConfig{}, err
	}
	config := StrategyConfig{
		Key: template.key, Market: market, MarketAuthority: mustKey(template.marketAuthority),
		Oracle:            mustKey(template.oracle),
		CollateralReserve: mustKey(template.collateralReserve), CollateralMint: template.collateralMint,
		CollateralCustody:         collateralCustody,
		CollateralLiquiditySupply: mustKey(template.collateralLiquiditySupply),
		CollateralReceiptMint:     mustKey(template.collateralReceiptMint),
		CollateralMintSupply:      mustKey(template.collateralMintSupply),
		DebtReserve:               mustKey(template.debtReserve), DebtMint: template.debtMint,
		DebtTokenProgram: debtTokenProgram, DebtCustody: debtCustody,
		DebtLiquiditySupply: mustKey(template.debtLiquiditySupply),
		DebtFeeVault:        mustKey(template.debtFeeVault),
		Obligation:          obligation, TargetLTVBPS: template.targetLTVBPS,
	}
	if template.debtFarmState != "" {
		farm := optionalFarm(template.debtFarmState)
		config.DebtFarmState = farm
		user, err := kamino.ObligationFarmUserState(*farm, obligation)
		if err != nil {
			return StrategyConfig{}, err
		}
		config.DebtFarmUser = &user
	}
	seeds, err := policySeeds(policySeedBase, template.staticCollateralSeed)
	if err != nil {
		return StrategyConfig{}, err
	}
	if config.CollateralPolicy, err = policyConfig(settings, seeds.collateral); err != nil {
		return StrategyConfig{}, err
	}
	if config.DebtPolicy, err = policyConfig(settings, seeds.debt); err != nil {
		return StrategyConfig{}, err
	}
	config.SwapPolicy, err = policyConfig(settings, seeds.swap)
	return config, err
}

type seeds struct{ collateral, debt, swap uint64 }

func policySeeds(base, staticCollateral uint64) (seeds, error) {
	if base == 0 {
		return seeds{collateral: staticCollateral, debt: staticCollateral + 1, swap: staticCollateral + 2}, nil
	}
	if base > (1<<64-1)-2 {
		return seeds{}, errors.New("Earn MAX policy seed overflow")
	}
	return seeds{collateral: base, debt: base + 1, swap: base + 2}, nil
}

func policyConfig(settings solana.PublicKey, seed uint64) (PolicyConfig, error) {
	account, _, err := squads.PolicyAddress(settings, seed)
	if err != nil {
		return PolicyConfig{}, fmt.Errorf("derive action account: %w", err)
	}
	return PolicyConfig{Seed: seed, Account: account}, nil
}

func routeKeyFor(settings solana.PublicKey, vaultIndex uint8) string {
	return "earn-max:" + settings.String() + ":" + strconv.Itoa(int(vaultIndex))
}
