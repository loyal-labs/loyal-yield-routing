package webapp

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
)

func strconvFormatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

type stateYieldPositionRow struct {
	ID                      int64
	Settings                string
	VaultIndex              int16
	VaultPubkey             string
	InitialReserve          string
	InitialMarket           *string
	InitialLiquidityMint    string
	InitialSupplyApyBps     *int64
	PrincipalAmountRaw      int64
	CurrentReserve          string
	CurrentMarket           *string
	CurrentLiquidityMint    string
	CurrentAmountRaw        int64
	CurrentObservedSlot     int64
	CurrentObservedAt       time.Time
	LastHoldingEventID      *int64
	LastRebalanceDecisionID *int64
	Status                  string
}

type stateHoldingEventRow struct {
	ID                        int64
	PositionID                int64
	EventType                 string
	Reserve                   string
	Market                    *string
	LiquidityMint             string
	AmountRaw                 int64
	ObservedSlot              int64
	ObservedAt                time.Time
	SourceRebalanceDecisionID *int64
	SourceSnapshotID          *int64
}

type stateOnboardingAttemptRow struct {
	PolicyID                 int64
	PolicyAccount            string
	PolicySeed               int64
	RoutePolicySignature     *string
	RoutePolicyConfirmedSlot *int64
	SetupPolicyID            *int64
	SetupPolicyAccount       *string
	SetupPolicySeed          *int64
	SetupPolicySignature     *string
	SetupPolicyConfirmedSlot *int64
	DepositSignature         *string
	DepositConfirmedSlot     *int64
	Status                   string
	LastErrorCode            *string
	UpdatedAt                time.Time
}

type stateAutodepositTargetRow struct {
	ID                                 int64
	BalanceSweepPolicyID               *int64
	Settings                           string
	PolicySeed                         int64
	PolicyAccount                      string
	VaultIndex                         int16
	VaultPubkey                        string
	Wallet                             string
	WalletUsdcAta                      string
	VaultUsdcAta                       string
	DelegatedSigners                   []string
	Cluster                            *string
	MaxAmountPerPeriod                 int64
	Active                             bool
	FirstSeenAt                        time.Time
	LastSeenSlot                       int64
	LastSeenSignature                  string
	SubscriptionAuthority              *string
	RecurringDelegation                *string
	RecurringDelegationNonce           *int64
	RecurringDelegationExpiryTimestamp *int64
	PeriodLengthSeconds                *int64
	StartTimestamp                     *int64
	WalletBalanceFloorRaw              *int64
	PolicySignature                    *string
	PolicyConfirmedSlot                *int64
	RecurringDelegationSignature       *string
	RecurringDelegationConfirmedSlot   *int64
	ChainStatus                        string
}

type stateBalanceSweepPolicyRow struct {
	ID                    int64
	PolicySeed            int64
	PolicyAccount         string
	DelegatedSigners      []string
	SubscriptionAuthority *string
	SubscriptionDelegatee *string
}

type statePendingSweepRow struct {
	Classification        string
	Confidence            string
	EligibleAfter         time.Time
	ExecuteNowAvailableAt *time.Time
	ID                    int64
	LotCount              int64
	OriginalAmountRaw     int64
	Reason                string
	RemainingAmountRaw    int64
	SlotID                int64
	Status                string
}

type stateCrossMintPolicyRow struct {
	Active                     bool
	DailySourceMintSpendingCap *int64
	DelegatedSigner            *string
	LastMutation               string
	LastSeenSignature          string
	LastSeenSlot               int64
	MaxSlippageBps             *int32
	PolicyAccount              string
	PolicySeed                 *int64
	SourceCommitment           string
	SourceShard                *string
	StartEligible              bool
}

type stateCrossMintOptInRow struct {
	Enabled    bool
	Generation int64
}

type stateVaultReserveRow struct {
	Reserve          string
	Market           *string
	LiquidityMint    string
	AmountRaw        int64
	SupplyApyBps     *int64
	SnapshotID       int64
	ObservedSlot     int64
	ObservedAt       time.Time
	PlanningMetadata map[string]any
}

