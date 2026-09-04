package fleet

import (
	"encoding/json"
	"time"
)

const (
	KaminoProgram                            = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	USDCMint                                 = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	CashMint                                 = "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"
	USDGMint                                 = "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"
	PYUSDMint                                = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	USDTMint                                 = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	USDSMint                                 = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	reserveLength                            = 8624
	amountSemanticsKaminoCollateralDeposited = "kamino_obligation_collateral_deposited_amount"
	amountSemanticsRedeemableLiquidity       = "redeemable_liquidity_amount"
)

var reserveDiscriminator = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}

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
}

type MarketSnapshot struct {
	Cluster          string                  `json:"-"`
	OptimizerEpochID int64                   `json:"optimizerEpochId"`
	ExpiresAt        time.Time               `json:"expiresAt"`
	MintExpiresAt    map[string]time.Time    `json:"-"`
	Slot             int64                   `json:"slot"`
	ObservedAt       time.Time               `json:"observedAt"`
	Hash             string                  `json:"hash"`
	Reserves         map[string]ReserveState `json:"reserves"`
}

// MarketEpochReserve is byte-for-byte JSON compatible with the Rust
// MarketEpochReserve contract. The state identity comes from the retained
// Kamino monitor's durable confirmed verification row; it is never synthesized
// from a direct RPC read.
type MarketEpochReserve struct {
	StateEventID             int64     `json:"stateEventId"`
	AccountDataHash          string    `json:"accountDataHash"`
	StateObservedAt          time.Time `json:"stateObservedAt"`
	StateSlot                int64     `json:"stateSlot"`
	VerificationCommitment   string    `json:"verificationCommitment"`
	Reserve                  string    `json:"reserve"`
	Market                   *string   `json:"market"`
	LiquidityMint            string    `json:"liquidityMint"`
	MintDecimals             uint8     `json:"mintDecimals"`
	MarketPriceUSDMicros     int64     `json:"marketPriceUsdMicros"`
	ReserveLastUpdateSlot    int64     `json:"reserveLastUpdateSlot"`
	EconomicSlotLag          int64     `json:"economicSlotLag"`
	EconomicExpiresAt        time.Time `json:"economicExpiresAt"`
	ReserveLastUpdateStale   bool      `json:"reserveLastUpdateStale"`
	ReservePriceStatus       int16     `json:"reservePriceStatus"`
	MarketPriceLastUpdatedTS int64     `json:"marketPriceLastUpdatedTs"`
	AvailableAmountRaw       string    `json:"availableAmountRaw"`
	BorrowedAmountRaw        string    `json:"borrowedAmountRaw"`
	TotalSupplyAmountRaw     string    `json:"totalSupplyAmountRaw"`
	UtilizationPPM           int64     `json:"utilizationPpm"`
	BorrowAPYBPS             int64     `json:"borrowApyBps"`
	ObservedAt               time.Time `json:"observedAt"`
	Slot                     int64     `json:"slot"`
	SupplyAPYBPS             int64     `json:"supplyApyBps"`
	TotalSupplyUSDMicros     int64     `json:"totalSupplyUsdMicros"`
	TargetEligible           bool      `json:"targetEligible"`
}

func (r MarketEpochReserve) MarshalJSON() ([]byte, error) {
	type alias MarketEpochReserve
	return json.Marshal(struct {
		alias
		StateObservedAt   rustJSONTime `json:"stateObservedAt"`
		EconomicExpiresAt rustJSONTime `json:"economicExpiresAt"`
		ObservedAt        rustJSONTime `json:"observedAt"`
	}{alias: alias(r), StateObservedAt: rustJSONTime(r.StateObservedAt), EconomicExpiresAt: rustJSONTime(r.EconomicExpiresAt), ObservedAt: rustJSONTime(r.ObservedAt)})
}

type MarketMintBlocker struct {
	Code    string  `json:"code"`
	Reserve *string `json:"reserve"`
	Detail  string  `json:"detail"`
}

type MarketMintCoverage struct {
	Mint                       string              `json:"mint"`
	CatalogReserveCount        int                 `json:"catalogReserveCount"`
	VerifiedReserveCount       int                 `json:"verifiedReserveCount"`
	EligibleTargetReserveCount int                 `json:"eligibleTargetReserveCount"`
	Complete                   bool                `json:"complete"`
	ExpiresAt                  *time.Time          `json:"expiresAt"`
	Blockers                   []MarketMintBlocker `json:"blockers"`
}

// ImmutableMarketEpoch mirrors the Rust serde camelCase contract exactly.
func (c MarketMintCoverage) MarshalJSON() ([]byte, error) {
	type alias MarketMintCoverage
	var expiresAt *rustJSONTime
	if c.ExpiresAt != nil {
		value := rustJSONTime(*c.ExpiresAt)
		expiresAt = &value
	}
	return json.Marshal(struct {
		alias
		ExpiresAt *rustJSONTime `json:"expiresAt"`
	}{alias: alias(c), ExpiresAt: expiresAt})
}

