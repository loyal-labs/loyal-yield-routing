package fleet

import "time"

const (
	KaminoProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	USDCMint      = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	reserveLength = 8624
)

var reserveDiscriminator = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}

type ReserveIdentity struct {
	Address string `json:"address"`
	Market  string `json:"market"`
	Mint    string `json:"mint"`
}

type ReserveState struct {
	ReserveIdentity
	Slot                   int64  `json:"slot"`
	LastUpdateSlot         int64  `json:"lastUpdateSlot"`
	SupplyAPYBPS           int64  `json:"supplyApyBps"`
	TotalSupplyUSDMicros   int64  `json:"totalSupplyUsdMicros"`
	EconomicLifetimeMillis int64  `json:"economicLifetimeMillis"`
	DataHash               string `json:"dataHash"`
}

type MarketSnapshot struct {
	Slot       int64                   `json:"slot"`
	ObservedAt time.Time               `json:"observedAt"`
	Hash       string                  `json:"hash"`
	Reserves   map[string]ReserveState `json:"reserves"`
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
	Reason                   string
	VaultID                  int64
	SourceSnapshotID         int64
	MarketSlot               int64
	SourceReserve            string
	TargetReserve            string
	Mint                     string
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
