package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	solana "github.com/solana-foundation/solana-go/v2"
)

// These preparation DTOs have no signer or submission capability. The command
// adapter converts them into the executor's custody protocol; fleet never
// imports the signing consumer. Every field is checked against durable state.
type CrossMintPreparationMovement struct {
	DecisionID, OpportunityID, OptimizerEpochID, VaultID      int64
	Cluster, VaultPubkey                                      string
	SourceSnapshotID                                          *int64
	SourceReserve, IntendedTargetReserve, ActiveTargetReserve string
	SourceMint, TargetMint                                    string
	PlannedAmountRaw                                          int64
	ExecutionPlan, PreflightCertification                     json.RawMessage
	CustodyMint, CustodyAccount                               string
	CustodyAmountRaw                                          int64
	CustodyObservedBalanceRaw, CustodyReconciledSlot          *int64
	CustodyVersion                                            int64
	Phase                                                     string
	TerminalOutcome                                           *string
}

type CrossMintPreparationRequest struct {
	Movement                                    CrossMintPreparationMovement
	Leg, Purpose                                string
	Generation, RemainingFeeLamports            int64
	ContinuationOwner                           string
	ContinuationFencingToken, ControlGeneration int64
	ExpiresAt                                   time.Time
}

type CrossMintLegPreparation struct {
	Preparation                     RoutePreparation
	LastValidBlockHeight            int64
	PolicyAccount                   string
	ExpectedEffect                  json.RawMessage
	BalanceAnchors                  json.RawMessage
	ConflictKeys                    []string
	SelectedALTs                    []ExecutionALT
	ExternalALTs                    []CrossMintExternalALT
	AltSelectionFingerprint         string
	ObservedSlot                    int64
	ObservedAt                      time.Time
	WaitingALT                      bool
	MissingAddresses                []string
	SharedAddresses, VaultAddresses []ALTManifestAddress
}

// Only a fully validated quote below the economic floor permits the caller to
// prepare source recovery. Provider, custody, policy and decoding errors hold.
var ErrCrossMintQuoteUnavailable = errors.New("validated cross-mint quote is below the required economic output")

// CrossMintPreflightCertificate is the retained source certificate emitted by
// certify_cross_mint_before_withdraw (fleet-worker/cross_mint.rs). It describes
// a verifier transaction, never an executable withdrawal-plus-swap route.
type CrossMintPreflightCertificate struct {
	Kind                         string                       `json:"kind"`
	CertifiedAt                  time.Time                    `json:"certifiedAt"`
	Cluster                      string                       `json:"cluster"`
	SourceMint                   string                       `json:"sourceMint"`
	TargetMint                   string                       `json:"targetMint"`
	InputAmountRaw               string                       `json:"inputAmountRaw"`
	MinimumOutputAmountRaw       string                       `json:"minimumOutputAmountRaw"`
	EffectiveSlippageBPS         uint16                       `json:"effectiveSlippageBps"`
	EffectiveMaximumValueLossBPS uint16                       `json:"effectiveMaximumValueLossBps"`
	FinalizedPolicyReadbacks     CrossMintCertificatePolicies `json:"finalizedPolicyReadbacks"`
	JupiterBuild                 CrossMintCertificateJupiter  `json:"jupiterBuild"`
}

type CrossMintCertificatePolicy struct {
	PolicyAccount string `json:"policyAccount"`
	ContextSlot   int64  `json:"contextSlot"`
	DataSHA256    string `json:"dataSha256"`
}

type CrossMintCertificateSwapPolicy struct {
	CrossMintCertificatePolicy
	PolicySeed                 string `json:"policySeed"`
	SourceShard                string `json:"sourceShard"`
	ManifestFingerprint        string `json:"manifestFingerprint"`
	Dialect                    string `json:"dialect"`
	ConstraintIndex            uint8  `json:"constraintIndex"`
	DailySourceMintSpendingCap string `json:"dailySourceMintSpendingCap"`
}

type CrossMintCertificatePolicies struct {
	Withdraw CrossMintCertificatePolicy     `json:"withdraw"`
	Swap     CrossMintCertificateSwapPolicy `json:"swap"`
	Deposit  CrossMintCertificatePolicy     `json:"deposit"`
}

type CrossMintCertificateJupiter struct {
	ResponseSHA256               string   `json:"responseSha256"`
	RouteStepCount               int      `json:"routeStepCount"`
	QuotedOutputAmountRaw        string   `json:"quotedOutputAmountRaw"`
	SetupInstructionCount        int      `json:"setupInstructionCount"`
	LookupTables                 []string `json:"lookupTables"`
	ComputeUnitLimit             uint64   `json:"computeUnitLimit"`
	PacketSizeBytes              int      `json:"packetSizeBytes"`
	PacketDataSizeBytes          int      `json:"packetDataSizeBytes"`
	FitsPacketDataSize           bool     `json:"fitsPacketDataSize"`
	MessageSHA256                string   `json:"messageSha256"`
	LastValidBlockHeight         int64    `json:"lastValidBlockHeight"`
	ObservedBlockHeight          int64    `json:"observedBlockHeight"`
	InputPreBalanceRaw           string   `json:"inputPreBalanceRaw"`
	OutputPreBalanceRaw          string   `json:"outputPreBalanceRaw"`
	SimulationAttempted          bool     `json:"simulationAttempted"`
	SimulationUnits              uint64   `json:"simulationUnits"`
	SimulationTopology           string   `json:"simulationTopology"`
	SimulationLookupTables       []string `json:"simulationLookupTables"`
	TargetDepositPolicyValidated bool     `json:"targetDepositPolicyValidated"`
	TargetReserve                string   `json:"targetReserve"`
	TargetObligation             string   `json:"targetObligation"`
}

func crossMintCertificateHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == value
}

func crossMintCertificateAmount(value string) (uint64, error) {
	amount, err := strconv.ParseUint(value, 10, 64)
	if err != nil || amount > math.MaxInt64 || strconv.FormatUint(amount, 10) != value {
		return 0, errors.New("certificate raw amount is not canonical SQL-range decimal")
	}
	return amount, nil
}