type ImmutableMarketEpoch struct {
	OptimizerEpochID       int64                `json:"optimizerEpochId"`
	Fingerprint            string               `json:"fingerprint"`
	CatalogFingerprint     string               `json:"catalogFingerprint"`
	CapturedAt             time.Time            `json:"capturedAt"`
	ExpiresAt              time.Time            `json:"expiresAt"`
	CatalogExpiresAt       time.Time            `json:"catalogExpiresAt"`
	CatalogReserveCount    int                  `json:"catalogReserveCount"`
	OldestMarketObservedAt *time.Time           `json:"oldestMarketObservedAt"`
	NewestMarketObservedAt *time.Time           `json:"newestMarketObservedAt"`
	MinimumMarketSlot      *int64               `json:"minimumMarketSlot"`
	MaximumMarketSlot      *int64               `json:"maximumMarketSlot"`
	MintCoverage           []MarketMintCoverage `json:"mintCoverage"`
	Reserves               []MarketEpochReserve `json:"reserves"`
}

func (e ImmutableMarketEpoch) MarshalJSON() ([]byte, error) {
	type alias ImmutableMarketEpoch
	var oldest, newest *rustJSONTime
	if e.OldestMarketObservedAt != nil {
		value := rustJSONTime(*e.OldestMarketObservedAt)
		oldest = &value
	}
	if e.NewestMarketObservedAt != nil {
		value := rustJSONTime(*e.NewestMarketObservedAt)
		newest = &value
	}
	return json.Marshal(struct {
		alias
		CapturedAt             rustJSONTime  `json:"capturedAt"`
		ExpiresAt              rustJSONTime  `json:"expiresAt"`
		CatalogExpiresAt       rustJSONTime  `json:"catalogExpiresAt"`
		OldestMarketObservedAt *rustJSONTime `json:"oldestMarketObservedAt"`
		NewestMarketObservedAt *rustJSONTime `json:"newestMarketObservedAt"`
	}{alias: alias(e), CapturedAt: rustJSONTime(e.CapturedAt), ExpiresAt: rustJSONTime(e.ExpiresAt), CatalogExpiresAt: rustJSONTime(e.CatalogExpiresAt), OldestMarketObservedAt: oldest, NewestMarketObservedAt: newest})
}

type rustJSONTime time.Time

func (value rustJSONTime) MarshalJSON() ([]byte, error) {
	timestamp := time.Time(value).UTC()
	layout := "2006-01-02T15:04:05Z"
	nanoseconds := timestamp.Nanosecond()
	if nanoseconds != 0 {
		switch {
		case nanoseconds%1_000_000 == 0:
			layout = "2006-01-02T15:04:05.000Z"
		case nanoseconds%1_000 == 0:
			layout = "2006-01-02T15:04:05.000000Z"
		default:
			layout = "2006-01-02T15:04:05.000000000Z"
		}
	}
	return json.Marshal(timestamp.Format(layout))
}

func (e ImmutableMarketEpoch) OptimizerEnvelopeExpiresAt() time.Time {
	result := e.ExpiresAt
	for _, coverage := range e.MintCoverage {
		if coverage.Complete && coverage.ExpiresAt != nil && coverage.ExpiresAt.After(result) {
			result = *coverage.ExpiresAt
		}
	}
	return result
}

func (e ImmutableMarketEpoch) MintExpiresAt(mint string) (time.Time, bool) {
	for _, coverage := range e.MintCoverage {
		if coverage.Complete && coverage.Mint == mint && coverage.ExpiresAt != nil {
			return *coverage.ExpiresAt, true
		}
	}
	return time.Time{}, false
}

func (e ImmutableMarketEpoch) Reserve(address string) (MarketEpochReserve, bool) {
	for _, reserve := range e.Reserves {
		if reserve.Reserve == address {
			return reserve, true
		}
	}
	return MarketEpochReserve{}, false
}

var earnStableMints = []string{CashMint, USDGMint, PYUSDMint, USDCMint, USDTMint, USDSMint}

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
	PolicyAccount                   string
	SourceReserve                   string
	Market                          string
	Mint                            string
	AmountRaw                       int64
	SourceCollateralAmountRaw       int64
	SourceAmountSemantics           string
	IdleVaultLiquidityAmountRaw     *int64
	SnapshotID                      int64
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
	SourceSnapshotID         int64
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

type PublishResult struct {
	Inserted      bool
	OpportunityID int64
	EpochID       int64
	Reason        string
}
