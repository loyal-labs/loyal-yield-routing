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
	TotalSupplyUSDMicros   int64     `json:"totalSupplyUsdMicros"`
	EconomicLifetimeMillis int64     `json:"economicLifetimeMillis"`
	DataHash               string    `json:"dataHash"`
	// Capacity evidence in the reserve's protocol raw units, which equal USD
	// micros for the six-decimal Earn stablecoins at their code-owned $1
	// valuation. AvailableLiquidityRaw is what KLend can pay out now;
	// DepositLimitRaw is the reserve's total-supply ceiling (0 blocks deposits).
	// nil is unknown evidence: never zero, never unlimited.
	AvailableLiquidityRaw *int64  `json:"availableLiquidityRaw"`
	DepositLimitRaw       *uint64 `json:"depositLimitRaw"`
}

// DepositRoomRaw is how much more KLend accepts before total supply reaches
// the deposit limit: deposit_limit - total supply, at least 0. A limit beyond
// int64 saturates, since no amount the fleet moves can reach it. known is
// false when the limit was not observed.
func (r ReserveState) DepositRoomRaw() (room int64, known bool) {
	if r.DepositLimitRaw == nil || r.TotalSupplyUSDMicros < 0 {
		return 0, false
	}
	limit, supply := *r.DepositLimitRaw, uint64(r.TotalSupplyUSDMicros)
	if limit <= supply {
		return 0, true
	}
	if limit-supply > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(limit - supply), true
}

func (r ReserveState) TargetEligible() bool {
	return !r.LastUpdateStale && r.EconomicLifetimeMillis >= minimumPublicationLifetime.Milliseconds() &&
		r.TotalSupplyUSDMicros >= minimumSupplyUSDMicros && r.SupplyAPYBPS >= 0 && r.SupplyAPYBPS < 5_000
}

type MarketSnapshot struct {
	Slot       int64                   `json:"slot"`
	ObservedAt time.Time               `json:"observedAt"`
	Hash       string                  `json:"hash"`
	Reserves   map[string]ReserveState `json:"reserves"`
}

// submissionTerminalStates are the signed_route_submissions states that no
// longer own their opportunity: the schema's one-open-submission index and
// every Rust holding guard exclude exactly these. Any other state is in flight.
const submissionTerminalStates = `('reconciled','expired','failed')`

var earnStableMints = []string{CashMint, USDGMint, PYUSDMint, USDCMint, USDTMint, USDSMint}

// EarnStableMints is the canonical Earn registry in Rust EarnUniverse order
// (CASH, USDG, PYUSD, USDC, USDT, USDS).
func EarnStableMints() []string { return append([]string(nil), earnStableMints...) }

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
