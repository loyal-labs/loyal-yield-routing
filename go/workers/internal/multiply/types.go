// Package multiply ports the Earn MAX (Kamino Multiply) route controller from
// crates/loyal-fleet-worker/src/multiply and the durable state contract from
// crates/loyal-yield-store/src/{fleet_orchestration/multiply.rs,multiply_state_store.rs}.
//
// The persisted JSON must stay byte-compatible with the Rust writer: the same
// field names, enum spellings, and tagged position encoding. A Go worker and
// the retained Rust worker read and write the same rows, so any drift here is
// a cross-binary correctness bug, not a style issue.
package multiply

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Versions and lease bounds mirror the Rust constants exactly.
const (
	StateSchemaVersion     = 9
	EngineVersion          = "earn_max_v2"
	DefaultLeaseSeconds    = 30
	defaultLeaseExpiry     = DefaultLeaseSeconds * time.Second
	ManifestVersion        = "earn-max-v2"
	tickLeaseExpirySeconds = 30
)

// StrategyKey is one fixed collateral/debt lane. Spelling matches the Rust
// serde snake_case encoding stored in multiply_operations.strategy_key.
type StrategyKey string

const (
	OnycUsdc       StrategyKey = "onyc_usdc"
	OnycUsds       StrategyKey = "onyc_usds"
	PrimeUsdc      StrategyKey = "prime_usdc"
	PrimePyusd     StrategyKey = "prime_pyusd"
	PrimeUsds      StrategyKey = "prime_usds"
	SyrupUsdcUsdc  StrategyKey = "syrup_usdc_usdc"
	SyrupUsdcPyusd StrategyKey = "syrup_usdc_pyusd"
)

var strategyKeys = []StrategyKey{OnycUsdc, OnycUsds, PrimeUsdc, PrimePyusd, PrimeUsds, SyrupUsdcUsdc, SyrupUsdcPyusd}

// StrategyKeys returns the catalog in the Rust enum declaration order, which
// fixes observation, planning, and reconciliation ordering.
func StrategyKeys() []StrategyKey { return append([]StrategyKey(nil), strategyKeys...) }

func (k StrategyKey) Valid() bool {
	for _, value := range strategyKeys {
		if value == k {
			return true
		}
	}
	return false
}

// RouteGoal is the persisted product intent. Never rewritten by observation.
type RouteGoal string

const (
	GoalIdle           RouteGoal = "idle"
	GoalDeploy         RouteGoal = "deploy"
	GoalWithdraw       RouteGoal = "withdraw"
	GoalClaimed        RouteGoal = "claimed"
	GoalManualRecovery RouteGoal = "manual_recovery"
)

// MultiplyAction spellings match multiply_operations.action.
type MultiplyAction string

const (
	ActionRequestWithdrawal           MultiplyAction = "request_withdrawal"
	ActionCancelWithdrawal            MultiplyAction = "cancel_withdrawal"
	ActionDepositClaimAsset           MultiplyAction = "deposit_claim_asset"
	ActionSwapClaimToCollateral       MultiplyAction = "swap_claim_to_collateral"
	ActionDepositCollateral           MultiplyAction = "deposit_collateral"
	ActionBorrowDebt                  MultiplyAction = "borrow_debt"
	ActionSwapDebtToCollateral        MultiplyAction = "swap_debt_to_collateral"
	ActionWithdrawCollateral          MultiplyAction = "withdraw_collateral"
	ActionSwapCollateralToDebt        MultiplyAction = "swap_collateral_to_debt"
	ActionRepayDebt                   MultiplyAction = "repay_debt"
	ActionWithdrawRemainingCollateral MultiplyAction = "withdraw_remaining_collateral"
	ActionSwapCollateralToClaim       MultiplyAction = "swap_collateral_to_claim"
	ActionClaim                       MultiplyAction = "claim"
)

func (a MultiplyAction) Valid() bool {
	switch a {
	case ActionRequestWithdrawal, ActionCancelWithdrawal, ActionDepositClaimAsset,
		ActionSwapClaimToCollateral, ActionDepositCollateral, ActionBorrowDebt,
		ActionSwapDebtToCollateral, ActionWithdrawCollateral, ActionSwapCollateralToDebt,
		ActionRepayDebt, ActionWithdrawRemainingCollateral, ActionSwapCollateralToClaim,
		ActionClaim:
		return true
	}
	return false
}