// ValidateCrossMintPreflightCertificate rejects a nonempty generic object and
// the older Go evidence shape. Those shapes cannot prove the source's complete
// target-policy and verifier topology gates. The caller must bind this exact
// typed certificate to an authoritative, write-once movement admission.
func ValidateCrossMintPreflightCertificate(m CrossMintPreparationMovement, now time.Time) (CrossMintPreflightCertificate, error) {
	var c CrossMintPreflightCertificate
	var plan crossMintPlan
	decoder := json.NewDecoder(bytes.NewReader(m.PreflightCertification))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil || !json.Valid(m.PreflightCertification) {
		return c, errors.New("cross-mint certificate is not the complete retained source shape")
	}
	if json.Unmarshal(m.ExecutionPlan, &plan) != nil || m.PlannedAmountRaw <= 0 || plan.Kind != "cross_mint_jupiter" || plan.Amount != uint64(m.PlannedAmountRaw) || plan.SourceMint != m.SourceMint || plan.TargetMint != m.TargetMint || plan.SourceMint == plan.TargetMint || plan.ValueLoss == 0 || plan.ValueLoss > 1000 || plan.Bindings.VaultPubkey != m.VaultPubkey || c.Kind != "cross_mint_preflight" || c.CertifiedAt.IsZero() || c.CertifiedAt.After(now.Add(5*time.Second)) || c.Cluster != m.Cluster || c.SourceMint != m.SourceMint || c.TargetMint != m.TargetMint || c.InputAmountRaw != strconv.FormatInt(m.PlannedAmountRaw, 10) || c.EffectiveSlippageBPS == 0 || c.EffectiveSlippageBPS > plan.Bindings.Swap.MaxSlippageBPS || c.EffectiveMaximumValueLossBPS == 0 || c.EffectiveMaximumValueLossBPS > plan.ValueLoss {
		return c, errors.New("cross-mint certificate identity or risk limits differ from immutable movement")
	}
	var amounts struct {
		Redeemable uint64 `json:"redeemable_source_liquidity_amount_raw"`
	}
	if json.Unmarshal(m.ExecutionPlan, &amounts) != nil || amounts.Redeemable != uint64(m.PlannedAmountRaw) {
		return c, errors.New("cross-mint certificate lacks original redeemable-liquidity amount binding")
	}
	minimum, err := crossMintCertificateAmount(c.MinimumOutputAmountRaw)
	if err != nil || minimum == 0 {
		return c, errors.New("cross-mint certificate minimum output is invalid")
	}
	floor, err := minimumEconomicOutput(uint64(m.PlannedAmountRaw), c.EffectiveMaximumValueLossBPS)
	if err != nil || minimum < floor {
		return c, errors.New("cross-mint certificate exceeds its signed value-loss limit")
	}
	for _, pair := range []struct {
		cert    CrossMintCertificatePolicy
		account string
		slot    uint64
	}{{c.FinalizedPolicyReadbacks.Withdraw, plan.Bindings.Withdraw.PolicyAccount, plan.Bindings.Withdraw.ObservedSlot}, {c.FinalizedPolicyReadbacks.Deposit, plan.Bindings.Deposit.PolicyAccount, plan.Bindings.Deposit.ObservedSlot}, {c.FinalizedPolicyReadbacks.Swap.CrossMintCertificatePolicy, plan.Bindings.Swap.PolicyAccount, plan.Bindings.Swap.ObservedSlot}} {
		if _, err := solana.PublicKeyFromBase58(pair.account); err != nil {
			return c, errors.New("certificate policy account is not a public identity")
		}
		if pair.slot == 0 || pair.cert.PolicyAccount != pair.account || pair.cert.ContextSlot <= 0 || uint64(pair.cert.ContextSlot) < pair.slot || !crossMintCertificateHash(pair.cert.DataSHA256) {
			return c, errors.New("certificate finalized policy readback lacks exact bound identity")
		}
	}
	if plan.Bindings.Withdraw.SourceCommitment != "finalized" || plan.Bindings.Deposit.SourceCommitment != "finalized" || plan.Bindings.Swap.SourceCommitment != "finalized" || plan.Bindings.Swap.DailySourceMintSpendingCap < uint64(m.PlannedAmountRaw) {
		return c, errors.New("certificate policy bindings lack finalized authority or full source spending cap")
	}
	swap := c.FinalizedPolicyReadbacks.Swap
	seed, err := strconv.ParseUint(swap.PolicySeed, 10, 64)
	if err != nil || strconv.FormatUint(seed, 10) != swap.PolicySeed {
		return c, errors.New("certificate swap policy seed is invalid")
	}
	derived, _, err := derivePolicyAccount(plan.Bindings.Settings, seed)
	if err != nil || derived != swap.PolicyAccount || swap.SourceShard != plan.Bindings.Swap.SourceShard || swap.ManifestFingerprint != plan.Bindings.Swap.ManifestFingerprint || !crossMintCertificateHash(swap.ManifestFingerprint) || swap.DailySourceMintSpendingCap != strconv.FormatUint(plan.Bindings.Swap.DailySourceMintSpendingCap, 10) || (swap.Dialect != "route_v2" || swap.ConstraintIndex != 0) && (swap.Dialect != "shared_accounts_route_v2" || swap.ConstraintIndex != 1) {
		return c, errors.New("certificate swap policy semantics differ from immutable binding")
	}
	j := c.JupiterBuild
	quoted, err := crossMintCertificateAmount(j.QuotedOutputAmountRaw)
	if err != nil || quoted < minimum || !crossMintCertificateHash(j.ResponseSHA256) || !crossMintCertificateHash(j.MessageSHA256) || j.RouteStepCount != 1 || j.SetupInstructionCount != 0 || j.ComputeUnitLimit == 0 || j.ComputeUnitLimit > defaultComputeLimit || j.SimulationUnits == 0 || j.SimulationUnits > j.ComputeUnitLimit || j.PacketSizeBytes <= 0 || j.PacketSizeBytes > SolanaPacketLimit || j.PacketDataSizeBytes != SolanaPacketLimit || !j.FitsPacketDataSize || !j.SimulationAttempted || j.SimulationTopology != "withdraw_then_swap_atomic_preflight_only" || !j.TargetDepositPolicyValidated || j.TargetReserve != m.IntendedTargetReserve || j.LastValidBlockHeight <= 0 || j.ObservedBlockHeight <= 0 || j.ObservedBlockHeight > j.LastValidBlockHeight || len(j.SimulationLookupTables) == 0 {
		return c, errors.New("certificate does not prove complete independent-leg prewithdraw verifier gates")
	}
	if _, err := solana.PublicKeyFromBase58(j.TargetObligation); err != nil {
		return c, errors.New("certificate target obligation is invalid")
	}
	for _, balance := range []string{j.InputPreBalanceRaw, j.OutputPreBalanceRaw} {
		if _, err := crossMintCertificateAmount(balance); err != nil {
			return c, err
		}
	}
	for _, tables := range [][]string{j.LookupTables, j.SimulationLookupTables} {
		seen := map[string]bool{}
		for _, table := range tables {
			if _, err := solana.PublicKeyFromBase58(table); err != nil || seen[table] {
				return c, errors.New("certificate ALT vector contains malformed or repeated identity")
			}
			seen[table] = true
		}
	}
	return c, nil
}

type crossMintPreparationStore interface {
	CheckCrossMintPreparation(context.Context, CrossMintPreparationRequest) error
}

type crossMintPreparationToken struct {
	Mint         string `json:"mint"`
	TokenAccount string `json:"tokenAccount"`
	AmountRaw    int64  `json:"amountRaw"`
}

// Retained Rust JSON tags are an external receipt contract, including nulls.
type crossMintPreparationPosition struct {
	Reserve          string `json:"reserve"`
	Market           string `json:"market"`
	Obligation       string `json:"obligation"`
	ObligationExists bool   `json:"obligationExists"`
	CollateralRaw    int64  `json:"depositedCollateralAmountRaw"`
}

