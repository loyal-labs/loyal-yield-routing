package fleet

// This file owns the part of the Kamino route which used to be reconstructed
// by the Rust fleet worker: whole-fleet wave selection, fresh evidence fences,
// Squads policy decoding/wrapping, reusable ALT selection, and exact v0 bytes.
// It intentionally contains no signer or broadcast API.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	SquadsProgram       = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"
	SolanaPacketLimit   = 1232
	defaultComputeLimit = uint64(1_400_000)
)

// FleetVault is one migrated and currently unblocked policy/vault. Callers
// must load this complete set in one repeatable-read snapshot.
type FleetVault struct {
	// Nonempty only for read-only idle-balance diagnostics. Not executable.
	IdleTokenAccount         string
	Position                 VaultPosition
	AllowedTargets           []string
	CrossMintTargets         map[string]CrossMintPolicyBindings
	CrossMintMaxValueLossBPS uint16
	CommittedInflows         map[string]int64
	CommittedOutflows        map[string]int64
}

type PlannedOpportunity struct {
	Decision       Decision        `json:"decision"`
	ExecutionPlan  json.RawMessage `json:"executionPlan"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

type FleetPlan struct {
	Opportunities []PlannedOpportunity `json:"opportunities"`
	Rejections    map[int64]string     `json:"rejections"`
}

// PlanFleet evaluates all vault/target pairs, orders by Rust's durable
// economic priority tie-breaks, and admits a wave while carrying forward the
// selected inflow/outflow. No reserve can exceed its 2% frontier.
func PlanFleet(snapshot MarketSnapshot, vaults []FleetVault) (FleetPlan, error) {
	for _, vault := range vaults {
		if vault.IdleTokenAccount != "" {
			return FleetPlan{}, errors.New("idle shadow sources cannot enter executable fleet planning")
		}
	}
	if len(snapshot.Reserves) < 2 {
		return FleetPlan{}, errors.New("complete reserve frontier is required")
	}
	if len(vaults) == 0 {
		return FleetPlan{Opportunities: []PlannedOpportunity{}, Rejections: map[int64]string{}}, nil
	}
	type candidate struct {
		vault  FleetVault
		target string
		d      Decision
	}
	seen := map[int64]bool{}
	baseInflow, baseOutflow := map[string]int64{}, map[string]int64{}
	for _, vault := range vaults {
		for reserve, value := range vault.CommittedInflows {
			if old, ok := baseInflow[reserve]; ok && old != value {
				return FleetPlan{}, errors.New("incoherent committed inflow frontier")
			}
			baseInflow[reserve] = value
		}
		for reserve, value := range vault.CommittedOutflows {
			if old, ok := baseOutflow[reserve]; ok && old != value {
				return FleetPlan{}, errors.New("incoherent committed outflow frontier")
			}
			baseOutflow[reserve] = value
		}
	}
	var candidates []candidate
	out := FleetPlan{Rejections: map[int64]string{}}
	reserveKeys := make([]string, 0, len(snapshot.Reserves))
	for key := range snapshot.Reserves {
		reserveKeys = append(reserveKeys, key)
	}
	sort.Strings(reserveKeys)
	for _, vault := range vaults {
		if vault.Position.VaultID <= 0 || seen[vault.Position.VaultID] {
			return FleetPlan{}, errors.New("fleet contains duplicate or invalid vault")
		}
		seen[vault.Position.VaultID] = true
		allowed := map[string]bool{}
		for _, target := range vault.AllowedTargets {
			allowed[target] = true
		}
		for target := range vault.CrossMintTargets {
			allowed[target] = true
		}
		best := candidate{}
		for _, target := range reserveKeys {
			if target == vault.Position.SourceReserve || !allowed[target] {
				continue
			}
			position := vault.Position
			position.TargetCommittedInflowUSDMicros, position.TargetCommittedOutflowUSDMicros = baseInflow[target], baseOutflow[target]
			position.SourceCommittedInflowUSDMicros, position.SourceCommittedOutflowUSDMicros = baseInflow[position.SourceReserve], baseOutflow[position.SourceReserve]
			d := Plan(snapshot, position, position.SourceReserve, target)
			if binding, cross := vault.CrossMintTargets[target]; cross {
				d.RouteKind = "cross_mint_jupiter"
				d.SourceMint = position.Mint
				d.TargetMint = snapshot.Reserves[target].Mint
				d.Mint = d.TargetMint
				d.PolicyBindings = &binding
				d.CrossMintMaxValueLossBPS = vault.CrossMintMaxValueLossBPS
				if d.Eligible && d.EstimatedCostLamports < 15_000 {
					d.Eligible, d.Reason = false, "cross_mint_fee_envelope_not_covered"
				}
			}
			if d.Eligible && (!best.d.Eligible || betterDecision(d, best.d)) {
				best = candidate{vault, target, d}
			}
		}
		if best.d.Eligible {
			candidates = append(candidates, best)
		} else {
			out.Rejections[vault.Position.VaultID] = "no_eligible_target"
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].d.EconomicPriority != candidates[j].d.EconomicPriority {
			return candidates[i].d.EconomicPriority > candidates[j].d.EconomicPriority
		}
		if candidates[i].d.ExpectedNetGainUSDMicros != candidates[j].d.ExpectedNetGainUSDMicros {
			return candidates[i].d.ExpectedNetGainUSDMicros > candidates[j].d.ExpectedNetGainUSDMicros
		}
		return candidates[i].d.VaultID < candidates[j].d.VaultID
	})
	inflow, outflow := map[string]int64{}, map[string]int64{}
	for _, c := range candidates {
		position := c.vault.Position
		var ok bool
		position.TargetCommittedInflowUSDMicros, ok = sumInt64(baseInflow[c.target], inflow[c.target])
		if !ok {
			return FleetPlan{}, errors.New("wave capacity overflow")
		}
		position.TargetCommittedOutflowUSDMicros, ok = sumInt64(baseOutflow[c.target], outflow[c.target])
		if !ok {
			return FleetPlan{}, errors.New("wave capacity overflow")
		}
		position.SourceCommittedInflowUSDMicros, ok = sumInt64(baseInflow[position.SourceReserve], inflow[position.SourceReserve])
		if !ok {
			return FleetPlan{}, errors.New("wave capacity overflow")
		}
		position.SourceCommittedOutflowUSDMicros, ok = sumInt64(baseOutflow[position.SourceReserve], outflow[position.SourceReserve])
		if !ok {
			return FleetPlan{}, errors.New("wave capacity overflow")
		}
		d := Plan(snapshot, position, position.SourceReserve, c.target)
		if binding, cross := c.vault.CrossMintTargets[c.target]; cross {
			d.RouteKind = "cross_mint_jupiter"
			d.SourceMint = position.Mint
			d.TargetMint = snapshot.Reserves[c.target].Mint
			d.Mint = d.TargetMint
			d.PolicyBindings = &binding
			d.CrossMintMaxValueLossBPS = c.vault.CrossMintMaxValueLossBPS
			if d.Eligible && d.EstimatedCostLamports < 15_000 {
				d.Eligible, d.Reason = false, "cross_mint_fee_envelope_not_covered"
			}
		}
		if !d.Eligible {
			out.Rejections[position.VaultID] = d.Reason
			continue
		}
		inflow[c.target], ok = sumInt64(inflow[c.target], d.PrincipalUSDMicros)
		if !ok {
			return FleetPlan{}, errors.New("wave inflow overflow")
		}
		outflow[d.SourceReserve], ok = sumInt64(outflow[d.SourceReserve], d.PrincipalUSDMicros)
		if !ok {
			return FleetPlan{}, errors.New("wave outflow overflow")
		}
		plan, err := canonicalExecutionPlan(snapshot, c.vault, d)
		if err != nil {
			return FleetPlan{}, err
		}
		expires := snapshot.ExpiresAt
		if mintExpiry, ok := snapshot.MintExpiresAt[d.SourceMint]; ok {
			expires = mintExpiry
		}
		if mintExpiry, ok := snapshot.MintExpiresAt[d.TargetMint]; ok && (expires.IsZero() || mintExpiry.Before(expires)) {
			expires = mintExpiry
		}
		if expires.IsZero() {
			expires = snapshot.ObservedAt.Add(5 * time.Minute)
		}
		epochID := snapshot.OptimizerEpochID
		if epochID <= 0 {
			epochID = snapshot.Slot
		}
		cluster := snapshot.Cluster
		if cluster == "" {
			cluster = "mainnet-beta"
		}
		key := opportunityIdentity(cluster, epochID, d, plan, expires)
		out.Opportunities = append(out.Opportunities, PlannedOpportunity{d, plan, key})
	}
	return out, nil
}
func betterDecision(a, b Decision) bool {
	if a.EconomicPriority != b.EconomicPriority {
		return a.EconomicPriority > b.EconomicPriority
	}
	if a.ExpectedNetGainUSDMicros != b.ExpectedNetGainUSDMicros {
		return a.ExpectedNetGainUSDMicros > b.ExpectedNetGainUSDMicros
	}
	return a.TargetReserve < b.TargetReserve
}

func canonicalExecutionPlan(snapshot MarketSnapshot, v FleetVault, d Decision) (json.RawMessage, error) {
	source := snapshot.Reserves[d.SourceReserve]
	target := snapshot.Reserves[d.TargetReserve]
	targetObservedAt := target.ObservedAt
	if targetObservedAt.IsZero() {
		targetObservedAt = snapshot.ObservedAt
	}
	if d.RouteKind == "cross_mint_jupiter" {
		return canonicalCrossMintExecutionPlan(v.Position, d, source.SupplyAPYBPS, target.SupplyAPYBPS, target.Slot, targetObservedAt)
	}
	return canonicalSameMintExecutionPlan(v.Position, d, source.SupplyAPYBPS, target.SupplyAPYBPS, target.Slot, targetObservedAt)
}

// canonicalSameMintExecutionPlan is shared by whole-fleet planning and durable
// publication so rediscovery hashes the exact same Rust-compatible JSON.
func canonicalSameMintExecutionPlan(position VaultPosition, decision Decision, observedSourceAPY, observedTargetAPY, targetSlot int64, targetObservedAt time.Time) (json.RawMessage, error) {
	feeTier := "base"
	if decision.EstimatedCostLamports >= 50_000 {
		feeTier = "high_value"
	} else if decision.EstimatedCostLamports >= 15_000 {
		feeTier = "standard"
	}
	plan := map[string]any{
		"kind": "same_mint", "route_kind": "same_mint", "settings": position.Settings, "vault_index": position.VaultIndex,
		"vault_pubkey": position.VaultPubkey, "policy_id": position.PolicyID, "source_kind": "reserve_position",
		"source_reserve": decision.SourceReserve, "target_reserve": decision.TargetReserve, "liquidity_mint": decision.Mint,
		"source_liquidity_mint": decision.Mint, "target_liquidity_mint": decision.Mint, "amount_raw": decision.AmountRaw,
		"route_amount_semantics": amountSemanticsRedeemableLiquidity, "source_amount_semantics": position.SourceAmountSemantics,
		"source_collateral_amount_raw": optionalPositiveInt64(position.SourceCollateralAmountRaw), "redeemable_source_liquidity_amount_raw": decision.AmountRaw,
		"idle_vault_liquidity_amount_raw": position.IdleVaultLiquidityAmountRaw, "idle_token_account": nil,
		"source_apy_bps": decision.SourceAPYBPS, "observed_source_apy_bps": observedSourceAPY,
		"observed_target_apy_bps": observedTargetAPY, "target_apy_bps": decision.TargetAPYBPS,
		"capacity_adjusted_target_apy_bps": decision.TargetAPYBPS, "estimated_edge_bps": decision.EdgeBPS,
		"confidence_ppm": decision.ConfidencePPM, "expected_service_millis": expectedServiceMillis,
		"holding_horizon_seconds": decision.HoldingHorizonSeconds, "estimated_execution_cost_usd_micros": decision.EstimatedCostUSDMicros,
		"estimated_execution_costs": map[string]any{"kind": "same_mint", "route_usd_micros": decision.EstimatedCostUSDMicros},
		"fee_cap_lamports":          decision.EstimatedCostLamports, "fee_tier": feeTier, "fee_gain_fraction_ppm": 50_000,
		"minimum_transaction_fee_lamports": 5_000, "conservative_sol_price_usd_micros": 1_000_000_000,
		"source_observed_at": position.ObservedAt.UTC(), "source_observed_slot": position.ObservedSlot,
		"optimizer_market_slot": decision.MarketSlot, "target_observed_at": targetObservedAt.UTC(), "target_observed_slot": targetSlot,
		"writable_conflict_keys":                  []string{"vault:" + position.VaultPubkey, "policy:" + fmt.Sprint(position.PolicyID), "source-reserve:" + decision.SourceReserve, "target-reserve:" + decision.TargetReserve},
		"planning_economics_are_executable_quote": false, "fresh_executable_jupiter_minimum_output_required": false, "policy_bindings": nil,
		"source_recovery_anchor_collateral_raw": nil, "cross_mint_maximum_value_loss_bps": nil,
	}
	return json.Marshal(plan)
}
func canonicalCrossMintExecutionPlan(position VaultPosition, decision Decision, observedSourceAPY, observedTargetAPY, targetSlot int64, targetObservedAt time.Time) (json.RawMessage, error) {
	if decision.PolicyBindings == nil || decision.SourceMint == decision.TargetMint || !isEarnStableMint(decision.SourceMint) || !isEarnStableMint(decision.TargetMint) {
		return nil, errors.New("cross-mint route requires distinct supported mints and exact policy bindings")
	}
	anchored, ok := crossMintRecoveryAnchoredPosition(position)
	if !ok || anchored.AmountRaw != decision.AmountRaw {
		return nil, errors.New("cross-mint route amounts do not preserve the source recovery anchor")
	}
	position = anchored
	feeTier := "base"
	if decision.EstimatedCostLamports >= 50_000 {
		feeTier = "high_value"
	} else if decision.EstimatedCostLamports >= 15_000 {
		feeTier = "standard"
	}
	bindings := decision.PolicyBindings
	conflicts := []string{"vault:" + position.VaultPubkey, "policy:" + fmt.Sprint(position.PolicyID), "source-reserve:" + decision.SourceReserve, "target-reserve:" + decision.TargetReserve, "swap-policy:" + bindings.Swap.PolicyAccount, "earn-policy:" + bindings.Withdraw.PolicyAccount}
	if bindings.Deposit.PolicyAccount != bindings.Withdraw.PolicyAccount {
		conflicts = append(conflicts, "earn-policy:"+bindings.Deposit.PolicyAccount)
	}
	plan := map[string]any{
		"kind": "cross_mint_jupiter", "route_kind": "cross_mint_jupiter", "settings": position.Settings, "vault_index": position.VaultIndex,
		"vault_pubkey": position.VaultPubkey, "policy_id": position.PolicyID, "source_kind": "reserve_position",
		"source_reserve": decision.SourceReserve, "target_reserve": decision.TargetReserve, "liquidity_mint": decision.SourceMint,
		"source_liquidity_mint": decision.SourceMint, "target_liquidity_mint": decision.TargetMint, "amount_raw": decision.AmountRaw,
		"route_amount_semantics": amountSemanticsRedeemableLiquidity, "source_amount_semantics": position.SourceAmountSemantics,
		"source_collateral_amount_raw": optionalPositiveInt64(position.SourceCollateralAmountRaw), "redeemable_source_liquidity_amount_raw": decision.AmountRaw,
		"idle_vault_liquidity_amount_raw": position.IdleVaultLiquidityAmountRaw, "idle_token_account": nil,
		"source_apy_bps": decision.SourceAPYBPS, "observed_source_apy_bps": observedSourceAPY,
		"observed_target_apy_bps": observedTargetAPY, "target_apy_bps": decision.TargetAPYBPS,
		"capacity_adjusted_target_apy_bps": decision.TargetAPYBPS, "estimated_edge_bps": decision.EdgeBPS,
		"confidence_ppm": decision.ConfidencePPM, "expected_service_millis": expectedServiceMillis * 3,
		"holding_horizon_seconds": decision.HoldingHorizonSeconds, "estimated_execution_cost_usd_micros": decision.EstimatedCostUSDMicros,
		"estimated_execution_costs": map[string]any{"kind": "cross_mint_jupiter", "withdraw_usd_micros": estimatedCostUSDMicros, "jupiter_swap_usd_micros": estimatedCostUSDMicros, "deposit_usd_micros": estimatedCostUSDMicros},
		"fee_cap_lamports":          decision.EstimatedCostLamports, "fee_tier": feeTier, "fee_gain_fraction_ppm": 50_000,
		"minimum_transaction_fee_lamports": 5_000, "conservative_sol_price_usd_micros": 1_000_000_000,
		"source_observed_at": position.ObservedAt.UTC(), "source_observed_slot": position.ObservedSlot,
		"optimizer_market_slot": decision.MarketSlot, "target_observed_at": targetObservedAt.UTC(), "target_observed_slot": targetSlot,
		"writable_conflict_keys": orderedUniqueStrings(conflicts), "planning_economics_are_executable_quote": false,
		"fresh_executable_jupiter_minimum_output_required": true, "policy_bindings": bindings,
		"source_recovery_anchor_collateral_raw": int64(1), "cross_mint_maximum_value_loss_bps": decision.CrossMintMaxValueLossBPS,
	}
	return json.Marshal(plan)
}

func optionalPositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func opportunityIdentity(cluster string, optimizerEpochID int64, d Decision, plan []byte, expires time.Time) string {
	values := []string{"loyal-rebalance-opportunity-v1", cluster, fmt.Sprint(d.VaultID), fmt.Sprint(d.SourceSnapshotID), fmt.Sprint(optimizerEpochID), "", "", d.SourceReserve, d.TargetReserve, d.Mint, fmt.Sprint(d.AmountRaw), fmt.Sprint(d.PrincipalUSDMicros), fmt.Sprint(d.SourceAPYBPS), fmt.Sprint(d.TargetAPYBPS), fmt.Sprint(d.EdgeBPS), fmt.Sprint(d.EstimatedCostLamports), fmt.Sprint(d.AnnualYieldGainUSDMicros), fmt.Sprint(d.ExpectedNetGainUSDMicros), fmt.Sprint(d.EconomicPriority), "lost-yield-service-net-reserve-capacity-v3", "yield_optimization", "", string(plan), rustRFC3339(expires), ""}
	h := sha256.New()
	var length [8]byte
	for _, s := range values {
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func rustRFC3339(value time.Time) string {
	value = value.UTC()
	base := value.Format("2006-01-02T15:04:05")
	if value.Nanosecond() != 0 {
		fraction := fmt.Sprintf(".%09d", value.Nanosecond())
		fraction = strings.TrimRight(fraction, "0")
		base += fraction
	}
	return base + "+00:00"
}

func orderedUniqueStrings(values []string) []string {
	set := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value != "" && !set[value] {
			set[value] = true
			out = append(out, value)
		}
	}
	return out
}

func canonicalStrings(values []string) []string {
	set := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if v != "" && !set[v] {
			set[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

type FreshAccount struct {
	Kind, Address, Owner, DataSHA256 string
	Slot                             int64
	Executable                       bool
	Exists                           bool
}
type FreshRouteEvidence struct {
	ObservedAt                    time.Time
	Slot                          int64
	ObservedSourceAPYBPS          int64
	ObservedTargetAPYBPS          int64
	TargetObservedSupplyUSDMicros int64
	OpportunityID                 int64
	OpportunityKey                string
	EpochID                       int64
	EpochFingerprint              string
	Accounts                      []FreshAccount
	PolicyData                    []byte
}
type DecodedSquadsPolicy struct {
	Settings              string
	PolicySeed            uint64
	Bump                  uint8
	TransactionIndex      uint64
	StaleTransactionIndex uint64
	TimeLock              uint32
	AccountIndex          uint8
	DelegatedSigners      []string
	SignerPermissions     []uint8
	InstructionPrograms   []string
	InstructionData       [][]byte
	AllowedIndexes        []uint8
	Constraints           []policyInstructionConstraint
}

type policyInstructionConstraint struct {
	Program  string
	Accounts []policyAccountConstraint
	Data     []policyDataConstraint
}
type policyAccountConstraint struct {
	Index   uint8
	Pubkeys []string
	Owner   string
}
type policyDataConstraint struct {
	Offset   uint64
	Kind     uint8
	Value    []byte
	Operator uint8
}

func ValidateFreshRouteEvidence(e FreshRouteEvidence, now time.Time, expectedOpportunityKey, expectedEpochFingerprint, delegatedSigner string, protected []RouteInstruction) (DecodedSquadsPolicy, error) {
	if e.Slot <= 0 || e.OpportunityID <= 0 || e.EpochID <= 0 || e.OpportunityKey != expectedOpportunityKey || e.EpochFingerprint != expectedEpochFingerprint {
		return DecodedSquadsPolicy{}, errors.New("opportunity or market epoch fence changed")
	}
	if e.ObservedAt.After(now) || now.Sub(e.ObservedAt) > 15*time.Second {
		return DecodedSquadsPolicy{}, errors.New("fresh route evidence expired")
	}
	required := map[string]int{"vault": 1, "reserve": 2, "obligation": 2, "token_account": 1, "policy": 1}
	for _, a := range e.Accounts {
		if !a.Exists || a.Executable || a.Slot < e.Slot || len(a.DataSHA256) != 64 {
			return DecodedSquadsPolicy{}, fmt.Errorf("invalid fresh %s account", a.Kind)
		}
		expectedOwner := ""
		switch a.Kind {
		case "reserve", "obligation":
			expectedOwner = KLendProgram
		case "policy":
			expectedOwner = SquadsProgram
		case "farm":
			expectedOwner = farmsProgram
		case "token_account":
			if a.Owner != "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" && a.Owner != "TokenzQdYhDYEzV8znWVkuxHcQKoZbYGWvVGg9Lzc" {
				return DecodedSquadsPolicy{}, errors.New("fresh token account owner is invalid")
			}
		}
		if expectedOwner != "" && a.Owner != expectedOwner {
			return DecodedSquadsPolicy{}, fmt.Errorf("fresh %s account owner is invalid", a.Kind)
		}
		if _, ok := required[a.Kind]; ok {
			required[a.Kind]--
		}
	}
	for kind, count := range required {
		if count > 0 {
			return DecodedSquadsPolicy{}, fmt.Errorf("fresh %s account evidence missing", kind)
		}
	}
	p, err := DecodeSquadsPolicy(e.PolicyData)
	if err != nil {
		return p, err
	}
	if len(p.DelegatedSigners) != 1 || p.DelegatedSigners[0] != delegatedSigner || len(p.SignerPermissions) != 1 || p.SignerPermissions[0] != 7 || p.TimeLock != 0 || p.StaleTransactionIndex > p.TransactionIndex {
		return p, errors.New("policy does not exclusively delegate full permissions to signer")
	}
	used := make(map[int]bool)
	for _, ix := range protected {
		matched := -1
		for index, constraint := range p.Constraints {
			if used[index] || !policyConstraintMatches(constraint, ix) {
				continue
			}
			matched = index
			break
		}
		if matched < 0 || matched > math.MaxUint8 {
			return p, fmt.Errorf("policy has no exact allowed instruction index for %s", ix.Step)
		}
		used[matched] = true
		p.AllowedIndexes = append(p.AllowedIndexes, uint8(matched))
	}
	return p, nil
}
func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

// DecodeSquadsPolicy is a direct port of the mature Rust worker's deployed
// ProgramInteraction decoder. It accepts both the legacy Borsh vectors and
// Squads' compact pubkey-table layout, while retaining every account and data
// constraint for exact instruction-index validation.
func DecodeSquadsPolicy(data []byte) (DecodedSquadsPolicy, error) {
	c := wireCursor{b: data}
	if !bytes.Equal(c.take(8), []byte{222, 135, 7, 163, 235, 177, 33, 68}) {
		return DecodedSquadsPolicy{}, errors.New("not a Squads policy account")
	}
	p := DecodedSquadsPolicy{Settings: encodeBase58(c.take(32)), PolicySeed: c.u64(), Bump: c.u8()}
	p.TransactionIndex = c.u64()
	p.StaleTransactionIndex = c.u64()
	n := c.u32()
	if c.err != nil || n == 0 || n > 32 {
		return DecodedSquadsPolicy{}, errors.New("invalid policy signer count")
	}
	for i := uint32(0); i < n; i++ {
		p.DelegatedSigners = append(p.DelegatedSigners, encodeBase58(c.take(32)))
		p.SignerPermissions = append(p.SignerPermissions, c.u8())
	}
	threshold := c.u16()
	p.TimeLock = c.u32()
	if threshold != 1 || c.u8() != 3 {
		return p, errors.New("policy is not threshold-one ProgramInteraction")
	}
	p.AccountIndex = c.u8()
	start := c
	constraints, err := decodeLegacyPolicyConstraints(&c)
	if err != nil {
		c = start
		constraints, err = decodeCompactPolicyConstraints(&c)
	}
	if err != nil || c.err != nil || len(constraints) == 0 {
		return p, fmt.Errorf("decode ProgramInteraction constraints: %w", err)
	}
	p.Constraints = constraints
	for _, constraint := range constraints {
		p.InstructionPrograms = append(p.InstructionPrograms, constraint.Program)
		var exact []byte
		for _, value := range constraint.Data {
			if value.Offset == 0 && value.Kind == 5 && value.Operator == 0 {
				exact = append([]byte(nil), value.Value...)
				break
			}
		}
		p.InstructionData = append(p.InstructionData, exact)
	}
	return p, nil
}

func decodeLegacyPolicyConstraints(c *wireCursor) ([]policyInstructionConstraint, error) {
	count := c.u32()
	if c.err != nil || count == 0 || count > 128 {
		return nil, errors.New("invalid legacy policy constraint count")
	}
	out := make([]policyInstructionConstraint, 0, count)
	for i := uint32(0); i < count; i++ {
		constraint := policyInstructionConstraint{Program: encodeBase58(c.take(32))}
		accountCount := c.u32()
		if accountCount > 128 {
			return nil, errors.New("legacy account constraint count exceeds 128")
		}
		for j := uint32(0); j < accountCount; j++ {
			account := policyAccountConstraint{Index: c.u8()}
			switch c.u8() {
			case 0:
				n := c.u32()
				if n > 128 {
					return nil, errors.New("legacy pubkey constraint count exceeds 128")
				}
				for k := uint32(0); k < n; k++ {
					account.Pubkeys = append(account.Pubkeys, encodeBase58(c.take(32)))
				}
			case 1:
				n := c.u32()
				if n > 128 {
					return nil, errors.New("legacy account-data constraint count exceeds 128")
				}
				for k := uint32(0); k < n; k++ {
					if _, err := decodePolicyDataConstraint(c); err != nil {
						return nil, err
					}
				}
			default:
				return nil, errors.New("unknown legacy account constraint kind")
			}
			switch c.u8() {
			case 0:
			case 1:
				account.Owner = encodeBase58(c.take(32))
			default:
				return nil, errors.New("invalid legacy owner option")
			}
			constraint.Accounts = append(constraint.Accounts, account)
		}
		dataCount := c.u32()
		if dataCount > 128 {
			return nil, errors.New("legacy data constraint count exceeds 128")
		}
		for j := uint32(0); j < dataCount; j++ {
			value, err := decodePolicyDataConstraint(c)
			if err != nil {
				return nil, err
			}
			constraint.Data = append(constraint.Data, value)
		}
		out = append(out, constraint)
	}
	if c.err != nil {
		return nil, c.err
	}
	return out, nil
}

func decodeCompactPolicyConstraints(c *wireCursor) ([]policyInstructionConstraint, error) {
	tableCount := int(c.u8())
	if tableCount > 240 {
		return nil, errors.New("compact pubkey table exceeds 240")
	}
	table := make([]string, tableCount)
	for i := range table {
		table[i] = encodeBase58(c.take(32))
	}
	key := func(index uint8) (string, error) {
		if int(index) >= len(table) {
			return "", errors.New("compact pubkey index out of bounds")
		}
		return table[index], nil
	}
	count := int(c.u8())
	if count == 0 || count > 128 {
		return nil, errors.New("invalid compact policy constraint count")
	}
	out := make([]policyInstructionConstraint, 0, count)
	for i := 0; i < count; i++ {
		program, err := key(c.u8())
		if err != nil {
			return nil, err
		}
		constraint := policyInstructionConstraint{Program: program}
		accountCount := int(c.u8())
		if accountCount > 128 {
			return nil, errors.New("compact account constraint count exceeds 128")
		}
		for j := 0; j < accountCount; j++ {
			account := policyAccountConstraint{Index: c.u8()}
			switch c.u8() {
			case 0:
				n := int(c.u8())
				if n > 128 {
					return nil, errors.New("compact pubkey constraint count exceeds 128")
				}
				for k := 0; k < n; k++ {
					v, e := key(c.u8())
					if e != nil {
						return nil, e
					}
					account.Pubkeys = append(account.Pubkeys, v)
				}
			case 1:
				n := int(c.u8())
				if n > 128 {
					return nil, errors.New("compact account-data constraint count exceeds 128")
				}
				for k := 0; k < n; k++ {
					if _, err := decodePolicyDataConstraint(c); err != nil {
						return nil, err
					}
				}
			default:
				return nil, errors.New("unknown compact account constraint kind")
			}
			switch c.u8() {
			case 0:
			case 1:
				account.Owner, err = key(c.u8())
				if err != nil {
					return nil, err
				}
			default:
				return nil, errors.New("invalid compact owner option")
			}
			constraint.Accounts = append(constraint.Accounts, account)
		}
		dataCount := int(c.u8())
		if dataCount > 128 {
			return nil, errors.New("compact data constraint count exceeds 128")
		}
		for j := 0; j < dataCount; j++ {
			value, err := decodePolicyDataConstraint(c)
			if err != nil {
				return nil, err
			}
			constraint.Data = append(constraint.Data, value)
		}
		out = append(out, constraint)
	}
	if c.err != nil {
		return nil, c.err
	}
	return out, nil
}

func decodePolicyDataConstraint(c *wireCursor) (policyDataConstraint, error) {
	value := policyDataConstraint{Offset: c.u64(), Kind: c.u8()}
	switch value.Kind {
	case 0:
		value.Value = append([]byte(nil), c.take(1)...)
	case 1:
		value.Value = append([]byte(nil), c.take(2)...)
	case 2:
		value.Value = append([]byte(nil), c.take(4)...)
	case 3:
		value.Value = append([]byte(nil), c.take(8)...)
	case 4:
		value.Value = append([]byte(nil), c.take(16)...)
	case 5:
		n := c.u32()
		if n > 256 {
			return value, errors.New("policy byte constraint exceeds 256")
		}
		value.Value = append([]byte(nil), c.take(int(n))...)
	default:
		return value, errors.New("unknown policy data value kind")
	}
	value.Operator = c.u8()
	if value.Operator > 5 {
		return value, errors.New("unknown policy data operator")
	}
	if c.err != nil {
		return value, c.err
	}
	return value, nil
}

func policyConstraintMatches(constraint policyInstructionConstraint, instruction RouteInstruction) bool {
	if constraint.Program != instruction.Program {
		return false
	}
	for _, account := range constraint.Accounts {
		if int(account.Index) >= len(instruction.Accounts) {
			return false
		}
		if len(account.Pubkeys) > 0 && !contains(account.Pubkeys, instruction.Accounts[account.Index].Address) {
			return false
		}
	}
	for _, data := range constraint.Data {
		if !policyDataMatches(data, instruction.Data) {
			return false
		}
	}
	return true
}

func policyDataMatches(constraint policyDataConstraint, data []byte) bool {
	offset := constraint.Offset
	if offset > uint64(len(data)) || uint64(len(constraint.Value)) > uint64(len(data))-offset {
		return false
	}
	actual := data[int(offset) : int(offset)+len(constraint.Value)]
	compare := 0
	for i := len(actual) - 1; i >= 0; i-- {
		if actual[i] < constraint.Value[i] {
			compare = -1
			break
		}
		if actual[i] > constraint.Value[i] {
			compare = 1
			break
		}
	}
	if constraint.Kind == 5 && constraint.Operator > 1 {
		return false
	}
	switch constraint.Operator {
	case 0:
		return compare == 0
	case 1:
		return compare != 0
	case 2:
		return compare > 0
	case 3:
		return compare >= 0
	case 4:
		return compare < 0
	case 5:
		return compare <= 0
	}
	return false
}

// BuildExactPolicyFixture is exported only to deterministic verification. It
// emits the same deployed legacy layout decoded above.
func BuildExactPolicyFixture(settings, signer string, accountIndex uint8, instructions []RouteInstruction) ([]byte, error) {
	s, err := decodePublicKey(settings)
	if err != nil {
		return nil, err
	}
	sg, err := decodePublicKey(signer)
	if err != nil {
		return nil, err
	}
	b := []byte{222, 135, 7, 163, 235, 177, 33, 68}
	b = append(b, s[:]...)
	b = append(b, make([]byte, 8+1+8+8)...)
	b = appendU32x(b, 1)
	b = append(b, sg[:]...)
	b = append(b, 7)
	b = appendU16x(b, 1)
	b = appendU32x(b, 0)
	b = append(b, 3, accountIndex)
	b = appendU32x(b, uint32(len(instructions)))
	for _, ix := range instructions {
		p, e := decodePublicKey(ix.Program)
		if e != nil {
			return nil, e
		}
		b = append(b, p[:]...)
		b = appendU32x(b, 0)
		b = appendU32x(b, 1)
		b = appendU64x(b, 0)
		b = append(b, 5)
		b = appendU32x(b, uint32(len(ix.Data)))
		b = append(b, ix.Data...)
		b = append(b, 0)
	}
	return b, nil
}

type txMeta struct {
	key                       string
	signer, writable, program bool
}

type LookupTable struct {
	Address          string
	Addresses        []string
	Active           bool
	UsableAfterSlot  int64
	LastVerifiedSlot int64
}
type PreparedTransaction struct {
	Message                   []byte
	UnsignedWire              []byte
	MessageSHA256, WireSHA256 string
	LookupTables              []string
	WritableAccounts          []string
	PacketBytes               int
	FeeLamports, ComputeLimit uint64
}
type SimulationEvidence struct {
	Slot          int64  `json:"slot"`
	Succeeded     bool   `json:"succeeded"`
	UnitsConsumed uint64 `json:"unitsConsumed"`
	Error         string `json:"error,omitempty"`
	WireSHA256    string `json:"wireSha256"`
}

type RoutePreparation struct {
	RouteFingerprint, RequirementsFingerprint string
	Transaction                               PreparedTransaction
	Simulation                                SimulationEvidence
	ExecutionPlan                             json.RawMessage
}

func PrepareRoute(route KaminoSameMintRoute, policy, signer string, policyAccountIndex uint8, allowedIndexes []uint8, tables []LookupTable, recentBlockhash string, feeLamports, computeLimit uint64, simulate func([]byte) (SimulationEvidence, error)) (RoutePreparation, error) {
	if len(route.Protected) == 0 || len(allowedIndexes) != len(route.Protected) {
		return RoutePreparation{}, errors.New("protected instructions and policy indexes differ")
	}
	seenIndexes := make(map[uint8]bool, len(allowedIndexes))
	for _, index := range allowedIndexes {
		if seenIndexes[index] {
			return RoutePreparation{}, errors.New("duplicate allowed instruction constraint index")
		}
		seenIndexes[index] = true
	}
	// Rust's mature same-mint route executes each value-moving KLend
	// instruction through its own ProgramInteraction payload. Public refreshes
	// are interleaved around withdrawal/deposit so the target obligation is
	// refreshed after the source withdrawal and immediately before deposit.
	wrapped := make([]RouteInstruction, len(route.Protected))
	for i := range route.Protected {
		var err error
		wrapped[i], err = wrapSquadsPolicy(policy, signer, policyAccountIndex, []uint8{allowedIndexes[i]}, []RouteInstruction{route.Protected[i]})
		if err != nil {
			return RoutePreparation{}, err
		}
	}
	instructions, err := interleaveMatureSameMintRoute(route.Public, wrapped)
	if err != nil {
		return RoutePreparation{}, err
	}
	tx, missing, err := compileV0Transaction(signer, recentBlockhash, instructions, tables, feeLamports, computeLimit)
	if err != nil {
		return RoutePreparation{}, err
	}
	if len(missing) > 0 || len(tx.LookupTables) == 0 {
		return RoutePreparation{}, fmt.Errorf("missing reusable ALT coverage: %v", missing)
	}
	if tx.PacketBytes > SolanaPacketLimit {
		return RoutePreparation{}, fmt.Errorf("transaction packet %d exceeds %d", tx.PacketBytes, SolanaPacketLimit)
	}
	if feeLamports == 0 || computeLimit == 0 || computeLimit > defaultComputeLimit {
		return RoutePreparation{}, errors.New("fee or compute limit is invalid")
	}
	sim, err := simulate(tx.UnsignedWire)
	if err != nil {
		return RoutePreparation{}, err
	}
	if !sim.Succeeded || sim.Slot <= 0 || sim.UnitsConsumed > computeLimit || sim.WireSHA256 != tx.WireSHA256 {
		return RoutePreparation{}, errors.New("exact transaction simulation failed or mismatched")
	}
	routeBytes, _ := json.Marshal(route)
	reqBytes, _ := json.Marshal(struct {
		Tables  []string `json:"tables"`
		Compute uint64   `json:"compute"`
		Fee     uint64   `json:"fee"`
	}{tx.LookupTables, computeLimit, feeLamports})
	rf := sha256.Sum256(routeBytes)
	rq := sha256.Sum256(reqBytes)
	plan, _ := json.Marshal(struct {
		Kind                      string             `json:"kind"`
		MessageBase64             string             `json:"message_base64"`
		MessageSHA256             string             `json:"message_sha256"`
		UnsignedTransactionBase64 string             `json:"unsigned_transaction_base64"`
		UnsignedWireSHA256        string             `json:"unsigned_wire_sha256"`
		LookupTables              []string           `json:"lookup_tables"`
		WritableAccountKeys       []string           `json:"writable_account_keys"`
		PacketBytes               int                `json:"packet_bytes"`
		CompiledFeeLamports       uint64             `json:"compiled_fee_lamports"`
		ComputeUnitLimit          uint64             `json:"compute_unit_limit"`
		Simulation                SimulationEvidence `json:"simulation"`
	}{"same_mint_kamino_v0", base64.StdEncoding.EncodeToString(tx.Message), tx.MessageSHA256, base64.StdEncoding.EncodeToString(tx.UnsignedWire), tx.WireSHA256, tx.LookupTables, tx.WritableAccounts, tx.PacketBytes, tx.FeeLamports, tx.ComputeLimit, sim})
	return RoutePreparation{hex.EncodeToString(rf[:]), hex.EncodeToString(rq[:]), tx, sim, plan}, nil
}

func interleaveMatureSameMintRoute(public, wrapped []RouteInstruction) ([]RouteInstruction, error) {
	if len(wrapped) != 2 {
		return nil, errors.New("mature same-mint route requires withdraw and deposit policy instructions")
	}
	firstObligation := -1
	for i := range public {
		if public[i].Step == "kamino_refresh_obligation" {
			firstObligation = i
			break
		}
	}
	if firstObligation < 0 || firstObligation+1 >= len(public) {
		return nil, errors.New("mature same-mint route requires source and target obligation refreshes")
	}
	instructions := append([]RouteInstruction(nil), public[:firstObligation+1]...)
	instructions = append(instructions, wrapped[0])
	instructions = append(instructions, public[firstObligation+1:]...)
	instructions = append(instructions, wrapped[1])
	return instructions, nil
}

func wrapSquadsPolicy(policy, signer string, accountIndex uint8, indexes []uint8, inner []RouteInstruction) (RouteInstruction, error) {
	if len(inner) == 0 || len(inner) > 255 {
		return RouteInstruction{}, errors.New("invalid inner instruction count")
	}
	accounts := []InstructionAccount{}
	push := func(a InstructionAccount) uint8 {
		for i := range accounts {
			if accounts[i].Address == a.Address {
				accounts[i].Signer = accounts[i].Signer || a.Signer
				accounts[i].Writable = accounts[i].Writable || a.Writable
				return uint8(i)
			}
		}
		accounts = append(accounts, a)
		return uint8(len(accounts) - 1)
	}
	compiled := []byte{uint8(len(inner))}
	for _, ix := range inner {
		ai := make([]byte, len(ix.Accounts))
		for i, a := range ix.Accounts {
			ai[i] = push(a)
		}
		pi := push(InstructionAccount{Address: ix.Program})
		compiled = append(compiled, pi, uint8(len(ai)))
		compiled = append(compiled, ai...)
		if len(ix.Data) > math.MaxUint16 {
			return RouteInstruction{}, errors.New("inner data too large")
		}
		compiled = appendU16x(compiled, uint16(len(ix.Data)))
		compiled = append(compiled, ix.Data...)
	}
	for i := range accounts {
		accounts[i].Signer = false
	}
	data := []byte{90, 81, 187, 81, 39, 70, 128, 78, accountIndex, 1, 1, 1, 1}
	data = appendU32x(data, uint32(len(indexes)))
	data = append(data, indexes...)
	data = append(data, 1, accountIndex)
	data = appendU32x(data, uint32(len(compiled)))
	data = append(data, compiled...)
	outer := []InstructionAccount{{policy, false, true}, {SquadsProgram, false, false}, {signer, true, false}}
	outer = append(outer, accounts...)
	return RouteInstruction{"squads_execute_program_interaction", SquadsProgram, outer, data}, nil
}

func compileV0Transaction(payer, blockhash string, instructions []RouteInstruction, tables []LookupTable, fee, compute uint64) (PreparedTransaction, []string, error) {
	payerKey, err := decodePublicKey(payer)
	if err != nil {
		return PreparedTransaction{}, nil, err
	}
	bh, err := decodePublicKey(blockhash)
	if err != nil {
		return PreparedTransaction{}, nil, err
	}
	_ = payerKey
	metas := map[string]*txMeta{payer: {payer, true, true, false}}
	for _, ix := range instructions {
		if _, e := decodePublicKey(ix.Program); e != nil {
			return PreparedTransaction{}, nil, e
		}
		m := metas[ix.Program]
		if m == nil {
			m = &txMeta{key: ix.Program, program: true}
			metas[ix.Program] = m
		}
		m.program = true
		for _, a := range ix.Accounts {
			if _, e := decodePublicKey(a.Address); e != nil {
				return PreparedTransaction{}, nil, e
			}
			m := metas[a.Address]
			if m == nil {
				m = &txMeta{key: a.Address}
				metas[a.Address] = m
			}
			m.signer = m.signer || a.Signer
			m.writable = m.writable || a.Writable
		}
	}
	eligible := map[string]struct {
		table   int
		ordinal int
	}{}
	selected := []int{}
	for ti, t := range tables {
		if !t.Active || t.UsableAfterSlot > t.LastVerifiedSlot {
			continue
		}
		for oi, a := range t.Addresses {
			if m := metas[a]; m != nil && !m.signer && !m.program {
				if _, ok := eligible[a]; !ok {
					eligible[a] = struct{ table, ordinal int }{ti, oi}
				}
			}
		}
	}
	static := []*txMeta{}
	for _, m := range metas {
		if _, ok := eligible[m.key]; !ok {
			static = append(static, m)
		}
	}
	sort.Slice(static, func(i, j int) bool {
		ri, rj := metaRank(static[i], payer), metaRank(static[j], payer)
		if ri != rj {
			return ri < rj
		}
		ik, _ := decodePublicKey(static[i].key)
		jk, _ := decodePublicKey(static[j].key)
		return bytes.Compare(ik[:], jk[:]) < 0
	})
	staticIndex := map[string]uint8{}
	for i, m := range static {
		staticIndex[m.key] = uint8(i)
	}
	type use struct{ w, r []uint8 }
	uses := map[int]*use{}
	for address, loc := range eligible {
		u := uses[loc.table]
		if u == nil {
			u = &use{}
			uses[loc.table] = u
			selected = append(selected, loc.table)
		}
		if metas[address].writable {
			u.w = append(u.w, uint8(loc.ordinal))
		} else {
			u.r = append(u.r, uint8(loc.ordinal))
		}
	}
	sort.Ints(selected)
	// solana-message's CompiledKeys is a BTreeMap keyed by raw Pubkey bytes.
	// Lookup indexes therefore follow pubkey order within writable/readonly
	// classes, not lookup-table ordinal order (which solana-go uses).
	for _, ti := range selected {
		lessPubkey := func(a, b uint8) bool {
			ak, _ := decodePublicKey(tables[ti].Addresses[int(a)])
			bk, _ := decodePublicKey(tables[ti].Addresses[int(b)])
			return bytes.Compare(ak[:], bk[:]) < 0
		}
		sort.Slice(uses[ti].w, func(i, j int) bool { return lessPubkey(uses[ti].w[i], uses[ti].w[j]) })
		sort.Slice(uses[ti].r, func(i, j int) bool { return lessPubkey(uses[ti].r[i], uses[ti].r[j]) })
	}
	dynamic := map[string]uint8{}
	next := len(static)
	// LoadedAddresses flattens writable addresses from every table first,
	// followed by readonly addresses from every table.
	for _, writable := range []bool{true, false} {
		for _, ti := range selected {
			ordinals := uses[ti].r
			if writable {
				ordinals = uses[ti].w
			}
			for _, ord := range ordinals {
				dynamic[tables[ti].Addresses[int(ord)]] = uint8(next)
				next++
			}
		}
	}
	missing := []string{}
	for _, m := range metas {
		if m.signer || m.program {
			continue
		}
		if _, ok := eligible[m.key]; !ok {
			missing = append(missing, m.key)
		}
	}
	sort.Strings(missing)
	if len(static) > 255 || next > 255 {
		return PreparedTransaction{}, nil, errors.New("v0 account index overflow")
	}
	required, roSigned, roUnsigned := 0, 0, 0
	for _, m := range static {
		if m.signer {
			required++
			if !m.writable {
				roSigned++
			}
		} else if !m.writable {
			roUnsigned++
		}
	}
	msg := []byte{0x80, uint8(required), uint8(roSigned), uint8(roUnsigned)}
	msg = append(msg, shortVec(len(static))...)
	for _, m := range static {
		k, _ := decodePublicKey(m.key)
		msg = append(msg, k[:]...)
	}
	msg = append(msg, bh[:]...)
	msg = append(msg, shortVec(len(instructions))...)
	for _, ix := range instructions {
		pi := staticIndex[ix.Program]
		msg = append(msg, pi)
		msg = append(msg, shortVec(len(ix.Accounts))...)
		for _, a := range ix.Accounts {
			idx, ok := staticIndex[a.Address]
			if !ok {
				idx = dynamic[a.Address]
			}
			msg = append(msg, idx)
		}
		msg = append(msg, shortVec(len(ix.Data))...)
		msg = append(msg, ix.Data...)
	}
	msg = append(msg, shortVec(len(selected))...)
	names := []string{}
	for _, ti := range selected {
		k, _ := decodePublicKey(tables[ti].Address)
		msg = append(msg, k[:]...)
		u := uses[ti]
		msg = append(msg, shortVec(len(u.w))...)
		msg = append(msg, u.w...)
		msg = append(msg, shortVec(len(u.r))...)
		msg = append(msg, u.r...)
		names = append(names, tables[ti].Address)
	}
	wire := append(shortVec(required), make([]byte, 64*required)...)
	wire = append(wire, msg...)
	mh := sha256.Sum256(msg)
	wh := sha256.Sum256(wire)
	writables := []string{}
	for _, m := range metas {
		if m.writable {
			writables = append(writables, m.key)
		}
	}
	sort.Strings(writables)
	return PreparedTransaction{msg, wire, hex.EncodeToString(mh[:]), hex.EncodeToString(wh[:]), names, writables, len(wire), fee, compute}, missing, nil
}
func metaRank(m *txMeta, payer string) int {
	if m.key == payer {
		return 0
	}
	if m.signer && m.writable {
		return 1
	}
	if m.signer {
		return 2
	}
	if m.writable {
		return 3
	}
	return 4
}
func shortVec(n int) []byte {
	out := []byte{}
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}
func appendU16x(b []byte, v uint16) []byte { return append(b, byte(v), byte(v>>8)) }
func appendU32x(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
func appendU64x(b []byte, v uint64) []byte {
	for i := 0; i < 8; i++ {
		b = append(b, byte(v))
		v >>= 8
	}
	return b
}

type wireCursor struct {
	b   []byte
	i   int
	err error
}

func (c *wireCursor) take(n int) []byte {
	if c.err != nil {
		return nil
	}
	if n < 0 || c.i+n > len(c.b) {
		c.err = errors.New("truncated")
		return nil
	}
	v := c.b[c.i : c.i+n]
	c.i += n
	return v
}
func (c *wireCursor) skip(n int) { c.take(n) }
func (c *wireCursor) u8() uint8 {
	v := c.take(1)
	if len(v) == 0 {
		return 0
	}
	return v[0]
}
func (c *wireCursor) u16() uint16 {
	v := c.take(2)
	if len(v) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(v)
}
func (c *wireCursor) u32() uint32 {
	v := c.take(4)
	if len(v) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(v)
}
func (c *wireCursor) u64() uint64 {
	v := c.take(8)
	if len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}