type stateVaultIdleRow struct {
	Mint             string
	AmountRaw        int64
	Owner            string
	TokenAccount     string
	SourceCommitment string
	ObservedSlot     int64
	ObservedAt       time.Time
}

type stateRebalanceDecisionRow struct {
	ID            int64
	Signature     *string
	ConfirmedSlot *int64
}

func stateAutodepositStatus(active bool, chainStatus string) string {
	switch chainStatus {
	case "active":
		if active {
			return "active"
		}
		return "paused"
	case "paused_missing_position":
		return "paused"
	}
	return "pending"
}

func stateAutodepositPeriodStart(startTimestamp, periodLengthSeconds *int64, now time.Time) *time.Time {
	if startTimestamp == nil {
		return nil
	}
	startMs := *startTimestamp * 1000
	if periodLengthSeconds == nil || *periodLengthSeconds <= 0 {
		start := time.UnixMilli(startMs)
		return &start
	}
	nowMs := now.UnixMilli()
	if nowMs < startMs {
		start := time.UnixMilli(startMs)
		return &start
	}
	periodMs := *periodLengthSeconds * 1000
	elapsed := (nowMs - startMs) / periodMs
	start := time.UnixMilli(startMs + elapsed*periodMs)
	return &start
}

func stateAutodepositDelegationReadyAt(startTimestamp *int64) *time.Time {
	if startTimestamp == nil {
		return nil
	}
	ready := time.UnixMilli(*startTimestamp * 1000)
	return &ready
}

func stateDisplayableSweeps(status string, sweeps []statePendingSweepRow) []statePendingSweepRow {
	if status != "active" {
		return []statePendingSweepRow{}
	}
	executing := false
	for _, sweep := range sweeps {
		if sweep.Status == "requested" || sweep.Status == "selected" {
			executing = true
			break
		}
	}
	if !executing {
		return sweeps
	}
	filtered := make([]statePendingSweepRow, 0, len(sweeps))
	for _, sweep := range sweeps {
		if sweep.Status != "scheduled" {
			filtered = append(filtered, sweep)
		}
	}
	return filtered
}

func stateMetadataStringValue(metadata map[string]any, keys ...string) (string, bool) {
	if metadata == nil {
		return "", false
	}
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && len(value) > 0 {
			return value, true
		}
	}
	return "", false
}

func stateUserFacingReserveMetadata(metadata map[string]any) bool {
	semantics, ok := stateMetadataStringValue(metadata, "amountSemantics", "amount_semantics")
	return ok && semantics == "kamino_redeemable_liquidity"
}

func stateCollateralUnitMetadata(metadata map[string]any) bool {
	semantics, ok := stateMetadataStringValue(metadata, "amountSemantics", "amount_semantics")
	return ok && semantics == "kamino_obligation_collateral_deposited_amount"
}

func stateTruthy(s *string) bool {
	return s != nil && *s != ""
}

func stateOnboardingNextStep(attempt *stateOnboardingAttemptRow, hasActivePosition, hasRoutePolicy, hasSetupPolicy bool) string {
	if hasActivePosition {
		return "complete"
	}
	if attempt != nil {
		if attempt.Status == "accounting_failed" ||
			(attempt.Status == "deposit_confirmed" && stateTruthy(attempt.DepositSignature)) {
			return "deposit_accounting_retry"
		}
	}
	if hasRoutePolicy && hasSetupPolicy {
		return "deposit"
	}
	if attempt != nil && stateTruthy(attempt.SetupPolicySignature) {
		return "deposit"
	}
	if hasRoutePolicy || (attempt != nil && stateTruthy(attempt.RoutePolicySignature)) {
		return "setup_policy"
	}
	return "route_policy"
}

func stateInt64Ptr(v *int64) *string {
	if v == nil {
		return nil
	}
	s := strconvFormatInt(*v)
	return &s
}

func stateStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	s := *v
	return &s
}

