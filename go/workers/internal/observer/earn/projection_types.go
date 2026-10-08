package earn

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// The projection inputs below mirror loyal-yield-store's serde types field for
// field (snake_case JSON). The Go Earn application writes the same rows as the
// stopped Rust earn-domain-bridge; the parity test feeds one JSON
// script to both implementations.

// PolicyMatchInput is one detected route or setup policy (route_policies row).
type PolicyMatchInput struct {
	Signature            string          `json:"signature"`
	Slot                 uint64          `json:"slot"`
	Cluster              string          `json:"cluster"`
	SourceCommitment     string          `json:"source_commitment"`
	Settings             string          `json:"settings"`
	Authority            string          `json:"authority"`
	PolicySeed           uint64          `json:"policy_seed"`
	PolicyAccount        string          `json:"policy_account"`
	VaultIndex           uint8           `json:"vault_index"`
	VaultPubkey          string          `json:"vault_pubkey"`
	DelegatedSigners     []string        `json:"delegated_signers"`
	Threshold            uint16          `json:"threshold"`
	RouteModes           []string        `json:"route_modes"`
	StableMints          []string        `json:"stable_mints"`
	KaminoMarkets        []string        `json:"kamino_markets"`
	KaminoLiquidityMints []string        `json:"kamino_liquidity_mints"`
	UniversePreset       *string         `json:"universe_preset"`
	RiskProfile          *string         `json:"risk_profile"`
	SwapLanes            json.RawMessage `json:"swap_lanes"`
}

// CrossMintSwapPolicyManifestInput is one strict generalized Jupiter policy.
type CrossMintSwapPolicyManifestInput struct {
	Signature                  string  `json:"signature"`
	Slot                       uint64  `json:"slot"`
	Cluster                    string  `json:"cluster"`
	SourceCommitment           string  `json:"source_commitment"`
	Mutation                   string  `json:"mutation"`
	Settings                   string  `json:"settings"`
	Authority                  string  `json:"authority"`
	PolicySeed                 *uint64 `json:"policy_seed"`
	PolicyAccount              string  `json:"policy_account"`
	VaultIndex                 uint8   `json:"vault_index"`
	VaultPubkey                string  `json:"vault_pubkey"`
	DelegatedSigner            string  `json:"delegated_signer"`
	SourceShard                string  `json:"source_shard"`
	MaxSlippageBPS             uint16  `json:"max_slippage_bps"`
	DailySourceMintSpendingCap uint64  `json:"daily_source_mint_spending_cap"`
	ManifestFingerprint        string  `json:"manifest_fingerprint"`
}

// PolicyRemovalInput closes one policy account of any family.
type PolicyRemovalInput struct {
	Signature        string `json:"signature"`
	Slot             uint64 `json:"slot"`
	Cluster          string `json:"cluster"`
	SourceCommitment string `json:"source_commitment"`
	Settings         string `json:"settings"`
	Authority        string `json:"authority"`
	PolicyAccount    string `json:"policy_account"`
}

// BalanceSweepPolicyMatchInput is one detected Autodeposit policy.
type BalanceSweepPolicyMatchInput struct {
	Signature          string   `json:"signature"`
	Slot               uint64   `json:"slot"`
	Cluster            string   `json:"cluster"`
	Settings           string   `json:"settings"`
	Authority          string   `json:"authority"`
	PolicySeed         uint64   `json:"policy_seed"`
	PolicyAccount      string   `json:"policy_account"`
	VaultIndex         uint8    `json:"vault_index"`
	VaultPubkey        string   `json:"vault_pubkey"`
	Wallet             string   `json:"wallet"`
	WalletUSDCATA      string   `json:"wallet_usdc_ata"`
	VaultUSDCATA       string   `json:"vault_usdc_ata"`
	TokenMint          string   `json:"token_mint"`
	WalletTokenATA     string   `json:"wallet_token_ata"`
	VaultTokenATA      string   `json:"vault_token_ata"`
	DelegatedSigners   []string `json:"delegated_signers"`
	Threshold          uint16   `json:"threshold"`
	MaxAmountPerPeriod uint64   `json:"max_amount_per_period"`
}

