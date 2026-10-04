package multiply

// The fixed Earn MAX topology ported from
// crates/loyal-fleet-worker/src/multiply/config.rs and the loyal-actions
// constants in crates/loyal-actions/src/{ids.rs,earn_max.rs}. Every address is
// evidence-backed mainnet state; nothing is derived except the vault, custody,
// obligation, farm, and policy PDAs, whose seeds mirror
// loyal_actions::derive_* exactly.

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/gagliardetto/solana-go"
)

const (
	// EarnMaxVaultIndex is the retail vault index 0 (legacy agent vault index
	// stays distinct per the worker contract).
	EarnMaxVaultIndex uint8 = 0

	MainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

	KlendProgram     = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	JupiterProgram   = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	FarmsProgram     = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	TokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	Token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	SquadsProgram    = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"
	ATokenProgram    = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"

	USDCMint  = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDSMint  = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	PYUSDMint = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"

	onycMint  = "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"
	primeMint = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	syrupMint = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"

	onreMarket   = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8" // PRIME_MARKET alias in Rust is Figure; ONYC market is OnRe
	figureMarket = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	mapleMarket  = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"

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

	multiplyObligationTag = 0x01
	multiplyObligationID  = 0x00
)

// JupiterSharedAccountsRouteDiscriminator is EARN_MAX_SHARED_ACCOUNTS_ROUTE
// from loyal-actions: [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81].
var JupiterSharedAccountsRouteDiscriminator = [8]byte{0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81}

// PolicyConfig is one Squads smart-account policy PDA for a strategy family.
type PolicyConfig struct {
	Seed    uint64
	Account solana.PublicKey
}

// StrategyConfig is one fixed collateral/debt lane.
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

// PolicyForAction mirrors StrategyConfig::policy: None for user-side actions.
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

// EarnMaxTopology is the deterministic deployment for one settings account.
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

// DeriveEarnMaxTopology mirrors derive_earn_max_topology_with_policy_seed_base.
// seedBase > 0 selects the dynamic policy seeds base, base+1 for debt and
// base+2 for swap; zero selects the reviewed static seeds 32/33/34.
func DeriveEarnMaxTopology(settings solana.PublicKey, policySeedBase uint64) (*EarnMaxTopology, error) {
	vault, _, err := solana.FindProgramAddress([][]byte{
		[]byte("smart_account"), settings[:], []byte("smart_account"), {EarnMaxVaultIndex},
	}, mustKey(SquadsProgram))
	if err != nil {
		return nil, fmt.Errorf("derive squads vault: %w", err)
	}
	claimCustody, err := DeriveAssociatedTokenAccount(vault, mustKey(USDCMint), mustKey(TokenProgram))
	if err != nil {
		return nil, err
	}
	collateralCustody, err := DeriveAssociatedTokenAccount(vault, mustKey(syrupMint), mustKey(TokenProgram))
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

// TopologyForRoute mirrors topology_for_route, including the route identity
// check against the deterministic topology.
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

// DeriveAssociatedTokenAccount mirrors derive_associated_token_account.
func DeriveAssociatedTokenAccount(owner, mint, tokenProgram solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{owner[:], tokenProgram[:], mint[:]}, mustKey(ATokenProgram))
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive ATA: %w", err)
	}
	return key, nil
}

// deriveKaminoObligation mirrors derive_kamino_obligation.
func deriveKaminoObligation(vault, market solana.PublicKey, collateralMint, debtMint solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{
		{multiplyObligationTag}, {multiplyObligationID}, vault[:], market[:],
		collateralMint[:], debtMint[:],
	}, mustKey(KlendProgram))
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive obligation: %w", err)
	}
	return key, nil
}

// deriveKaminoFarmUserState mirrors derive_kamino_obligation_farm_user_state.
func deriveKaminoFarmUserState(farmState, obligation solana.PublicKey) (solana.PublicKey, error) {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("user"), farmState[:], obligation[:]}, mustKey(FarmsProgram))
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive farm user state: %w", err)
	}
	return key, nil
}

// deriveActionAccount mirrors derive_action_account.
func deriveActionAccount(settings solana.PublicKey, seed uint64) (solana.PublicKey, error) {
	var seedBytes [8]byte
	for index := range seedBytes {
		seedBytes[index] = byte(seed >> (8 * index))
	}
	key, _, err := solana.FindProgramAddress([][]byte{
		[]byte("smart_account"), []byte("policy"), settings[:], seedBytes[:],
	}, mustKey(SquadsProgram))
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive action account: %w", err)
	}
	return key, nil
}

