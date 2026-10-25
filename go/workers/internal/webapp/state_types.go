package webapp

// Response body for GET /api/smart-accounts/yield-optimization/earn-state.
// Ports web/apps/web/src/app/api/smart-accounts/yield-optimization/earn-state/route.ts
// and L/earn-state-serializers.server.ts. Raw amounts are integer unit strings
// exactly as the TS emits BigInt.toString(); TS null stays null. Deliberate
// additions over Next are autodeposit.lifecycleStatus (pathmap row
// autodeposit/state: the raw balance_sweep_targets.chain_status text the RN
// client already reads) and programId (foreman: the Squads program id the
// prepare wire ops use, for the RN holdings client).

type EarnStateVault struct {
	AccountIndex int    `json:"accountIndex"`
	Pubkey       string `json:"pubkey"`
}

type EarnStateProvenance struct {
	LastHoldingEventID      *string `json:"lastHoldingEventId"`
	LastRebalanceDecisionID *string `json:"lastRebalanceDecisionId"`
}

type EarnStateCurrentHolding struct {
	AmountRaw     string              `json:"amountRaw"`
	LiquidityMint string              `json:"liquidityMint"`
	Market        *string             `json:"market"`
	ObservedAt    string              `json:"observedAt"`
	ObservedSlot  string              `json:"observedSlot"`
	Provenance    EarnStateProvenance `json:"provenance"`
	Reserve       string              `json:"reserve"`
}

type EarnStateInitialHolding struct {
	LiquidityMint string  `json:"liquidityMint"`
	Market        *string `json:"market"`
	Reserve       string  `json:"reserve"`
	SupplyApyBps  *string `json:"supplyApyBps"`
}

type EarnStatePosition struct {
	CurrentHolding               EarnStateCurrentHolding `json:"currentHolding"`
	ID                           string                  `json:"id"`
	InitialHolding               EarnStateInitialHolding `json:"initialHolding"`
	CurrentTotalAmountRaw        string                  `json:"currentTotalAmountRaw"`
	CurrentTotalNominalUsdMicros string                  `json:"currentTotalNominalUsdMicros"`
	PrincipalAmountRaw           string                  `json:"principalAmountRaw"`
	Status                       string                  `json:"status"`
}

type EarnStateSetupPolicy struct {
	Account           string   `json:"account"`
	DelegatedSigners  []string `json:"delegatedSigners"`
	ID                string   `json:"id"`
	LastSeenSignature string   `json:"lastSeenSignature"`
	LastSeenSlot      string   `json:"lastSeenSlot"`
	Seed              string   `json:"seed"`
}

type EarnStatePolicy struct {
	Account              string                `json:"account"`
	DelegatedSigners     []string              `json:"delegatedSigners"`
	ID                   string                `json:"id"`
	KaminoLiquidityMints []string              `json:"kaminoLiquidityMints"`
	KaminoMarkets        []string              `json:"kaminoMarkets"`
	LastSeenSignature    string                `json:"lastSeenSignature"`
	LastSeenSlot         string                `json:"lastSeenSlot"`
	RiskProfile          *string               `json:"riskProfile"`
	RouteModes           []string              `json:"routeModes"`
	Seed                 string                `json:"seed"`
	SetupPolicy          *EarnStateSetupPolicy `json:"setupPolicy"`
	StableMints          []string              `json:"stableMints"`
	UniversePreset       *string               `json:"universePreset"`
	VaultIndex           int                   `json:"vaultIndex"`
	VaultPubkey          string                `json:"vaultPubkey"`
}

type EarnStateOnboardingPolicy struct {
	Account           string  `json:"account"`
	ID                string  `json:"id"`
	LastSeenSignature *string `json:"lastSeenSignature"`
	LastSeenSlot      *string `json:"lastSeenSlot"`
	Seed              string  `json:"seed"`
}

// EarnStateOnboarding is nextStep alone, or nextStep with the non-complete
// attempt's fields spread in (serializeEarnDepositOnboardingState). Next
// spreads the attempt, so absent attempt fields are omitted rather than null;
// the web client types them optional and reads them with ?. and ??.
type EarnStateOnboarding struct {
	NextStep             string                     `json:"nextStep"`
	DepositConfirmedSlot *string                    `json:"depositConfirmedSlot,omitempty"`
	DepositSignature     *string                    `json:"depositSignature,omitempty"`
	LastErrorCode        *string                    `json:"lastErrorCode,omitempty"`
	Policy               *EarnStateOnboardingPolicy `json:"policy,omitempty"`
	SetupPolicy          *EarnStateOnboardingPolicy `json:"setupPolicy,omitempty"`
	Status               *string                    `json:"status,omitempty"`
	UpdatedAt            *string                    `json:"updatedAt,omitempty"`
}

type EarnStateScheduledSweep struct {
	Classification        string  `json:"classification"`
	Confidence            string  `json:"confidence"`
	EligibleAfter         string  `json:"eligibleAfter"`
	ExecuteNowAvailableAt *string `json:"executeNowAvailableAt"`
	ID                    string  `json:"id"`
	LotCount              int     `json:"lotCount"`
	OriginalAmountRaw     string  `json:"originalAmountRaw"`
	Reason                string  `json:"reason"`
	RemainingAmountRaw    string  `json:"remainingAmountRaw"`
	SlotID                string  `json:"slotId"`
	Status                string  `json:"status"`
}