func (a MultiplyAction) IsStrategyAction() bool {
	switch a {
	case ActionRequestWithdrawal, ActionCancelWithdrawal, ActionDepositClaimAsset, ActionClaim:
		return false
	}
	return a.Valid()
}

// OperationStatus is the immutable attempt lifecycle spelled as SQL values.
type OperationStatus string

const (
	StatusPrepared              OperationStatus = "prepared"
	StatusSignedPersisted       OperationStatus = "signed_persisted"
	StatusBroadcastIntent       OperationStatus = "broadcast_intent"
	StatusConfirmed             OperationStatus = "confirmed"
	StatusReconciliationPending OperationStatus = "reconciliation_pending"
	StatusReconciled            OperationStatus = "reconciled"
	StatusExpired               OperationStatus = "expired"
	StatusManualRecovery        OperationStatus = "manual_recovery"
)

func (s OperationStatus) IsTerminal() bool {
	return s == StatusReconciled || s == StatusExpired || s == StatusManualRecovery
}

// WithdrawalStatus spellings match the Rust serde encoding.
type WithdrawalStatus string

const (
	WithdrawalRequested WithdrawalStatus = "requested"
	WithdrawalUnwinding WithdrawalStatus = "unwinding"
	WithdrawalClaimable WithdrawalStatus = "claimable"
	WithdrawalClaimed   WithdrawalStatus = "claimed"
)

// TokenBalance is a raw integer custody balance tagged with mint and program.
type TokenBalance struct {
	Account      string `json:"account"`
	Mint         string `json:"mint"`
	TokenProgram string `json:"tokenProgram"`
	AmountRaw    uint64 `json:"amountRaw"`
}

func (b TokenBalance) Validate() error {
	if strings.TrimSpace(b.Account) == "" || strings.TrimSpace(b.Mint) == "" || strings.TrimSpace(b.TokenProgram) == "" {
		return errors.New("invalid token balance identity")
	}
	return nil
}

// MultiplyPosition is the tagged enum persisted in route state JSON, exactly
// as Rust serde writes it: {"kind":"idle","claim":...} or
// {"kind":"active","strategyKey":...,...} with the arm fields at top level.
type MultiplyPosition struct {
	Kind            string        `json:"kind"`
	Claim           *TokenBalance `json:"claim,omitempty"`
	StrategyKey     *StrategyKey  `json:"strategyKey,omitempty"`
	Obligation      *string       `json:"obligation,omitempty"`
	Collateral      *TokenBalance `json:"collateral,omitempty"`
	Debt            *TokenBalance `json:"debt,omitempty"`
	DebtAmountSF    *string       `json:"debtAmountSf,omitempty"`
	HealthFactorPPM *uint64       `json:"healthFactorPpm,omitempty"`
}

// NewIdlePosition builds the idle arm.
func NewIdlePosition(claim TokenBalance) MultiplyPosition {
	return MultiplyPosition{Kind: "idle", Claim: &claim}
}

// NewActivePosition builds the active arm.
func NewActivePosition(strategyKey StrategyKey, obligation string, collateral, debt TokenBalance, debtAmountSF string, healthFactorPPM uint64) MultiplyPosition {
	key := strategyKey
	obligationCopy := obligation
	sfCopy := debtAmountSF
	hf := healthFactorPPM
	return MultiplyPosition{
		Kind:            "active",
		StrategyKey:     &key,
		Obligation:      &obligationCopy,
		Collateral:      &collateral,
		Debt:            &debt,
		DebtAmountSF:    &sfCopy,
		HealthFactorPPM: &hf,
	}
}

func (p MultiplyPosition) ActiveStrategyKey() StrategyKey {
	if p.StrategyKey == nil {
		return ""
	}
	return *p.StrategyKey
}

func (p MultiplyPosition) ObservedAccounts() []TokenBalance {
	if p.Kind == "idle" && p.Claim != nil {
		return []TokenBalance{*p.Claim}
	}
	if p.Kind == "active" && p.Collateral != nil && p.Debt != nil {
		return []TokenBalance{*p.Collateral, *p.Debt}
	}
	return nil
}