func serializeEarnStatePosition(position stateYieldPositionRow, currentTotalAmountRaw int64) EarnStatePosition {
	total := strconvFormatInt(currentTotalAmountRaw)
	return EarnStatePosition{
		CurrentHolding: EarnStateCurrentHolding{
			AmountRaw:     strconvFormatInt(position.CurrentAmountRaw),
			LiquidityMint: position.CurrentLiquidityMint,
			Market:        stateStringPtr(position.CurrentMarket),
			ObservedAt:    isoMilliseconds(position.CurrentObservedAt),
			ObservedSlot:  strconvFormatInt(position.CurrentObservedSlot),
			Provenance: EarnStateProvenance{
				LastHoldingEventID:      stateInt64Ptr(position.LastHoldingEventID),
				LastRebalanceDecisionID: stateInt64Ptr(position.LastRebalanceDecisionID),
			},
			Reserve: position.CurrentReserve,
		},
		ID: strconvFormatInt(position.ID),
		InitialHolding: EarnStateInitialHolding{
			LiquidityMint: position.InitialLiquidityMint,
			Market:        stateStringPtr(position.InitialMarket),
			Reserve:       position.InitialReserve,
			SupplyApyBps:  stateInt64Ptr(position.InitialSupplyApyBps),
		},
		CurrentTotalAmountRaw:        total,
		CurrentTotalNominalUsdMicros: total,
		PrincipalAmountRaw:           strconvFormatInt(position.PrincipalAmountRaw),
		Status:                       position.Status,
	}
}

// serializeEarnStatePolicy mirrors serializeRoutePolicyState. The route
// policies are looked up through h.activePolicyPair, which does not scan
// vault_index/vault_pubkey, so they arrive as arguments; the pair lookup pins
// both to the Earn vault, so they equal the row values.
func serializeEarnStatePolicy(route RoutePolicy, setup *RoutePolicy, vaultIndex int, vaultPubkey string) *EarnStatePolicy {
	serialized := &EarnStatePolicy{
		Account:              route.PolicyAccount,
		DelegatedSigners:     route.DelegatedSigners,
		ID:                   strconvFormatInt(route.ID),
		KaminoLiquidityMints: route.KaminoLiquidityMints,
		KaminoMarkets:        route.KaminoMarkets,
		LastSeenSignature:    route.LastSeenSignature,
		LastSeenSlot:         strconvFormatInt(route.LastSeenSlot),
		RiskProfile:          stateStringPtr(route.RiskProfile),
		RouteModes:           route.RouteModes,
		Seed:                 strconvFormatInt(route.PolicySeed),
		StableMints:          route.StableMints,
		UniversePreset:       stateStringPtr(route.UniversePreset),
		VaultIndex:           vaultIndex,
		VaultPubkey:          vaultPubkey,
	}
	if setup != nil {
		serialized.SetupPolicy = &EarnStateSetupPolicy{
			Account:           setup.PolicyAccount,
			DelegatedSigners:  setup.DelegatedSigners,
			ID:                strconvFormatInt(setup.ID),
			LastSeenSignature: setup.LastSeenSignature,
			LastSeenSlot:      strconvFormatInt(setup.LastSeenSlot),
			Seed:              strconvFormatInt(setup.PolicySeed),
		}
	}
	return serialized
}

func serializeEarnStateOnboarding(attempt *stateOnboardingAttemptRow, nextStep string) EarnStateOnboarding {
	out := EarnStateOnboarding{NextStep: nextStep}
	if attempt == nil {
		return out
	}
	out.DepositConfirmedSlot = stateInt64Ptr(attempt.DepositConfirmedSlot)
	out.DepositSignature = stateStringPtr(attempt.DepositSignature)
	out.LastErrorCode = stateStringPtr(attempt.LastErrorCode)
	out.Policy = &EarnStateOnboardingPolicy{
		Account:           attempt.PolicyAccount,
		ID:                strconvFormatInt(attempt.PolicyID),
		LastSeenSignature: stateStringPtr(attempt.RoutePolicySignature),
		LastSeenSlot:      stateInt64Ptr(attempt.RoutePolicyConfirmedSlot),
		Seed:              strconvFormatInt(attempt.PolicySeed),
	}
	if attempt.SetupPolicyAccount != nil && *attempt.SetupPolicyAccount != "" && attempt.SetupPolicySeed != nil {
		seed := attempt.SetupPolicyID
		if seed == nil {
			seed = attempt.SetupPolicySeed
		}
		out.SetupPolicy = &EarnStateOnboardingPolicy{
			Account:           *attempt.SetupPolicyAccount,
			ID:                strconvFormatInt(*seed),
			LastSeenSignature: stateStringPtr(attempt.SetupPolicySignature),
			LastSeenSlot:      stateInt64Ptr(attempt.SetupPolicyConfirmedSlot),
			Seed:              strconvFormatInt(*attempt.SetupPolicySeed),
		}
	}
	out.Status = stateStringPtr(&attempt.Status)
	updatedAt := isoMilliseconds(attempt.UpdatedAt)
	out.UpdatedAt = &updatedAt
	return out
}

