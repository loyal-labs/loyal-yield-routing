package fleet

import (
	"math"
	"time"
)

const (
	USDCMint                                 = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	CashMint                                 = "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"
	USDGMint                                 = "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"
	PYUSDMint                                = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	USDTMint                                 = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	USDSMint                                 = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	KaminoMainMarket                         = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	KaminoFigureMarket                       = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	KaminoMapleMarket                        = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"
	KaminoOnReMarket                         = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"
	KaminoAutoMarket                         = "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk"
	KaminoEthenaMarket                       = "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"
	amountSemanticsKaminoCollateralDeposited = "kamino_obligation_collateral_deposited_amount"
	amountSemanticsRedeemableLiquidity       = "redeemable_liquidity_amount"
)

type ReserveIdentity struct {
	Address string `json:"address"`
	Market  string `json:"market"`
	Mint    string `json:"mint"`
}

type ReserveState struct {
	ReserveIdentity
	Slot                   int64     `json:"slot"`
	ObservedAt             time.Time `json:"observedAt"`
	LastUpdateSlot         int64     `json:"lastUpdateSlot"`
	LastUpdateStale        bool      `json:"lastUpdateStale"`
	EconomicSlotLag        int64     `json:"economicSlotLag"`
	SupplyAPYBPS           int64     `json:"supplyApyBps"`
	TotalSupplyRaw         int64     `json:"totalSupplyUsdMicros"`
	EconomicLifetimeMillis int64     `json:"economicLifetimeMillis"`
	DataHash               string    `json:"dataHash"`
	// KLend capacity: nil is unknown evidence, never zero and never unlimited; a deposit limit of 0 blocks deposits.
	AvailableLiquidityRaw *int64  `json:"availableLiquidityRaw"`
	DepositLimitRaw       *uint64 `json:"depositLimitRaw"`
}

// DepositRoomRaw is the supply the deposit limit still admits; known is false when the limit was not observed.
func (r ReserveState) DepositRoomRaw() (room int64, known bool) {
	if r.DepositLimitRaw == nil || r.TotalSupplyRaw < 0 {
		return 0, false
	}
	limit, supply := *r.DepositLimitRaw, uint64(r.TotalSupplyRaw)
	if limit <= supply {
		return 0, true
	}
	if limit-supply > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(limit - supply), true
}

// SupplyUSDMicros holds because Earn reserves are six-decimal stablecoins valued at a code-owned $1.
func (r ReserveState) SupplyUSDMicros() int64 { return r.TotalSupplyRaw }

func (r ReserveState) TargetEligible() bool {
	return !r.LastUpdateStale && r.EconomicLifetimeMillis >= minimumPublicationLifetime.Milliseconds() &&
		r.SupplyUSDMicros() >= minimumSupplyUSDMicros && r.SupplyAPYBPS >= 0 && r.SupplyAPYBPS < 5_000
}

type MarketSnapshot struct {
	Slot       int64                   `json:"slot"`
	ObservedAt time.Time               `json:"observedAt"`
	Hash       string                  `json:"hash"`
	Reserves   map[string]ReserveState `json:"reserves"`
}

var SubmissionTerminalStates = []string{"reconciled", "expired", "failed"}

type EarnStablecoin struct{ Mint, TokenProgram string }

var earnStablecoins = []EarnStablecoin{
	{CashMint, token2022Program},
	{USDGMint, token2022Program},
	{PYUSDMint, token2022Program},
	{USDCMint, tokenProgram},
	{USDTMint, tokenProgram},
	{USDSMint, tokenProgram},
}

var earnStableMints = func() []string {
	mints := make([]string, len(earnStablecoins))
	for i, stable := range earnStablecoins {
		mints[i] = stable.Mint
	}
	return mints
}()

func EarnStablecoins() []EarnStablecoin { return append([]EarnStablecoin(nil), earnStablecoins...) }

func EarnStableMints() []string { return append([]string(nil), earnStableMints...) }

func EarnStableTokenProgram(mint string) (string, bool) {
	for _, stable := range earnStablecoins {
		if stable.Mint == mint {
			return stable.TokenProgram, true
		}
	}
	return "", false
}

type CrossMintEarnPolicyBinding struct {
	PolicyAccount     string `json:"policy_account"`
	ObservedSlot      uint64 `json:"observed_slot"`
	ObservedSignature string `json:"observed_signature"`
	SourceCommitment  string `json:"source_commitment"`
	ConstraintIndex   uint8  `json:"constraint_index"`
}

type CrossMintSwapPolicyBinding struct {
	PolicyAccount              string `json:"policy_account"`
	SourceShard                string `json:"source_shard"`
	EnrollmentGeneration       int64  `json:"enrollment_generation"`
	ObservedSlot               uint64 `json:"observed_slot"`
	ObservedSignature          string `json:"observed_signature"`
	SourceCommitment           string `json:"source_commitment"`
	MaxSlippageBPS             uint16 `json:"max_slippage_bps"`
	DailySourceMintSpendingCap uint64 `json:"daily_source_mint_spending_cap"`
	ManifestFingerprint        string `json:"manifest_fingerprint"`
}

type CrossMintPolicyBindings struct {
	Settings        string                     `json:"settings"`
	VaultIndex      uint8                      `json:"vault_index"`
	VaultPubkey     string                     `json:"vault_pubkey"`
	DelegatedSigner string                     `json:"delegated_signer"`
	Withdraw        CrossMintEarnPolicyBinding `json:"withdraw"`
	Swap            CrossMintSwapPolicyBinding `json:"swap"`
	Deposit         CrossMintEarnPolicyBinding `json:"deposit"`
}

type VaultPosition struct {
	VaultID                         int64
	Settings                        string
	VaultIndex                      int16
	VaultPubkey                     string
	PolicyID                        int64
	PolicyAuthority                 string
	PolicyAccount                   string
	SourceReserve                   string
	Market                          string
	Mint                            string
	AmountRaw                       int64
	SourceCollateralAmountRaw       int64
	SourceAmountSemantics           string
	IdleVaultLiquidityAmountRaw     *int64
	ObservedSlot                    int64
	ObservedAt                      time.Time
	BlockedReason                   string
	SourceCommittedInflowUSDMicros  int64
	SourceCommittedOutflowUSDMicros int64
	TargetCommittedInflowUSDMicros  int64
	TargetCommittedOutflowUSDMicros int64
}

type Decision struct {
	Eligible                 bool
	RouteKind                string
	Reason                   string
	VaultID                  int64
	MarketSlot               int64
	SourceReserve            string
	TargetReserve            string
	Mint                     string
	SourceMint               string
	TargetMint               string
	PolicyBindings           *CrossMintPolicyBindings
	CrossMintMaxValueLossBPS uint16
	AmountRaw                int64
	PrincipalUSDMicros       int64
	SourceAPYBPS             int64
	TargetAPYBPS             int64
	EdgeBPS                  int64
	AnnualYieldGainUSDMicros int64
	ExpectedNetGainUSDMicros int64
	EconomicPriority         int64
	EstimatedCostLamports    int64
	EstimatedCostUSDMicros   int64
	HoldingHorizonSeconds    int64
	ConfidencePPM            int64
	TargetCapacityUSDMicros  int64
	SnapshotHash             string
	ObservedAt               time.Time
}