// RecurringDelegationObserved is one confirmed Subscriptions create.
type RecurringDelegationObserved struct {
	Wallet                string `json:"wallet"`
	VaultPubkey           string `json:"vault_pubkey"`
	SubscriptionAuthority string `json:"subscription_authority"`
	RecurringDelegation   string `json:"recurring_delegation"`
	Nonce                 uint64 `json:"nonce"`
	AmountPerPeriod       uint64 `json:"amount_per_period"`
	PeriodLengthSeconds   uint64 `json:"period_length_seconds"`
	StartTimestamp        int64  `json:"start_timestamp"`
	ExpiryTimestamp       int64  `json:"expiry_timestamp"`
	Signature             string `json:"signature"`
	Slot                  uint64 `json:"slot"`
}

// EarnMaxPolicySetProjectionInput is one Earn MAX three-policy manifest.
type EarnMaxPolicySetProjectionInput struct {
	Settings          string          `json:"settings"`
	VaultIndex        uint8           `json:"vault_index"`
	Vault             string          `json:"vault"`
	ManifestVersion   string          `json:"manifest_version"`
	ManifestSHA256    string          `json:"manifest_sha256"`
	PolicySeedBase    uint64          `json:"policy_seed_base"`
	Status            string          `json:"status"`
	PolicyAccounts    json.RawMessage `json:"policy_accounts"`
	ObservedSignature string          `json:"observed_signature"`
	ObservedSlot      uint64          `json:"observed_slot"`
	ObservedAt        time.Time       `json:"observed_at"`
}

// EarnMaxIntent is the serde enum {"withdraw":{...}} | {"cancel":{...}}.
type EarnMaxIntent struct {
	Withdraw *EarnMaxWithdrawIntent `json:"withdraw,omitempty"`
	Cancel   *EarnMaxCancelIntent   `json:"cancel,omitempty"`
}

type EarnMaxWithdrawIntent struct {
	RequestID          string  `json:"request_id"`
	DestinationAccount string  `json:"destination_account"`
	AmountRaw          *uint64 `json:"amount_raw"`
}

type EarnMaxCancelIntent struct {
	RequestID string `json:"request_id"`
}

// EarnMaxIntentProjectionInput is one root-signed Earn MAX memo.
type EarnMaxIntentProjectionInput struct {
	Settings         string        `json:"settings"`
	VaultIndex       uint8         `json:"vault_index"`
	Signature        string        `json:"signature"`
	InstructionIndex uint16        `json:"instruction_index"`
	Slot             uint64        `json:"slot"`
	ObservedAt       time.Time     `json:"observed_at"`
	Intent           EarnMaxIntent `json:"intent"`
}

// EarnReserveMutation is one confirmed reserve position.
type EarnReserveMutation struct {
	Reserve          string          `json:"reserve"`
	Market           *string         `json:"market"`
	LiquidityMint    string          `json:"liquidity_mint"`
	AmountRaw        uint64          `json:"amount_raw"`
	HasValue         bool            `json:"has_value"`
	SupplyAPYBPS     *int64          `json:"supply_apy_bps"`
	BorrowAPYBPS     *int64          `json:"borrow_apy_bps"`
	PlanningMetadata json.RawMessage `json:"planning_metadata"`
}

// EarnIdleTokenMutation is one confirmed idle vault token account.
type EarnIdleTokenMutation struct {
	Mint             string     `json:"mint"`
	AmountRaw        uint64     `json:"amount_raw"`
	Owner            string     `json:"owner"`
	TokenAccount     string     `json:"token_account"`
	ObservedSlot     uint64     `json:"observed_slot"`
	ObservedAt       *time.Time `json:"observed_at"`
	SourceCommitment string     `json:"source_commitment"`
}

type EarnPolicyOnlyMutation struct {
	RoutePolicy PolicyMatchInput `json:"route_policy"`
	SetupPolicy PolicyMatchInput `json:"setup_policy"`
}

type EarnDepositMutation struct {
	RoutePolicy         PolicyMatchInput        `json:"route_policy"`
	SetupPolicy         *PolicyMatchInput       `json:"setup_policy"`
	DepositSignature    string                  `json:"deposit_signature"`
	DepositSlot         uint64                  `json:"deposit_slot"`
	ObservedSlot        uint64                  `json:"observed_slot"`
	DepositMint         string                  `json:"deposit_mint"`
	PrincipalAmountRaw  uint64                  `json:"principal_amount_raw"`
	TargetReserve       string                  `json:"target_reserve"`
	Market              *string                 `json:"market"`
	LiquidityMint       string                  `json:"liquidity_mint"`
	TargetSupplyAPYBPS  *int64                  `json:"target_supply_apy_bps"`
	Wallet              string                  `json:"wallet"`
	SmartAccountAddress string                  `json:"smart_account_address"`
	ReserveState        []EarnReserveMutation   `json:"reserve_state"`
	IdleState           []EarnIdleTokenMutation `json:"idle_state"`
	ObservedAt          *time.Time              `json:"observed_at"`
}