func serializeEarnStateScheduledSweeps(status string, sweeps []statePendingSweepRow) []EarnStateScheduledSweep {
	visible := stateDisplayableSweeps(status, sweeps)
	out := make([]EarnStateScheduledSweep, 0, len(visible))
	for _, sweep := range visible {
		out = append(out, EarnStateScheduledSweep{
			Classification:        sweep.Classification,
			Confidence:            sweep.Confidence,
			EligibleAfter:         isoMilliseconds(sweep.EligibleAfter),
			ExecuteNowAvailableAt: stateTimePtr(sweep.ExecuteNowAvailableAt),
			ID:                    strconvFormatInt(sweep.ID),
			LotCount:              int(sweep.LotCount),
			OriginalAmountRaw:     strconvFormatInt(sweep.OriginalAmountRaw),
			Reason:                sweep.Reason,
			RemainingAmountRaw:    strconvFormatInt(sweep.RemainingAmountRaw),
			SlotID:                strconvFormatInt(sweep.SlotID),
			Status:                sweep.Status,
		})
	}
	return out
}

func serializeEarnStateAutodeposit(target stateAutodepositTargetRow, policy *stateBalanceSweepPolicyRow, depositedThisPeriodRaw int64, sweeps []statePendingSweepRow) EarnStateAutodeposit {
	status := stateAutodepositStatus(target.Active, target.ChainStatus)

	delegatedSigner := stateFirstSigner(target.DelegatedSigners)
	if delegatedSigner == nil && policy != nil {
		delegatedSigner = stateFirstSigner(policy.DelegatedSigners)
	}
	balanceSweepPolicyID := stateInt64Ptr(target.BalanceSweepPolicyID)
	if balanceSweepPolicyID == nil && policy != nil {
		id := strconvFormatInt(policy.ID)
		balanceSweepPolicyID = &id
	}
	policyAccount := target.PolicyAccount
	if policy != nil {
		policyAccount = policy.PolicyAccount
	}
	policySeed := target.PolicySeed
	if policy != nil {
		policySeed = policy.PolicySeed
	}
	subscriptionAuthority := stateStringPtr(target.SubscriptionAuthority)
	if subscriptionAuthority == nil && policy != nil {
		subscriptionAuthority = stateStringPtr(policy.SubscriptionAuthority)
	}
	subscriptionDelegatee := target.VaultPubkey
	if policy != nil && policy.SubscriptionDelegatee != nil {
		subscriptionDelegatee = *policy.SubscriptionDelegatee
	}
	startTimestamp := stateInt64Ptr(target.StartTimestamp)
	if startTimestamp == nil {
		fallback := strconvFormatInt(target.FirstSeenAt.Unix())
		startTimestamp = &fallback
	}

	return EarnStateAutodeposit{
		Active:                           target.Active,
		AmountPerPeriodRaw:               strconvFormatInt(target.MaxAmountPerPeriod),
		BalanceSweepPolicyID:             balanceSweepPolicyID,
		Cluster:                          stateStringPtr(target.Cluster),
		DelegatedSigner:                  delegatedSigner,
		DepositedThisPeriodRaw:           strconvFormatInt(depositedThisPeriodRaw),
		ExpiryTimestamp:                  stateInt64Ptr(target.RecurringDelegationExpiryTimestamp),
		LastSeenSignature:                target.LastSeenSignature,
		LastSeenSlot:                     strconvFormatInt(target.LastSeenSlot),
		LifecycleStatus:                  target.ChainStatus,
		Nonce:                            stateInt64Ptr(target.RecurringDelegationNonce),
		PeriodLengthSeconds:              stateInt64Ptr(target.PeriodLengthSeconds),
		PolicyAccount:                    policyAccount,
		PolicyConfirmedSlot:              stateInt64Ptr(target.PolicyConfirmedSlot),
		PolicySeed:                       strconvFormatInt(policySeed),
		PolicySignature:                  stateStringPtr(target.PolicySignature),
		RecurringDelegation:              stateStringPtr(target.RecurringDelegation),
		RecurringDelegationConfirmedSlot: stateInt64Ptr(target.RecurringDelegationConfirmedSlot),
		RecurringDelegationSignature:     stateStringPtr(target.RecurringDelegationSignature),
		ScheduledSweeps:                  serializeEarnStateScheduledSweeps(status, sweeps),
		StartTimestamp:                   *startTimestamp,
		Status:                           status,
		SubscriptionAuthority:            subscriptionAuthority,
		SubscriptionDelegatee:            subscriptionDelegatee,
		VaultUsdcAta:                     target.VaultUsdcAta,
		WalletBalanceFloorRaw:            stateInt64Ptr(target.WalletBalanceFloorRaw),
		WalletUsdcAta:                    target.WalletUsdcAta,
	}
}