type EarnStateAutodeposit struct {
	Active                           bool                      `json:"active"`
	AmountPerPeriodRaw               string                    `json:"amountPerPeriodRaw"`
	BalanceSweepPolicyID             *string                   `json:"balanceSweepPolicyId"`
	Cluster                          *string                   `json:"cluster"`
	DelegatedSigner                  *string                   `json:"delegatedSigner"`
	DepositedThisPeriodRaw           string                    `json:"depositedThisPeriodRaw"`
	ExpiryTimestamp                  *string                   `json:"expiryTimestamp"`
	LastSeenSignature                string                    `json:"lastSeenSignature"`
	LastSeenSlot                     string                    `json:"lastSeenSlot"`
	LifecycleStatus                  string                    `json:"lifecycleStatus"`
	Nonce                            *string                   `json:"nonce"`
	PeriodLengthSeconds              *string                   `json:"periodLengthSeconds"`
	PolicyAccount                    string                    `json:"policyAccount"`
	PolicyConfirmedSlot              *string                   `json:"policyConfirmedSlot"`
	PolicySeed                       string                    `json:"policySeed"`
	PolicySignature                  *string                   `json:"policySignature"`
	RecurringDelegation              *string                   `json:"recurringDelegation"`
	RecurringDelegationConfirmedSlot *string                   `json:"recurringDelegationConfirmedSlot"`
	RecurringDelegationSignature     *string                   `json:"recurringDelegationSignature"`
	ScheduledSweeps                  []EarnStateScheduledSweep `json:"scheduledSweeps"`
	StartTimestamp                   string                    `json:"startTimestamp"`
	Status                           string                    `json:"status"`
	SubscriptionAuthority            *string                   `json:"subscriptionAuthority"`
	SubscriptionDelegatee            string                    `json:"subscriptionDelegatee"`
	VaultUsdcAta                     string                    `json:"vaultUsdcAta"`
	WalletBalanceFloorRaw            *string                   `json:"walletBalanceFloorRaw"`
	WalletUsdcAta                    string                    `json:"walletUsdcAta"`
}

type EarnStateBoundPolicy struct {
	Account     string `json:"account"`
	Seed        string `json:"seed"`
	SourceShard string `json:"sourceShard"`
}

type EarnStateAutoswapPolicy struct {
	Account           string `json:"account"`
	Seed              string `json:"seed"`
	SourceShard       string `json:"sourceShard"`
	LastSeenSignature string `json:"lastSeenSignature"`
	LastSeenSlot      string `json:"lastSeenSlot"`
}

type EarnStateAutoswap struct {
	BoundPolicies              []EarnStateBoundPolicy    `json:"boundPolicies"`
	DailySourceMintSpendingCap string                    `json:"dailySourceMintSpendingCap"`
	Enabled                    bool                      `json:"enabled"`
	Generation                 string                    `json:"generation"`
	MaxSlippageBps             int                       `json:"maxSlippageBps"`
	Policies                   []EarnStateAutoswapPolicy `json:"policies"`
	Status                     string                    `json:"status"`
}

type EarnStateAutoswapIndexPolicy struct {
	Account                    string `json:"account"`
	DailySourceMintSpendingCap string `json:"dailySourceMintSpendingCap"`
	DelegatedSigner            string `json:"delegatedSigner"`
	LastSeenSignature          string `json:"lastSeenSignature"`
	LastSeenSlot               string `json:"lastSeenSlot"`
	MaxSlippageBps             int    `json:"maxSlippageBps"`
	Seed                       string `json:"seed"`
	SourceCommitment           string `json:"sourceCommitment"`
	SourceShard                string `json:"sourceShard"`
}

type EarnStateAutoswapIndex struct {
	Policies []EarnStateAutoswapIndexPolicy `json:"policies"`
	State    string                         `json:"state"`
}

type EarnStateResponse struct {
	Autodeposit           *EarnStateAutodeposit   `json:"autodeposit"`
	Autoswap              *EarnStateAutoswap      `json:"autoswap"`
	AutoswapIndex         *EarnStateAutoswapIndex `json:"autoswapIndex"`
	AutoswapAvailable     bool                    `json:"autoswapAvailable"`
	CanonicalVaultPubkey  string                  `json:"canonicalVaultPubkey"`
	LoadErrors            map[string]bool         `json:"loadErrors"`
	Onboarding            EarnStateOnboarding     `json:"onboarding"`
	Policy                *EarnStatePolicy        `json:"policy"`
	Position              *EarnStatePosition      `json:"position"`
	PolicySignerPublicKey string                  `json:"policySignerPublicKey"`
	ProgramID             string                  `json:"programId"`
	SettingsPda           string                  `json:"settingsPda"`
	Vault                 EarnStateVault          `json:"vault"`
}