// deriveActionAccountWithBump also returns the PDA bump, which the policy
// account decoder checks against the stored bump byte.
func deriveActionAccountWithBump(settings solana.PublicKey, seed uint64) (solana.PublicKey, uint8) {
	key, bump, err := deriveActionAccountWithBumpErr(settings, seed)
	if err != nil {
		panic(err)
	}
	return key, bump
}

func deriveActionAccountWithBumpErr(settings solana.PublicKey, seed uint64) (solana.PublicKey, uint8, error) {
	var seedBytes [8]byte
	for index := range seedBytes {
		seedBytes[index] = byte(seed >> (8 * index))
	}
	key, bump, err := solana.FindProgramAddress([][]byte{
		[]byte("smart_account"), []byte("policy"), settings[:], seedBytes[:],
	}, mustKey(SquadsProgram))
	if err != nil {
		return solana.PublicKey{}, 0, fmt.Errorf("derive action account: %w", err)
	}
	return key, bump, nil
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
	debtFarmState             string // empty when none
	targetLTVBPS              uint16
	staticCollateralSeed      uint64
}

var strategyTemplates = []strategyTemplate{
	{
		key: OnycUsdc, market: onreMarket, marketAuthority: onycMarketAuthority, oracle: commonOracle,
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
		key: OnycUsds, market: onreMarket, marketAuthority: onycMarketAuthority, oracle: commonOracle,
		collateralReserve: onycCollateralReserve, collateralMint: onycMint,
		collateralLiquiditySupply: onycCollateralLiquiditySupply, collateralReceiptMint: onycCollateralReceiptMint,
		collateralMintSupply: onycCollateralReceiptSupply,
		debtReserve:          "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", debtMint: USDSMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "21Skwocv5cJoftyejSTtXVaHJWTg88xcWGQtnRvUyKLx",
		debtFeeVault:  "CmMAn2UtLWHsQhwv31Trz4BZwVravs2jgxZYK2daTHaK",
		debtFarmState: "5piFMvvPonJM8zJbCGoPD2jZt59hNURDLDTpXQzgbydc", targetLTVBPS: 5000,
		staticCollateralSeed: 32,
	},
	{
		key: PrimeUsdc, market: figureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", debtMint: USDCMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
		debtFeeVault: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: PrimePyusd, market: figureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", debtMint: PYUSDMint,
		debtTokenProgram: Token2022Program, debtLiquiditySupply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt",
		debtFeeVault: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: PrimeUsds, market: figureMarket, marketAuthority: primeMarketAuthority, oracle: commonOracle,
		collateralReserve: primeCollateralReserve, collateralMint: primeMint,
		collateralLiquiditySupply: primeCollateralLiquiditySupply, collateralReceiptMint: primeCollateralReceiptMint,
		collateralMintSupply: primeCollateralReceiptSupply,
		debtReserve:          "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", debtMint: USDSMint,
		debtTokenProgram: TokenProgram, debtLiquiditySupply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5",
		debtFeeVault: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY", targetLTVBPS: 6500,
		staticCollateralSeed: 32,
	},
	{
		key: SyrupUsdcUsdc, market: mapleMarket, marketAuthority: syrupMarketAuthority, oracle: commonOracle,
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
		key: SyrupUsdcPyusd, market: mapleMarket, marketAuthority: syrupMarketAuthority, oracle: commonOracle,
		collateralReserve: syrupCollateralReserve, collateralMint: syrupMint,
		collateralLiquiditySupply: syrupCollateralLiquiditySupply, collateralReceiptMint: syrupCollateralReceiptMint,
		collateralMintSupply: syrupCollateralMintSupply,
		debtReserve:          "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", debtMint: PYUSDMint,
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
	obligation, err := deriveKaminoObligation(vault, market, collateralMint, debtMint)
	if err != nil {
		return StrategyConfig{}, err
	}
	collateralCustody, err := DeriveAssociatedTokenAccount(vault, collateralMint, mustKey(TokenProgram))
	if err != nil {
		return StrategyConfig{}, err
	}
	debtCustody, err := DeriveAssociatedTokenAccount(vault, debtMint, debtTokenProgram)
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
		user, err := deriveKaminoFarmUserState(*farm, obligation)
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
	account, err := deriveActionAccount(settings, seed)
	if err != nil {
		return PolicyConfig{}, err
	}
	return PolicyConfig{Seed: seed, Account: account}, nil
}

// routeKeyFor mirrors format!("earn-max:{settings}:{vault_index}").
func routeKeyFor(settings solana.PublicKey, vaultIndex uint8) string {
	return "earn-max:" + settings.String() + ":" + strconv.Itoa(int(vaultIndex))
}