func stateFirstSigner(signers []string) *string {
	if len(signers) == 0 {
		return nil
	}
	first := signers[0]
	return &first
}

func stateTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := isoMilliseconds(*t)
	return &s
}

func deriveEarnCrossMintIndex(rows []stateCrossMintPolicyRow) EarnStateAutoswapIndex {
	empty := func(state string) EarnStateAutoswapIndex {
		return EarnStateAutoswapIndex{Policies: []EarnStateAutoswapIndexPolicy{}, State: state}
	}
	var active []stateCrossMintPolicyRow
	for _, row := range rows {
		if row.Active && (row.LastMutation == "create" || row.LastMutation == "update") {
			active = append(active, row)
		}
	}
	if len(active) == 0 {
		return empty("empty")
	}
	candidates := make([]EarnStateAutoswapIndexPolicy, 0, len(active))
	for _, row := range active {
		if !row.StartEligible ||
			!stateIsPolicyShard(row.SourceShard) ||
			!stateIsSupportedCommitment(row.SourceCommitment) ||
			row.PolicySeed == nil ||
			row.MaxSlippageBps == nil ||
			row.DailySourceMintSpendingCap == nil ||
			!stateTruthy(row.DelegatedSigner) {
			return empty("ambiguous")
		}
		candidates = append(candidates, EarnStateAutoswapIndexPolicy{
			Account:                    row.PolicyAccount,
			DailySourceMintSpendingCap: strconvFormatInt(*row.DailySourceMintSpendingCap),
			DelegatedSigner:            *row.DelegatedSigner,
			LastSeenSignature:          row.LastSeenSignature,
			LastSeenSlot:               strconvFormatInt(row.LastSeenSlot),
			MaxSlippageBps:             int(*row.MaxSlippageBps),
			Seed:                       strconvFormatInt(*row.PolicySeed),
			SourceCommitment:           row.SourceCommitment,
			SourceShard:                *row.SourceShard,
		})
	}
	accounts := map[string]bool{}
	seeds := map[string]bool{}
	shards := map[string]bool{}
	parametersMatch := true
	for i, candidate := range candidates {
		accounts[candidate.Account] = true
		seeds[candidate.Seed] = true
		shards[candidate.SourceShard] = true
		if i > 0 {
			first := candidates[0]
			parametersMatch = parametersMatch &&
				candidate.DelegatedSigner == first.DelegatedSigner &&
				candidate.MaxSlippageBps == first.MaxSlippageBps &&
				candidate.DailySourceMintSpendingCap == first.DailySourceMintSpendingCap
		}
	}
	if len(candidates) > 2 ||
		len(accounts) != len(candidates) ||
		len(seeds) != len(candidates) ||
		len(shards) != len(candidates) ||
		!parametersMatch {
		return empty("ambiguous")
	}
	// classic sorts before token_2022; shards are distinct here.
	sortStateCandidates(candidates)
	state := "partial"
	if len(candidates) == 2 {
		state = "complete"
	}
	return EarnStateAutoswapIndex{Policies: candidates, State: state}
}