func (p MultiplyPosition) Validate() error {
	switch p.Kind {
	case "idle":
		if p.Claim == nil || p.StrategyKey != nil || p.Obligation != nil || p.Collateral != nil ||
			p.Debt != nil || p.DebtAmountSF != nil || p.HealthFactorPPM != nil {
			return errors.New("idle position requires exactly a claim balance")
		}
	case "active":
		if p.Claim != nil || p.StrategyKey == nil || p.Obligation == nil || p.Collateral == nil ||
			p.Debt == nil || p.DebtAmountSF == nil || p.HealthFactorPPM == nil {
			return errors.New("active position requires exactly active fields")
		}
		if !p.StrategyKey.Valid() || strings.TrimSpace(*p.Obligation) == "" {
			return errors.New("active position identity drifted")
		}
	default:
		return fmt.Errorf("unknown position kind %q", p.Kind)
	}
	for _, balance := range p.ObservedAccounts() {
		if err := balance.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DepositEvidence proves one confirmed wallet deposit.
type DepositEvidence struct {
	RequestID            string    `json:"requestId"`
	TransactionSignature string    `json:"transactionSignature"`
	WalletAccount        string    `json:"walletAccount"`
	WalletPreAmountRaw   uint64    `json:"walletPreAmountRaw"`
	WalletPostAmountRaw  uint64    `json:"walletPostAmountRaw"`
	VaultPreAmountRaw    uint64    `json:"vaultPreAmountRaw"`
	VaultPostAmountRaw   uint64    `json:"vaultPostAmountRaw"`
	AmountRaw            uint64    `json:"amountRaw"`
	ObservedSlot         uint64    `json:"observedSlot"`
	ObservedAt           time.Time `json:"observedAt"`
}

// Withdrawal is the product SLA record for one user withdrawal request.
type Withdrawal struct {
	RequestID          string           `json:"requestId"`
	DestinationAccount string           `json:"destinationAccount"`
	AmountRaw          uint64           `json:"amountRaw"`
	Status             WithdrawalStatus `json:"status"`
	RequestedAt        time.Time        `json:"requestedAt"`
	ReadyBy            time.Time        `json:"readyBy"`
	UnwindCompletedAt  *time.Time       `json:"unwindCompletedAt"`
	ClaimSignature     *string          `json:"claimSignature"`
}

func (w Withdrawal) validate(now time.Time) error {
	if strings.TrimSpace(w.RequestID) == "" || strings.TrimSpace(w.DestinationAccount) == "" ||
		w.AmountRaw == 0 || w.ReadyBy.Before(w.RequestedAt) || w.ReadyBy.Sub(w.RequestedAt) > 10*time.Minute {
		return errors.New("withdrawal violates identity, amount, or time bounds")
	}
	if w.Status == WithdrawalClaimed && (w.ClaimSignature == nil || strings.TrimSpace(*w.ClaimSignature) == "") {
		return errors.New("claimed withdrawal requires its claim signature")
	}
	return nil
}

// RouteState is the schema-9 route document, byte-compatible with the Rust
// MultiplyRouteState serde encoding (camelCase, deny unknown fields).
type RouteState struct {
	SchemaVersion        int              `json:"schemaVersion"`
	EngineVersion        string           `json:"engineVersion"`
	RouteKey             string           `json:"routeKey"`
	Settings             string           `json:"settings"`
	VaultIndex           uint8            `json:"vaultIndex"`
	Vault                string           `json:"vault"`
	PolicySeedBase       uint64           `json:"policySeedBase"`
	Generation           uint64           `json:"generation"`
	Cycle                uint64           `json:"cycle"`
	Goal                 RouteGoal        `json:"goal"`
	Position             MultiplyPosition `json:"position"`
	Deposit              *DepositEvidence `json:"deposit"`
	Withdrawal           *Withdrawal      `json:"withdrawal"`
	CurrentOperationID   *string          `json:"currentOperationId"`
	ManualRecoveryReason *string          `json:"manualRecoveryReason"`
	ObservedSlot         uint64           `json:"observedSlot"`
	ObservedAt           time.Time        `json:"observedAt"`
}

// NewRouteState mirrors MultiplyRouteState::new including its validation.
func NewRouteState(routeKey, settings string, vaultIndex uint8, vault string, policySeedBase uint64, claim TokenBalance, observedSlot uint64, observedAt time.Time) (*RouteState, error) {
	state := &RouteState{
		SchemaVersion:  StateSchemaVersion,
		EngineVersion:  EngineVersion,
		RouteKey:       routeKey,
		Settings:       settings,
		VaultIndex:     vaultIndex,
		Vault:          vault,
		PolicySeedBase: policySeedBase,
		Generation:     1,
		Cycle:          1,
		Goal:           GoalIdle,
		Position:       NewIdlePosition(claim),
		ObservedSlot:   observedSlot,
		ObservedAt:     observedAt,
	}
	if err := state.ValidatePersisted(); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *RouteState) WithdrawalMatches(requestID, destinationAccount string, amountRaw uint64) bool {
	return s.Withdrawal != nil &&
		s.Withdrawal.RequestID == requestID &&
		s.Withdrawal.DestinationAccount == destinationAccount &&
		s.Withdrawal.AmountRaw == amountRaw
}

// ValidatePersisted mirrors Rust validate_persisted exactly.
func (s *RouteState) ValidatePersisted() error {
	if s.SchemaVersion != StateSchemaVersion || s.EngineVersion != EngineVersion ||
		strings.TrimSpace(s.RouteKey) == "" || strings.TrimSpace(s.Settings) == "" ||
		strings.TrimSpace(s.Vault) == "" || s.PolicySeedBase == 0 ||
		s.Generation == 0 || s.Cycle == 0 || s.ObservedSlot == 0 {
		return errors.New("invalid Multiply route identity or observation")
	}
	if err := s.Position.Validate(); err != nil {
		return err
	}
	if s.Goal == GoalManualRecovery && (s.ManualRecoveryReason == nil || strings.TrimSpace(*s.ManualRecoveryReason) == "") {
		return errors.New("manual recovery requires a reason")
	}
	if s.Goal != GoalManualRecovery && s.ManualRecoveryReason != nil {
		return errors.New("manual recovery reason is present outside manual recovery")
	}
	if s.Withdrawal != nil {
		if err := s.Withdrawal.validate(time.Now()); err != nil {
			return err
		}
	}
	if s.Goal != GoalIdle && s.Goal != GoalDeploy && s.Goal != GoalWithdraw && s.Goal != GoalClaimed && s.Goal != GoalManualRecovery {
		return fmt.Errorf("unknown route goal %q", s.Goal)
	}
	return nil
}

// AdmitDeposit mirrors Rust admit_deposit: one confirmed deposit starts Deploy.
func (s *RouteState) AdmitDeposit(evidence DepositEvidence) error {
	if strings.TrimSpace(evidence.RequestID) == "" || strings.TrimSpace(evidence.TransactionSignature) == "" ||
		strings.TrimSpace(evidence.WalletAccount) == "" || evidence.AmountRaw == 0 ||
		evidence.WalletPreAmountRaw < evidence.WalletPostAmountRaw ||
		evidence.VaultPostAmountRaw < evidence.VaultPreAmountRaw ||
		evidence.WalletPreAmountRaw-evidence.WalletPostAmountRaw != evidence.AmountRaw ||
		evidence.VaultPostAmountRaw-evidence.VaultPreAmountRaw != evidence.AmountRaw {
		return errors.New("deposit evidence does not prove equal wallet and vault deltas")
	}
	s.Generation++
	s.Cycle++
	s.Goal = GoalDeploy
	s.Deposit = &evidence
	s.Withdrawal = nil
	s.ManualRecoveryReason = nil
	return nil
}

// RequestWithdrawal mirrors Rust request_withdrawal including idempotent
// same-request replay.
func (s *RouteState) RequestWithdrawal(requestID, destinationAccount string, amountRaw uint64, requestedAt time.Time) (bool, error) {
	if s.WithdrawalMatches(requestID, destinationAccount, amountRaw) {
		return false, nil
	}
	pending := s.Withdrawal != nil && s.Withdrawal.Status != WithdrawalClaimed
	reused := s.Withdrawal != nil && s.Withdrawal.RequestID == requestID
	if s.CurrentOperationID != nil || pending || reused ||
		strings.TrimSpace(requestID) == "" || strings.TrimSpace(destinationAccount) == "" || amountRaw == 0 {
		return false, errors.New("route goal change is invalid while another action is active")
	}
	s.Generation++
	s.Goal = GoalWithdraw
	readyBy := requestedAt.Add(10 * time.Minute)
	s.Withdrawal = &Withdrawal{
		RequestID:          requestID,
		DestinationAccount: destinationAccount,
		AmountRaw:          amountRaw,
		Status:             WithdrawalRequested,
		RequestedAt:        requestedAt,
		ReadyBy:            readyBy,
	}
	return true, nil
}

// CancelWithdrawal mirrors Rust cancel_withdrawal: re-enter Deploy with the
// planner converging without an extra mutation when unwind never started.
func (s *RouteState) CancelWithdrawal(requestID string) error {
	canCancel := s.CurrentOperationID == nil && s.Withdrawal != nil &&
		s.Withdrawal.RequestID == requestID &&
		(s.Withdrawal.Status == WithdrawalRequested || s.Withdrawal.Status == WithdrawalClaimable)
	if !canCancel {
		return errors.New("route goal change is invalid while another action is active")
	}
	s.Generation++
	s.Goal = GoalDeploy
	s.Withdrawal = nil
	return nil
}

// TokenDelta is one expected custody movement.
type TokenDelta struct {
	Account  string `json:"account"`
	Mint     string `json:"mint"`
	RawDelta int64  `json:"rawDelta"`
}

// ObligationDelta is one expected KLend obligation movement.
type ObligationDelta struct {
	Obligation         string `json:"obligation"`
	CollateralRawDelta int64  `json:"collateralRawDelta"`
	DebtRawDelta       int64  `json:"debtRawDelta"`
}

// TokenAmountBefore is the exact pre-transaction custody amount.
type TokenAmountBefore struct {
	Account   string `json:"account"`
	Mint      string `json:"mint"`
	AmountRaw uint64 `json:"amountRaw"`
}

// ObligationBefore is the exact pre-transaction obligation state.
type ObligationBefore struct {
	Obligation    string `json:"obligation"`
	CollateralRaw uint64 `json:"collateralRaw"`
	DebtRaw       uint64 `json:"debtRaw"`
	DebtAmountSF  string `json:"debtAmountSf"`
}

// ExpectedEffects is the reconciliation contract persisted with the operation.
type ExpectedEffects struct {
	// Rust serializes the empty vec as [] (serde(default) is decode-only), so
	// this field must never be omitted from the persisted JSONB.
	TokenAmountsBefore []TokenAmountBefore `json:"tokenAmountsBefore"`
	TokenDeltas        []TokenDelta        `json:"tokenDeltas"`
	ObligationBefore   *ObligationBefore   `json:"obligationBefore,omitempty"`
	ObligationDelta    *ObligationDelta    `json:"obligationDelta,omitempty"`
}

// MarshalJSON keeps the Rust serde contract: tokenAmountsBefore is always a
// JSON array ([] when empty), never null.
func (e ExpectedEffects) MarshalJSON() ([]byte, error) {
	type expectedEffectsAlias ExpectedEffects
	if e.TokenAmountsBefore == nil {
		e.TokenAmountsBefore = []TokenAmountBefore{}
	}
	if e.TokenDeltas == nil {
		e.TokenDeltas = []TokenDelta{}
	}
	return jsonMarshal(expectedEffectsAlias(e))
}

// MultiplyOperation is one durable transaction attempt.
type MultiplyOperation struct {
	OperationID            string          `json:"operationId"`
	RouteKey               string          `json:"routeKey"`
	Cycle                  uint64          `json:"-"`
	EngineVersion          string          `json:"engineVersion"`
	Action                 MultiplyAction  `json:"action"`
	StrategyKey            StrategyKey     `json:"-"`
	Status                 OperationStatus `json:"-"`
	IDempotencyKey         string          `json:"-"`
	ExpectedEffects        ExpectedEffects `json:"-"`
	PolicyAccount          *string         `json:"-"`
	PolicyDataSHA256       *string         `json:"-"`
	MessageSHA256          *string         `json:"-"`
	SignedWire             []byte          `json:"-"`
	SignedWireSHA256       *string         `json:"-"`
	TransactionSignature   *string         `json:"-"`
	SourceInstructionIndex *uint16         `json:"-"`
	RecentBlockhash        *string         `json:"-"`
	LastValidBlockHeight   *uint64         `json:"-"`
	BroadcastIntentAt      *time.Time      `json:"-"`
	ConfirmedSlot          *uint64         `json:"-"`
	ReconciliationSHA256   *string         `json:"-"`
	CreatedAt              time.Time       `json:"-"`
	UpdatedAt              time.Time       `json:"-"`
}

// Validate mirrors MultiplyOperation::validate.
func (o *MultiplyOperation) Validate() error {
	if strings.TrimSpace(o.OperationID) == "" || strings.TrimSpace(o.RouteKey) == "" ||
		o.Cycle == 0 || o.EngineVersion != EngineVersion ||
		strings.TrimSpace(o.IDempotencyKey) == "" {
		return errors.New("invalid operation identity")
	}
	for _, delta := range o.ExpectedEffects.TokenDeltas {
		if strings.TrimSpace(delta.Account) == "" || strings.TrimSpace(delta.Mint) == "" {
			return errors.New("invalid operation expected effects")
		}
	}
	return nil
}

// SignedOperation is the exact signed wire and its identity, persisted before
// any broadcast intent. The wire itself is immutable once accepted.
type SignedOperation struct {
	Wire                 []byte
	WireSHA256           string
	TransactionSignature string
	RecentBlockhash      string
	LastValidBlockHeight int64
}

// NewSignedOperation mirrors SignedOperation::new including BIGINT checks.
func NewSignedOperation(wire []byte, transactionSignature, recentBlockhash string, lastValidBlockHeight uint64) (*SignedOperation, error) {
	if len(wire) == 0 || strings.TrimSpace(transactionSignature) == "" ||
		strings.TrimSpace(recentBlockhash) == "" || lastValidBlockHeight == 0 || lastValidBlockHeight > (1<<63-1) {
		return nil, errors.New("signed operation identity is incomplete")
	}
	digest := sha256.Sum256(wire)
	return &SignedOperation{
		Wire:                 append([]byte(nil), wire...),
		WireSHA256:           hex.EncodeToString(digest[:]),
		TransactionSignature: transactionSignature,
		RecentBlockhash:      recentBlockhash,
		LastValidBlockHeight: int64(lastValidBlockHeight),
	}, nil
}

// TickResult is the per-tick JSON evidence the Rust worker printed.
type TickResult struct {
	RouteKey    *string `json:"routeKey"`
	Condition   string  `json:"condition"`
	OperationID *string `json:"operationId"`
	Signature   *string `json:"signature"`
}

// RouteView is the status command projection.
type RouteView struct {
	RouteKey           string  `json:"routeKey"`
	Settings           string  `json:"settings"`
	VaultIndex         uint8   `json:"vaultIndex"`
	Vault              string  `json:"vault"`
	Generation         uint64  `json:"generation"`
	Cycle              uint64  `json:"cycle"`
	Goal               string  `json:"goal"`
	CurrentOperationID *string `json:"currentOperationId"`
}

// RouteViewOf mirrors multiply::view::route_view.
func RouteViewOf(state *RouteState) RouteView {
	current := (*string)(nil)
	if state.CurrentOperationID != nil {
		value := *state.CurrentOperationID
		current = &value
	}
	goal := string(state.Goal)
	switch state.Goal {
	case GoalIdle, GoalDeploy, GoalWithdraw, GoalClaimed, GoalManualRecovery:
	default:
		goal = "invalid"
	}
	return RouteView{
		RouteKey:           state.RouteKey,
		Settings:           state.Settings,
		VaultIndex:         state.VaultIndex,
		Vault:              state.Vault,
		Generation:         state.Generation,
		Cycle:              state.Cycle,
		Goal:               goal,
		CurrentOperationID: current,
	}
}