type crossMintPreparationEffect struct {
	Debit         *crossMintPreparationToken `json:"debit"`
	CreditMint    *string                    `json:"creditMint"`
	CreditAccount *string                    `json:"creditTokenAccount"`
	MinimumCredit *int64                     `json:"minimumCreditAmountRaw"`
}

type crossMintPreparationAnchors struct {
	Debit    *crossMintPreparationToken    `json:"debit"`
	Credit   *crossMintPreparationToken    `json:"credit"`
	Position *crossMintPreparationPosition `json:"kaminoPosition"`
}

func validateCrossMintPreparationRequest(q CrossMintPreparationRequest, signer, owner string, now time.Time) (crossMintPlan, error) {
	m := q.Movement
	var plan crossMintPlan
	if m.DecisionID <= 0 || m.OpportunityID <= 0 || m.OptimizerEpochID <= 0 || m.VaultID <= 0 || m.Cluster == "" || m.SourceReserve == "" || m.IntendedTargetReserve == "" || m.ActiveTargetReserve == "" || m.SourceMint == m.TargetMint || m.PlannedAmountRaw <= 0 || m.CustodyAmountRaw < 0 || m.CustodyVersion < 0 || m.TerminalOutcome != nil || q.Generation <= 0 || q.RemainingFeeLamports <= 0 || q.ContinuationOwner != owner || q.ContinuationOwner == "" || q.ContinuationFencingToken <= 0 || q.ControlGeneration < 0 || !q.ExpiresAt.After(now.Add(5*time.Second)) {
		return plan, errors.New("cross-mint preparation lacks live movement authority")
	}
	if json.Unmarshal(m.ExecutionPlan, &plan) != nil || plan.Kind != "cross_mint_jupiter" || plan.SourceMint != m.SourceMint || plan.TargetMint != m.TargetMint || plan.Amount != uint64(m.PlannedAmountRaw) || plan.ValueLoss == 0 || plan.ValueLoss > 1000 {
		return plan, errors.New("cross-mint canonical plan or retained certification is invalid")
	}
	b := plan.Bindings
	if b.Settings == "" || b.VaultPubkey != m.VaultPubkey || b.DelegatedSigner != signer || b.Withdraw.SourceCommitment != "finalized" || b.Deposit.SourceCommitment != "finalized" || b.Swap.SourceCommitment != "finalized" || b.Withdraw.ObservedSlot == 0 || b.Deposit.ObservedSlot == 0 || b.Swap.ObservedSlot == 0 || b.Withdraw.ObservedSlot > math.MaxInt64 || b.Deposit.ObservedSlot > math.MaxInt64 || b.Swap.ObservedSlot > math.MaxInt64 || b.Withdraw.PolicyAccount == "" || b.Deposit.PolicyAccount == "" || b.Swap.PolicyAccount == "" {
		return plan, errors.New("cross-mint immutable policy bindings differ from preparation identity")
	}
	for _, key := range []string{m.VaultPubkey, m.SourceReserve, m.IntendedTargetReserve, m.ActiveTargetReserve, b.Settings, signer} {
		if _, err := solana.PublicKeyFromBase58(key); err != nil {
			return plan, errors.New("cross-mint preparation has invalid public identity")
		}
	}
	if _, ok := stableTokenProgram(m.SourceMint); !ok {
		return plan, errors.New("source mint is not a canonical Earn stable asset")
	}
	if _, ok := stableTokenProgram(m.TargetMint); !ok {
		return plan, errors.New("target mint is not a canonical Earn stable asset")
	}
	if _, err := ValidateCrossMintPreflightCertificate(m, now); err != nil {
		return plan, err
	}
	allowed := m.Phase == "source_reserve" && m.CustodyVersion == 0 && m.CustodyMint == m.SourceMint && m.CustodyAmountRaw == m.PlannedAmountRaw && q.Leg == "withdraw" && q.Purpose == "optimize_yield" ||
		m.Phase == "source_idle" && m.CustodyVersion > 0 && m.CustodyMint == m.SourceMint && (q.Leg == "swap" && q.Purpose == "optimize_yield" || q.Leg == "deposit" && q.Purpose == "recover_source") ||
		m.Phase == "target_idle" && m.CustodyVersion > 0 && m.CustodyMint == m.TargetMint && q.Leg == "deposit" && (q.Purpose == "optimize_yield" && m.ActiveTargetReserve == m.IntendedTargetReserve || q.Purpose == "fallback_target" && m.ActiveTargetReserve != m.IntendedTargetReserve)
	if !allowed {
		return plan, errors.New("cross-mint leg or purpose does not follow authoritative custody phase")
	}
	if q.Leg != "withdraw" && (m.CustodyAmountRaw <= 0 || m.CustodyObservedBalanceRaw == nil || *m.CustodyObservedBalanceRaw < m.CustodyAmountRaw || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0) {
		return plan, errors.New("idle custody lacks a finalized attribution anchor")
	}
	return plan, nil
}