func sortStateCandidates(candidates []EarnStateAutoswapIndexPolicy) {
	slices.SortStableFunc(candidates, func(a, b EarnStateAutoswapIndexPolicy) int {
		if (a.SourceShard == "classic") == (b.SourceShard == "classic") {
			return 0
		}
		if a.SourceShard == "classic" {
			return -1
		}
		return 1
	})
}

func stateIsPolicyShard(shard *string) bool {
	return shard != nil && (*shard == "classic" || *shard == "token_2022")
}

func stateIsSupportedCommitment(commitment string) bool {
	return commitment == "confirmed" || commitment == "finalized"
}

func buildEarnStateAutoswap(optIn *stateCrossMintOptInRow, index EarnStateAutoswapIndex) *EarnStateAutoswap {
	if optIn == nil || index.State != "complete" {
		return nil
	}
	var classic, token2022 *EarnStateAutoswapIndexPolicy
	for i := range index.Policies {
		switch index.Policies[i].SourceShard {
		case "classic":
			classic = &index.Policies[i]
		case "token_2022":
			token2022 = &index.Policies[i]
		}
	}
	if classic == nil || token2022 == nil {
		return nil
	}
	pairIsReady := stateIsSupportedCommitment(classic.SourceCommitment) &&
		stateIsSupportedCommitment(token2022.SourceCommitment)
	bound := []EarnStateBoundPolicy{
		{Account: classic.Account, Seed: classic.Seed, SourceShard: classic.SourceShard},
		{Account: token2022.Account, Seed: token2022.Seed, SourceShard: token2022.SourceShard},
	}
	policies := []EarnStateAutoswapPolicy{}
	if pairIsReady {
		policies = []EarnStateAutoswapPolicy{
			{Account: classic.Account, Seed: classic.Seed, SourceShard: classic.SourceShard, LastSeenSignature: classic.LastSeenSignature, LastSeenSlot: classic.LastSeenSlot},
			{Account: token2022.Account, Seed: token2022.Seed, SourceShard: token2022.SourceShard, LastSeenSignature: token2022.LastSeenSignature, LastSeenSlot: token2022.LastSeenSlot},
		}
	}
	status := "finalizing"
	if pairIsReady {
		status = "on"
	}
	if !optIn.Enabled {
		status = "paused"
	}
	return &EarnStateAutoswap{
		BoundPolicies:              bound,
		DailySourceMintSpendingCap: classic.DailySourceMintSpendingCap,
		Enabled:                    optIn.Enabled,
		Generation:                 strconvFormatInt(optIn.Generation),
		MaxSlippageBps:             classic.MaxSlippageBps,
		Policies:                   policies,
		Status:                     status,
	}
}

func earnAutoswapEnrollmentEnabled(raw string, wallet string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	if raw == "*" {
		return true, nil
	}
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return false, errors.New("EARN_AUTOSWAP_ENABLED_WALLETS contains an empty wallet entry.")
		}
		key, err := solana.PublicKeyFromBase58(entry)
		if err != nil {
			return false, errors.New("EARN_AUTOSWAP_ENABLED_WALLETS contains an invalid wallet.")
		}
		canonical := key.String()
		if canonical != entry {
			return false, errors.New("EARN_AUTOSWAP_ENABLED_WALLETS contains a non-canonical wallet.")
		}
		if seen[canonical] {
			return false, errors.New("EARN_AUTOSWAP_ENABLED_WALLETS contains a duplicate wallet.")
		}
		seen[canonical] = true
	}
	return seen[wallet], nil
}