type EarnWithdrawalMutation struct {
	RoutePolicy         PolicyMatchInput        `json:"route_policy"`
	WithdrawalSignature string                  `json:"withdrawal_signature"`
	ConfirmedSlot       uint64                  `json:"confirmed_slot"`
	ObservedSlot        uint64                  `json:"observed_slot"`
	Wallet              string                  `json:"wallet"`
	VaultPubkey         string                  `json:"vault_pubkey"`
	TargetReserve       string                  `json:"target_reserve"`
	Market              *string                 `json:"market"`
	LiquidityMint       string                  `json:"liquidity_mint"`
	WithdrawnAmountRaw  uint64                  `json:"withdrawn_amount_raw"`
	RemainingAmountRaw  uint64                  `json:"remaining_amount_raw"`
	ReserveState        []EarnReserveMutation   `json:"reserve_state"`
	IdleState           []EarnIdleTokenMutation `json:"idle_state"`
	ObservedAt          *time.Time              `json:"observed_at"`
}

type EarnCleanupMutation struct {
	Settings         string     `json:"settings"`
	VaultIndex       uint8      `json:"vault_index"`
	VaultPubkey      string     `json:"vault_pubkey"`
	CleanupSignature string     `json:"cleanup_signature"`
	ConfirmedSlot    uint64     `json:"confirmed_slot"`
	ObservedAt       *time.Time `json:"observed_at"`
}

type EarnRefundMutation struct {
	Cluster         string     `json:"cluster"`
	FullCleanup     bool       `json:"full_cleanup"`
	Settings        string     `json:"settings"`
	VaultIndex      uint8      `json:"vault_index"`
	VaultPubkey     string     `json:"vault_pubkey"`
	Wallet          string     `json:"wallet"`
	RefundSignature string     `json:"refund_signature"`
	ConfirmedSlot   uint64     `json:"confirmed_slot"`
	RefundKind      string     `json:"refund_kind"`
	ObservedAt      *time.Time `json:"observed_at"`
}

// EarnMutation is the serde enum EarnDirectMutation: exactly one variant, or
// none for Noop ("Noop" as a bare JSON string).
type EarnMutation struct {
	PolicyOnly *EarnPolicyOnlyMutation `json:"PolicyOnly,omitempty"`
	Deposit    *EarnDepositMutation    `json:"Deposit,omitempty"`
	Withdrawal *EarnWithdrawalMutation `json:"Withdrawal,omitempty"`
	Cleanup    *EarnCleanupMutation    `json:"Cleanup,omitempty"`
	Refund     *EarnRefundMutation     `json:"Refund,omitempty"`
}

func (m EarnMutation) MarshalJSON() ([]byte, error) {
	type variants EarnMutation
	if m == (EarnMutation{}) {
		return []byte(`"Noop"`), nil
	}
	return json.Marshal(variants(m))
}

func (m *EarnMutation) UnmarshalJSON(raw []byte) error {
	type variants EarnMutation
	if string(raw) == `"Noop"` {
		*m = EarnMutation{}
		return nil
	}
	return json.Unmarshal(raw, (*variants)(m))
}

func slotBigint(slot uint64) (int64, error) {
	if slot > math.MaxInt64 {
		return 0, fmt.Errorf("slot %d exceeds PostgreSQL BIGINT", slot)
	}
	return int64(slot), nil
}

func amountBigint(amount uint64) (int64, error) {
	if amount > math.MaxInt64 {
		return 0, fmt.Errorf("amount %d exceeds PostgreSQL BIGINT", amount)
	}
	return int64(amount), nil
}

func commitmentRank(commitment string) (int, error) {
	switch commitment {
	case "unknown":
		// Migration 36 rows predate commitment tracking; the weakest rank lets a
		// later finalized observation repair them.
		return 0, nil
	case "processed":
		return 1, nil
	case "confirmed":
		return 2, nil
	case "finalized":
		return 3, nil
	}
	return 0, fmt.Errorf("unsupported policy source commitment %q", commitment)
}

func strongerCommitment(left, right string) (string, error) {
	a, err := commitmentRank(left)
	if err != nil {
		return "", err
	}
	b, err := commitmentRank(right)
	if err != nil {
		return "", err
	}
	return []string{"unknown", "processed", "confirmed", "finalized"}[max(a, b)], nil
}