// PrepareCrossMintLeg builds one independent leg, using the KLend builders or
// the strict Jupiter validator. It neither advances custody nor signs.
func (r *Revalidator) PrepareCrossMintLeg(ctx context.Context, q CrossMintPreparationRequest) (CrossMintLegPreparation, error) {
	var out CrossMintLegPreparation
	if r == nil || r.rpc == nil || !r.crossMintEnabled || r.computeLimit == 0 || r.slotDuration <= 0 {
		return out, errors.New("cross-mint preparation runtime is not configured")
	}
	plan, err := validateCrossMintPreparationRequest(q, r.signer, r.owner, time.Now())
	if err != nil {
		return out, err
	}
	certificate, err := ValidateCrossMintPreflightCertificate(q.Movement, time.Now())
	if err != nil {
		return out, err
	}
	if certificate.EffectiveMaximumValueLossBPS > r.crossMintMaxValueLossBPS || certificate.EffectiveSlippageBPS > r.crossMintMaxSlippageBPS {
		return out, errors.New("stored cross-mint certification exceeds current configured risk caps")
	}
	store, ok := r.store.(crossMintPreparationStore)
	if !ok {
		return out, errors.New("cross-mint preparation requires durable continuation checks")
	}
	ctx, cancel := context.WithDeadline(ctx, q.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	err = store.CheckCrossMintPreparation(ctx, q)
	if err != nil {
		return out, err
	}
	bank, err := r.loadCrossMintPreparationBank(ctx, q, plan, nil)
	if err != nil {
		return out, err
	}
	if q.Leg == "withdraw" {
		if certificate.JupiterBuild.TargetObligation != bank.target.Obligation {
			return out, errors.New("certified target obligation differs from finalized derived destination")
		}
		for _, p := range []CrossMintCertificatePolicy{certificate.FinalizedPolicyReadbacks.Withdraw, certificate.FinalizedPolicyReadbacks.Deposit, certificate.FinalizedPolicyReadbacks.Swap.CrossMintCertificatePolicy} {
			a := bank.accounts[p.PolicyAccount]
			hash := sha256.Sum256(a.Data)
			if a.Owner != SquadsProgram || a.Executable || a.Lamports == 0 || hex.EncodeToString(hash[:]) != p.DataSHA256 {
				return out, errors.New("finalized policy data differs from prewithdraw certificate")
			}
		}
	}
	if q.Leg != "withdraw" {
		if err := r.checkCrossMintCustody(q, bank); err != nil {
			return out, err
		}
	}
	var instructions []RouteInstruction
	var effect crossMintPreparationEffect
	var anchors crossMintPreparationAnchors
	if q.Leg == "swap" {
		instructions, effect, anchors, err = r.prepareCrossMintSwapInstructions(ctx, q, plan, &bank)
		out.PolicyAccount = plan.Bindings.Swap.PolicyAccount
	} else {
		instructions, effect, anchors, out.PolicyAccount, err = r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
	}
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	manifestInput := KaminoSameMintRouteRequest{Vault: q.Movement.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position}
	if q.Leg == "deposit" {
		p := bank.active.Position
		if q.Purpose == "recover_source" {
			p = bank.source.Position
		}
		manifestInput.Source, manifestInput.Target = p, p
	}
	manifest, err := BuildRouteALTManifest(manifestInput, plan.Bindings.Settings, out.PolicyAccount, r.signer, instructions, bank.swap)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	manifest, err = r.bindFinalizedCrossMintALTManifest(ctx, manifest, bank.externalTables, bank.slot)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	requirements, err := ALTManifestRequirementsFingerprint(&manifest)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.ExternalALTs, err = ALTManifestExternalSnapshots(&manifest)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.SharedAddresses, out.VaultAddresses = manifest.SharedAddresses, manifest.VaultAddresses
	required := requiredLookupTableAddresses(instructions)
	tables, err := r.store.LoadReusableLookupTables(ctx, q.Movement.Cluster, q.Movement.VaultID, bank.slot, required)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	tables, err = r.verifyFinalizedLookupTables(ctx, tables, bank.slot)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	combined, err := combineCrossMintLookupTables(tables, bank.externalTables)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.Preparation, out.LastValidBlockHeight, out.MissingAddresses, err = r.compileCrossMintIndependentLeg(ctx, q, instructions, combined, bank.slot)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.Preparation.RequirementsFingerprint = requirements
	out.Preparation.Manifest = &manifest
	if len(out.MissingAddresses) > 0 {
		if err := store.CheckCrossMintPreparation(ctx, q); err != nil {
			return CrossMintLegPreparation{}, err
		}
		out.WaitingALT = true
		return out, nil
	}
	for _, name := range out.Preparation.Transaction.LookupTables {
		found := false
		for _, table := range tables {
			if table.Address == name {
				if table.ID <= 0 || table.FamilyID <= 0 || table.Generation < 0 || table.MutationEpoch < 0 {
					return CrossMintLegPreparation{}, errors.New("compiled managed ALT lost registered generation identity")
				}
				out.SelectedALTs = append(out.SelectedALTs, ExecutionALT{TableID: table.ID, MutationEpoch: table.MutationEpoch, FamilyID: table.FamilyID, Generation: table.Generation, BindingID: table.BindingID, Address: table.Address, Addresses: append([]string(nil), table.Addresses...)})
				found = true
				break
			}
		}
		if !found {
			for _, external := range out.ExternalALTs {
				if external.Address == name {
					found = true
				}
			}
			if !found {
				return CrossMintLegPreparation{}, errors.New("compiled cross-mint ALT lacks managed identity or finalized provider snapshot")
			}
		}
	}
	out.AltSelectionFingerprint, err = CrossMintALTSelectionFingerprint(out.SelectedALTs, out.ExternalALTs, out.Preparation.Transaction.LookupTables)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.ConflictKeys = canonicalStrings([]string{"vault-write:" + q.Movement.VaultPubkey, fmt.Sprintf("fleet-shared-write-lane:%02d", q.Movement.VaultID%64)})
	out.ExpectedEffect, err = json.Marshal(effect)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	out.BalanceAnchors, err = json.Marshal(anchors)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	// Resolution, fee queries and simulation may take time. Re-observe the full
	// same-bank envelope and history before releasing signable bytes, just as
	// the source runtime does after its resolver finishes.
	finalBank, err := r.loadCrossMintPreparationBank(ctx, q, plan, bank.additionalMints)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	if !sameCrossMintPreparationBank(bank, finalBank) {
		return CrossMintLegPreparation{}, errors.New("cross-mint finalized account state changed during preparation")
	}
	err = store.CheckCrossMintPreparation(ctx, q)
	if err != nil {
		return CrossMintLegPreparation{}, err
	}
	if q.Leg != "withdraw" {
		if err := r.checkCrossMintCustody(q, finalBank); err != nil {
			return CrossMintLegPreparation{}, err
		}
	}
	if time.Since(bank.observedAt) > 15*time.Second || ctx.Err() != nil {
		return CrossMintLegPreparation{}, errors.New("cross-mint preparation exceeded its fresh account or lease deadline")
	}
	out.ObservedAt, out.ObservedSlot = bank.observedAt, bank.slot
	return out, nil
}

type crossMintPreparationBank struct {
	slot                                                 int64
	observedAt                                           time.Time
	source, target, active                               decodedRoutePosition
	sourceCollateral, targetCollateral, activeCollateral uint64
	sourceEconomics, targetEconomics                     ReserveState
	accounts                                             map[string]Account
	additionalMints                                      []string
	swap                                                 *RouteInstruction
	externalTables                                       []LookupTable
}

func sameCrossMintPreparationBank(a, b crossMintPreparationBank) bool {
	if b.slot < a.slot || len(a.accounts) != len(b.accounts) {
		return false
	}
	for name, account := range a.accounts {
		if !reflect.DeepEqual(account, b.accounts[name]) {
			return false
		}
	}
	return true
}

func (r *Revalidator) loadCrossMintPreparationBank(ctx context.Context, q CrossMintPreparationRequest, plan crossMintPlan, additionalMints []string) (crossMintPreparationBank, error) {
	var bank crossMintPreparationBank
	m, b := q.Movement, plan.Bindings
	floor := int64(max(b.Withdraw.ObservedSlot, b.Deposit.ObservedSlot, b.Swap.ObservedSlot))
	certificate, err := ValidateCrossMintPreflightCertificate(m, time.Now())
	if err != nil {
		return bank, err
	}
	floor = max(floor, certificate.FinalizedPolicyReadbacks.Withdraw.ContextSlot, certificate.FinalizedPolicyReadbacks.Deposit.ContextSlot, certificate.FinalizedPolicyReadbacks.Swap.ContextSlot)
	if m.CustodyReconciledSlot != nil {
		floor = max(floor, *m.CustodyReconciledSlot)
	}
	return r.loadCrossMintRouteBank(ctx, q, plan, additionalMints, floor)
}

// Discovery and repeat batch share one finalized bank for every route leg and
// the initial verifier; certificate/lease admission remains their caller's job.
func (r *Revalidator) loadCrossMintRouteBank(ctx context.Context, q CrossMintPreparationRequest, plan crossMintPlan, additionalMints []string, floor int64) (crossMintPreparationBank, error) {
	var bank crossMintPreparationBank
	m, b := q.Movement, plan.Bindings
	reserves := canonicalStrings([]string{m.SourceReserve, m.IntendedTargetReserve})
	policy, mint := b.Withdraw.PolicyAccount, m.SourceMint
	if q.Leg == "swap" {
		policy = b.Swap.PolicyAccount
	}
	if q.Leg == "deposit" {
		if q.Purpose == "recover_source" {
			reserves = []string{m.SourceReserve}
		} else {
			reserves = []string{m.ActiveTargetReserve}
			policy = b.Deposit.PolicyAccount
			mint = m.TargetMint
		}
	}
	bank.additionalMints = append([]string(nil), additionalMints...)
	discoverySlot, discovery, err := r.rpc.FinalizedAccounts(ctx, reserves, floor)
	if err != nil {
		return bank, err
	}
	preliminary := map[string]decodedRoutePosition{}
	addresses := append([]string{}, reserves...)
	addresses = append(addresses, mint, policy)
	if q.Leg == "withdraw" {
		addresses = append(addresses, b.Withdraw.PolicyAccount, b.Deposit.PolicyAccount, b.Swap.PolicyAccount)
	}
	if q.Leg != "deposit" {
		addresses = append(addresses, m.SourceMint, m.TargetMint)
	}
	for _, account := range discovery {
		p, err := decodeRouteReserve(account, m.VaultPubkey)
		if err != nil {
			return bank, err
		}
		preliminary[account.Address] = p
		addresses = append(addresses, p.Obligation, p.Position.VaultLiquidityATA)
		if p.Position.ReserveFarmState != "" {
			addresses = append(addresses, p.Position.ReserveFarmState, p.FarmUser)
		}
	}
	for _, mint := range additionalMints {
		ata, err := deriveATA(m.VaultPubkey, mint, mustStableProgram(mint))
		if err != nil {
			return bank, err
		}
		addresses = append(addresses, ata)
	}
	addresses = canonicalStrings(addresses)
	bank.slot, discovery, err = r.rpc.FinalizedAccounts(ctx, addresses, discoverySlot)
	if err != nil {
		return bank, err
	}
	bank.observedAt = time.Now().UTC()
	bank.accounts = map[string]Account{}
	for _, account := range discovery {
		bank.accounts[account.Address] = account
	}
	positions := map[string]decodedRoutePosition{}
	collateral := map[string]uint64{}
	for _, name := range reserves {
		account := bank.accounts[name]
		p, err := decodeRouteReserve(account, m.VaultPubkey)
		if err != nil || !reflect.DeepEqual(p, preliminary[name]) || account.Executable || account.Lamports == 0 {
			return bank, errors.New("cross-mint reserve identity changed during finalized discovery")
		}
		mint := m.TargetMint
		if name == m.SourceReserve {
			mint = m.SourceMint
		}
		program, _ := stableTokenProgram(mint)
		if p.Position.LiquidityMint != mint || p.Position.LiquidityTokenProgram != program {
			return bank, errors.New("cross-mint reserve mint or token program differs from canonical asset")
		}
		obligation := bank.accounts[p.Obligation]
		if obligation.Executable || obligation.Lamports == 0 {
			return bank, errors.New("cross-mint destination obligation is not initialized")
		}
		if _, err := decodeObligation(obligation, p.Position.Market, m.VaultPubkey, "", &p.Position); err != nil {
			return bank, err
		}
		for i := 0; i < 8; i++ {
			offset := 96 + i*136
			if encodeBase58(obligation.Data[offset:offset+32]) == name {
				collateral[name] = binary.LittleEndian.Uint64(obligation.Data[offset+32 : offset+40])
			}
		}
		if collateral[name] > math.MaxInt64 {
			return bank, errors.New("cross-mint position amount exceeds SQL custody range")
		}
		if err := validateVaultTokenAccount(bank.accounts[p.Position.VaultLiquidityATA], mint, m.VaultPubkey); err != nil {
			return bank, err
		}
		if binary.LittleEndian.Uint64(bank.accounts[p.Position.VaultLiquidityATA].Data[64:72]) > math.MaxInt64 {
			return bank, errors.New("cross-mint aggregate balance exceeds SQL custody range")
		}
		if err := validateStableMint(bank.accounts[mint], mint); err != nil {
			return bank, err
		}
		if p.Position.ReserveFarmState != "" {
			for _, name := range []string{p.Position.ReserveFarmState, p.FarmUser} {
				a := bank.accounts[name]
				if a.Owner != farmsProgram || a.Executable || a.Lamports == 0 {
					return bank, errors.New("cross-mint farm setup is not ready")
				}
			}
		}
		positions[name] = p
	}
	bank.source, bank.target, bank.active = positions[m.SourceReserve], positions[m.IntendedTargetReserve], positions[m.ActiveTargetReserve]
	bank.sourceCollateral, bank.targetCollateral, bank.activeCollateral = collateral[m.SourceReserve], collateral[m.IntendedTargetReserve], collateral[m.ActiveTargetReserve]
	if q.Leg != "deposit" {
		bank.sourceEconomics, err = DecodeKaminoReserve(bank.accounts[m.SourceReserve], ReserveIdentity{Address: m.SourceReserve, Market: bank.source.Position.Market, Mint: m.SourceMint}, bank.slot, r.slotDuration)
		if err != nil {
			return bank, err
		}
		bank.targetEconomics, err = DecodeKaminoReserve(bank.accounts[m.IntendedTargetReserve], ReserveIdentity{Address: m.IntendedTargetReserve, Market: bank.target.Position.Market, Mint: m.TargetMint}, bank.slot, r.slotDuration)
	} else {
		p := bank.active
		if q.Purpose == "recover_source" {
			p = bank.source
		}
		_, err = DecodeKaminoReserve(bank.accounts[p.Position.Reserve], ReserveIdentity{Address: p.Position.Reserve, Market: p.Position.Market, Mint: p.Position.LiquidityMint}, bank.slot, r.slotDuration)
	}
	return bank, err
}

func (r *Revalidator) prepareCrossMintKaminoInstructions(ctx context.Context, q CrossMintPreparationRequest, plan crossMintPlan, bank crossMintPreparationBank) ([]RouteInstruction, crossMintPreparationEffect, crossMintPreparationAnchors, string, error) {
	var effect crossMintPreparationEffect
	var anchors crossMintPreparationAnchors
	m, b := q.Movement, plan.Bindings
	position, collateral, binding, index := bank.active, bank.activeCollateral, b.Deposit, b.Deposit.ConstraintIndex
	if q.Leg == "withdraw" || q.Purpose == "recover_source" {
		position, collateral, binding = bank.source, bank.sourceCollateral, b.Withdraw
		index = binding.ConstraintIndex
		if q.Purpose == "recover_source" {
			index = 1 // immutable source recovery policy arm in the retained Rust protocol
		}
	}
	var route KaminoSameMintRoute
	var err error
	if q.Leg == "withdraw" {
		var values struct {
			Collateral uint64 `json:"source_collateral_amount_raw"`
			Recovery   uint64 `json:"source_recovery_anchor_collateral_raw"`
		}
		if json.Unmarshal(m.ExecutionPlan, &values) != nil || values.Recovery != 1 || values.Collateral == 0 || values.Collateral > math.MaxInt64-1 || values.Collateral+1 != collateral {
			return nil, effect, anchors, "", errors.New("cross-mint source collateral no longer matches one-unit recovery anchor")
		}
		account := bank.accounts[m.SourceReserve]
		backing, err := backyard.KaminoRedeemableLiquidity(backyard.ConfirmedAccount{Address: account.Address, Owner: account.Owner, Lamports: account.Lamports, Executable: account.Executable, Data: account.Data}, position.Position.Market, m.SourceMint, values.Collateral)
		if err != nil || backing == 0 {
			return nil, effect, anchors, "", errors.New("cross-mint withdrawal lacks redeemable source backing")
		}
		route, err = BuildCrossMintLegs(KaminoSameMintRouteRequest{Vault: m.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position, WithdrawCollateralAmount: values.Collateral, DepositLiquidityAmount: uint64(m.PlannedAmountRaw)})
		if err != nil {
			return nil, effect, anchors, "", err
		}
		if len(route.Protected) != 2 {
			return nil, effect, anchors, "", errors.New("cross-mint mature builder omitted independent legs")
		}
		last := -1
		for i, ix := range route.Public {
			if ix.Step == "kamino_refresh_obligation" {
				last = i
				break
			}
		}
		if last < 0 {
			return nil, effect, anchors, "", errors.New("withdrawal omitted source obligation refresh")
		}
		route.Public, route.Protected = route.Public[:last+1], route.Protected[:1]
		mint, ata, minimum := m.SourceMint, position.Position.VaultLiquidityATA, int64(1)
		effect.CreditMint, effect.CreditAccount, effect.MinimumCredit = &mint, &ata, &minimum
		anchors.Credit = &crossMintPreparationToken{mint, ata, int64(binary.LittleEndian.Uint64(bank.accounts[ata].Data[64:72]))}
	} else {
		if position.Position.VaultLiquidityATA != m.CustodyAccount || position.Position.LiquidityMint != m.CustodyMint {
			return nil, effect, anchors, "", errors.New("deposit destination differs from attributed custody")
		}
		route, err = BuildIdleDeposit(KaminoIdleDepositRequest{Vault: m.VaultPubkey, Target: position.Position, DepositLiquidityAmount: uint64(m.CustodyAmountRaw)})
		if err != nil {
			return nil, effect, anchors, "", err
		}
		effect.Debit = &crossMintPreparationToken{m.CustodyMint, m.CustodyAccount, m.CustodyAmountRaw}
		anchors.Debit = &crossMintPreparationToken{m.CustodyMint, m.CustodyAccount, *m.CustodyObservedBalanceRaw}
	}
	anchors.Position = &crossMintPreparationPosition{position.Position.Reserve, position.Position.Market, position.Obligation, true, int64(collateral)}
	policyAccount := bank.accounts[binding.PolicyAccount]
	policy, err := DecodeSquadsPolicy(policyAccount.Data)
	if err != nil {
		return nil, effect, anchors, "", err
	}
	derived, bump, err := derivePolicyAccount(b.Settings, policy.PolicySeed)
	if err != nil || derived != binding.PolicyAccount || bump != policy.Bump || policyAccount.Owner != SquadsProgram || policyAccount.Executable || policyAccount.Lamports == 0 || policy.Settings != b.Settings || policy.AccountIndex != b.VaultIndex || int(index) >= len(policy.Constraints) || !policyConstraintMatches(policy.Constraints[index], route.Protected[0]) {
		return nil, effect, anchors, "", errors.New("finalized Earn policy does not authorize exact cross-mint leg and index")
	}
	if _, err := validateDelegatedInstructions(policy, r.signer, route.Protected); err != nil {
		return nil, effect, anchors, "", err
	}
	wrapped, err := wrapSquadsPolicy(binding.PolicyAccount, r.signer, b.VaultIndex, []uint8{index}, route.Protected)
	if err != nil {
		return nil, effect, anchors, "", err
	}
	return append(append([]RouteInstruction{}, route.Public...), wrapped), effect, anchors, binding.PolicyAccount, nil
}

func (r *Revalidator) prepareCrossMintSwapInstructions(ctx context.Context, q CrossMintPreparationRequest, plan crossMintPlan, bank *crossMintPreparationBank) ([]RouteInstruction, crossMintPreparationEffect, crossMintPreparationAnchors, error) {
	var effect crossMintPreparationEffect
	var anchors crossMintPreparationAnchors
	m, b := q.Movement, plan.Bindings
	if r.jupiter == nil || b.Swap.MaxSlippageBPS == 0 || b.Swap.MaxSlippageBPS > 10_000 || uint64(m.CustodyAmountRaw) > b.Swap.DailySourceMintSpendingCap || r.crossMintMaxSlippageBPS == 0 || r.crossMintMaxValueLossBPS == 0 {
		return nil, effect, anchors, errors.New("cross-mint swap policy or local limits are invalid")
	}
	if bank.targetEconomics.LastUpdateStale || bank.targetEconomics.EconomicLifetimeMillis <= 0 || bank.targetEconomics.TotalSupplyUSDMicros <= minimumReserveSupplyUSDMicros || bank.targetEconomics.SupplyAPYBPS < 0 || bank.targetEconomics.SupplyAPYBPS >= 5000 {
		return nil, effect, anchors, errors.New("cross-mint intended target is no longer economically eligible")
	}
	amount := uint64(m.CustodyAmountRaw)
	minimum, err := minimumEconomicOutput(amount, min(plan.ValueLoss, r.crossMintMaxValueLossBPS))
	if err != nil {
		return nil, effect, anchors, err
	}
	profitable, err := minimumProfitableCrossMintOutput(m.ExecutionPlan, amount, bank.sourceEconomics.SupplyAPYBPS, bank.targetEconomics.SupplyAPYBPS)
	if err != nil {
		return nil, effect, anchors, err
	}
	minimum = max(minimum, profitable)
	plan.Amount = amount // post-withdraw attribution, never the stale planned estimate
	slippage := min(b.Swap.MaxSlippageBPS, r.crossMintMaxSlippageBPS)
	var validated validatedJupiterBuild
	var externalTables []LookupTable
	for attempt := 0; attempt < 2; attempt++ {
		body, err := r.jupiter.fetch(ctx, m.SourceMint, m.TargetMint, amount, m.VaultPubkey, slippage)
		if err != nil {
			return nil, effect, anchors, err
		}
		var envelope rawJupiterBuild
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, effect, anchors, err
		}
		tables, err := r.loadFinalizedJupiterTables(ctx, envelope.AddressesByLookupTableAddress, bank.slot)
		if err != nil {
			return nil, effect, anchors, err
		}
		validated, err = validateJupiterEnvelope(body, plan, m.VaultPubkey, slippage, tables)
		if err != nil {
			return nil, effect, anchors, err
		}
		externalTables = tables
		if validated.MinimumOutput >= minimum {
			break
		}
		if validated.QuotedOutput <= minimum || attempt == 1 {
			return nil, effect, anchors, ErrCrossMintQuoteUnavailable
		}
		hi, lo := bits.Mul64(validated.QuotedOutput-minimum, 10_000)
		available, _ := bits.Div64(hi, lo, validated.QuotedOutput)
		if available <= 1 {
			return nil, effect, anchors, ErrCrossMintQuoteUnavailable
		}
		slippage = min(slippage, uint16(available-1))
	}
	var additional []string
	for _, mint := range earnStableMints {
		if mint == m.SourceMint || mint == m.TargetMint {
			continue
		}
		ata, _ := deriveATA(m.VaultPubkey, mint, mustStableProgram(mint))
		for _, meta := range validated.Swap.Accounts {
			if meta.Address == ata {
				additional = append(additional, mint)
				break
			}
		}
	}
	if len(additional) > 0 {
		fresh, err := r.loadCrossMintPreparationBank(ctx, q, plan, additional)
		if err != nil {
			return nil, effect, anchors, err
		}
		if !sameCrossMintCoreBank(*bank, fresh) {
			return nil, effect, anchors, errors.New("finalized swap accounts changed during quote validation")
		}
		*bank = fresh
		for _, mint := range additional {
			ata, _ := deriveATA(m.VaultPubkey, mint, mustStableProgram(mint))
			if err := validateVaultTokenAccount(bank.accounts[ata], mint, m.VaultPubkey); err != nil {
				return nil, effect, anchors, err
			}
		}
	}
	account := bank.accounts[b.Swap.PolicyAccount]
	if account.Owner != SquadsProgram || account.Executable || account.Lamports == 0 {
		return nil, effect, anchors, errors.New("cross-mint swap policy is not a funded finalized Squads account")
	}
	policy, table, limits, err := decodeStrictSwapPolicy(account.Data)
	if err != nil {
		return nil, effect, anchors, err
	}
	if err := validateCrossMintSwapPolicy(policy, table, limits, b, validated.Swap, validated.Dialect); err != nil {
		return nil, effect, anchors, err
	}
	wrapped, err := wrapSquadsPolicy(b.Swap.PolicyAccount, r.signer, b.VaultIndex, []uint8{validated.ConstraintIndex}, []RouteInstruction{validated.Swap})
	if err != nil {
		return nil, effect, anchors, err
	}
	if validated.MinimumOutput > math.MaxInt64 {
		return nil, effect, anchors, errors.New("cross-mint swap credit exceeds SQL custody range")
	}
	mint, ata, minimumRaw := m.TargetMint, bank.target.Position.VaultLiquidityATA, int64(validated.MinimumOutput)
	effect = crossMintPreparationEffect{Debit: &crossMintPreparationToken{m.CustodyMint, m.CustodyAccount, m.CustodyAmountRaw}, CreditMint: &mint, CreditAccount: &ata, MinimumCredit: &minimumRaw}
	anchors = crossMintPreparationAnchors{Debit: &crossMintPreparationToken{m.CustodyMint, m.CustodyAccount, *m.CustodyObservedBalanceRaw}, Credit: &crossMintPreparationToken{mint, ata, int64(binary.LittleEndian.Uint64(bank.accounts[ata].Data[64:72]))}}
	bank.swap = &validated.Swap
	bank.externalTables = externalTables
	return []RouteInstruction{wrapped}, effect, anchors, nil
}

func (r *Revalidator) compileCrossMintIndependentLeg(ctx context.Context, q CrossMintPreparationRequest, instructions []RouteInstruction, tables []LookupTable, slot int64) (RoutePreparation, int64, []string, error) {
	var out RoutePreparation
	blockhash, height, err := r.rpc.finalizedBlockhash(ctx, slot)
	if err != nil {
		return out, 0, nil, err
	}
	base := append(computeBudgetInstructions(uint32(r.computeLimit), 0), instructions...)
	preview, static, err := compileV0Transaction(r.signer, blockhash, base, tables, 1, r.computeLimit)
	if err != nil {
		return out, 0, nil, err
	}
	required := map[string]bool{}
	for _, name := range requiredLookupTableAddresses(instructions) {
		required[name] = true
	}
	var missing []string
	for _, name := range static {
		if required[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 || len(preview.LookupTables) == 0 {
		if len(missing) == 0 {
			missing = requiredLookupTableAddresses(instructions)
		}
		out = crossMintMissingALTPreparation(q, instructions, missing)
		return out, 0, canonicalStrings(missing), nil
	}
	baseline, err := r.rpc.simulateExactTransaction(ctx, preview.UnsignedWire, slot, "finalized")
	if err != nil || !baseline.Succeeded || baseline.UnitsConsumed > r.computeLimit || preview.PacketBytes > SolanaPacketLimit {
		if err == nil {
			err = errors.New("cross-mint independent baseline simulation failed")
		}
		return out, 0, nil, err
	}
	compute := paddedComputeUnits(baseline.UnitsConsumed)
	if compute > r.computeLimit {
		return out, 0, nil, errors.New("cross-mint measured compute exceeds configured maximum")
	}
	fee, err := r.rpc.feeForMessage(ctx, preview.Message, slot, "finalized")
	if err != nil || fee > uint64(q.RemainingFeeLamports) {
		if err == nil {
			err = errors.New("cross-mint baseline fee exhausts remaining movement budget")
		}
		return out, 0, nil, err
	}
	priority, err := r.rpc.RecentPriorityFee(ctx, preview.WritableAccounts)
	if err != nil {
		return out, 0, nil, err
	}
	remaining := uint64(q.RemainingFeeLamports) - fee
	cap := uint64(math.MaxUint64)
	if remaining <= math.MaxUint64/1_000_000 {
		cap = remaining * 1_000_000 / compute
	}
	priority = min(priority, cap)
	budgeted := append(computeBudgetInstructions(uint32(compute), priority), instructions...)
	out.Transaction, _, err = compileV0Transaction(r.signer, blockhash, budgeted, tables, 1, compute)
	if err != nil {
		return RoutePreparation{}, 0, nil, err
	}
	if out.Transaction.PacketBytes > SolanaPacketLimit {
		return RoutePreparation{}, 0, nil, errors.New("cross-mint final packet exceeds Solana limit")
	}
	out.Transaction.FeeLamports, err = r.rpc.feeForMessage(ctx, out.Transaction.Message, slot, "finalized")
	if err != nil || out.Transaction.FeeLamports > uint64(q.RemainingFeeLamports) {
		if err == nil {
			err = errors.New("cross-mint final compiled fee exceeds remaining movement budget")
		}
		return RoutePreparation{}, 0, nil, err
	}
	out.Simulation, err = r.rpc.simulateExactTransaction(ctx, out.Transaction.UnsignedWire, slot, "finalized")
	if err != nil || !out.Simulation.Succeeded || out.Simulation.UnitsConsumed > compute || out.Simulation.WireSHA256 != out.Transaction.WireSHA256 {
		if err == nil {
			err = errors.New("cross-mint exact final simulation failed")
		}
		return RoutePreparation{}, 0, nil, err
	}
	routeHash := sha256.Sum256(out.Transaction.Message)
	requirements, _ := json.Marshal(struct {
		Leg, Purpose string
		Generation   int64
		Required     []string
	}{q.Leg, q.Purpose, q.Generation, requiredLookupTableAddresses(instructions)})
	requirementHash := sha256.Sum256(requirements)
	out.RouteFingerprint, out.RequirementsFingerprint = hex.EncodeToString(routeHash[:]), hex.EncodeToString(requirementHash[:])
	out.ExecutionPlan = bytes.Clone(q.Movement.ExecutionPlan)
	return out, height, nil, nil
}

func (s *Store) CheckCrossMintPreparation(ctx context.Context, q CrossMintPreparationRequest) error {
	m := q.Movement
	var valid bool
	err := s.pool.QueryRow(ctx, `SELECT d.continuation_lease_owner=$2 AND d.continuation_fencing_token=$3
 AND d.continuation_control_generation=$4 AND d.continuation_lease_expires_at=$5 AND d.continuation_lease_expires_at>clock_timestamp()+interval '5 seconds'
 AND o.id=$6 AND o.optimizer_epoch_id=$7 AND o.cluster=$8 AND d.vault_id=$9 AND o.vault_id=$9 AND v.vault_pubkey=$10
 AND d.source_snapshot_id IS NOT DISTINCT FROM $11::bigint AND d.source_reserve=$12 AND d.target_reserve=$13 AND d.active_target_reserve=$14
 AND d.source_liquidity_mint=$15 AND d.target_liquidity_mint=$16 AND d.amount_raw=$17
 AND o.execution_plan=$18::jsonb AND d.cross_mint_preflight_certification=$19::jsonb
 AND d.custody_mint=$20 AND d.custody_account=$21 AND d.custody_amount_raw=$22 AND d.custody_version=$23
 AND d.custody_observed_balance_raw IS NOT DISTINCT FROM $24::bigint AND d.custody_reconciled_slot IS NOT DISTINCT FROM $25::bigint
 AND d.movement_route='cross_mint_jupiter' AND d.status='confirming' AND d.terminal_outcome IS NULL
 AND COALESCE(c.continue_or_recover_existing,true) AND COALESCE(c.generation,0)=$4
 AND ($26<>'withdraw' OR COALESCE(c.start_new_movements,false))
 AND (SELECT COALESCE(max(leg_generation),0)+1 FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id AND movement_leg=$26)=$27
 AND o.estimated_cost_lamports-COALESCE((SELECT sum(compiled_fee_lamports) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),0)=$28
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions holding WHERE holding.decision_id=d.id AND holding.submission_state NOT IN `+submissionTerminalStates+`)
 FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id
 JOIN loyal_yield.managed_vaults v ON v.id=d.vault_id
 LEFT JOIN loyal_yield.cross_mint_movement_controls c ON c.cluster=o.cluster WHERE d.id=$1`, m.DecisionID, q.ContinuationOwner, q.ContinuationFencingToken, q.ControlGeneration, q.ExpiresAt, m.OpportunityID, m.OptimizerEpochID, m.Cluster, m.VaultID, m.VaultPubkey, m.SourceSnapshotID, m.SourceReserve, m.IntendedTargetReserve, m.ActiveTargetReserve, m.SourceMint, m.TargetMint, m.PlannedAmountRaw, string(m.ExecutionPlan), string(m.PreflightCertification), m.CustodyMint, m.CustodyAccount, m.CustodyAmountRaw, m.CustodyVersion, m.CustodyObservedBalanceRaw, m.CustodyReconciledSlot, q.Leg, q.Generation, q.RemainingFeeLamports).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return errors.New("cross-mint continuation authority, custody or leg budget changed")
	}
	if err != nil {
		return err
	}
	return nil
}

// The signing consumer must prove recognized finalized custody history before
// and after this preparation. This readback independently binds its exact
// aggregate to a coherent bank; equality alone is never attribution proof.
func (r *Revalidator) checkCrossMintCustody(q CrossMintPreparationRequest, bank crossMintPreparationBank) error {
	m := q.Movement
	program, _ := stableTokenProgram(m.CustodyMint)
	ata, err := deriveATA(m.VaultPubkey, m.CustodyMint, program)
	if err != nil || ata != m.CustodyAccount {
		return errors.New("cross-mint custody is not the canonical vault ATA")
	}
	a := bank.accounts[m.CustodyAccount]
	if err := validateVaultTokenAccount(a, m.CustodyMint, m.VaultPubkey); err != nil {
		return err
	}
	amount := binary.LittleEndian.Uint64(a.Data[64:72])
	if amount > math.MaxInt64 || int64(amount) != *m.CustodyObservedBalanceRaw || amount < uint64(m.CustodyAmountRaw) {
		return errors.New("finalized custody aggregate differs from historical attribution")
	}
	return nil
}

// Waiting metadata is not signable and is stable across retries of this exact
// movement leg and generation. The consumer owns fenced durable provisioning.
func crossMintMissingALTPreparation(q CrossMintPreparationRequest, instructions []RouteInstruction, missing []string) RoutePreparation {
	route, _ := json.Marshal(struct {
		DecisionID   int64
		Leg, Purpose string
		Generation   int64
		Instructions []RouteInstruction
	}{q.Movement.DecisionID, q.Leg, q.Purpose, q.Generation, instructions})
	requirements, _ := json.Marshal(canonicalStrings(missing))
	routeHash, requirementHash := sha256.Sum256(route), sha256.Sum256(requirements)
	return RoutePreparation{RouteFingerprint: hex.EncodeToString(routeHash[:]), RequirementsFingerprint: hex.EncodeToString(requirementHash[:]), ExecutionPlan: bytes.Clone(q.Movement.ExecutionPlan)}
}

func sameCrossMintCoreBank(a, b crossMintPreparationBank) bool {
	if b.slot < a.slot {
		return false
	}
	for name, account := range a.accounts {
		if !reflect.DeepEqual(account, b.accounts[name]) {
			return false
		}
	}
	return true
}
