package fleetexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	solana "github.com/solana-foundation/solana-go/v2"
)

func TestRevalidatorCrossMintRequestPreservesAuthorityAndCustody(t *testing.T) {
	amount, slot, snapshot, terminal := int64(991), int64(123), int64(8), "terminal"
	m := CrossMintMovement{DecisionID: 1, OpportunityID: 2, OptimizerEpochID: 3, VaultID: 4, Cluster: "localnet", VaultPubkey: "vault", SourceSnapshotID: &snapshot, SourceReserve: "source", IntendedTargetReserve: "intended", ActiveTargetReserve: "fallback", SourceMint: "mint-a", TargetMint: "mint-b", PlannedAmountRaw: 990, ExecutionPlan: json.RawMessage(`{"plan":1}`), PreflightCertification: json.RawMessage(`{"cert":2}`), CustodyMint: "mint-b", CustodyAccount: "aggregate", CustodyAmountRaw: 990, CustodyObservedBalanceRaw: &amount, CustodyReconciledSlot: &slot, CustodyVersion: 7, Phase: CrossMintTargetIdle, TerminalOutcome: &terminal}
	q := CrossMintLegRequest{Movement: m, Leg: LegDeposit, Purpose: PurposeFallbackTarget, Generation: 9, RemainingFeeLamports: 10000, ContinuationOwner: "actual-d-owner", ContinuationFencingToken: 10, ControlGeneration: 0, ExpiresAt: time.Now().Add(time.Minute)}
	got := revalidatorCrossMintLegRequest(q)
	if got.ContinuationOwner != q.ContinuationOwner || got.ContinuationFencingToken != q.ContinuationFencingToken || got.ControlGeneration != 0 || !got.ExpiresAt.Equal(q.ExpiresAt) || got.Generation != q.Generation || got.RemainingFeeLamports != q.RemainingFeeLamports || got.Leg != q.Leg || got.Purpose != q.Purpose {
		t.Fatalf("changed actual lease authority: %+v", got)
	}
	encodedD, _ := json.Marshal(m)
	encodedC, _ := json.Marshal(got.Movement)
	if !bytes.Equal(encodedD, encodedC) {
		t.Fatalf("movement field lost or changed:\n%s\n%s", encodedD, encodedC)
	}
	m.ExecutionPlan[0], m.PreflightCertification[0] = 'x', 'x'
	if !json.Valid(got.Movement.ExecutionPlan) || !json.Valid(got.Movement.PreflightCertification) {
		t.Fatal("source request aliases mutable input evidence bytes")
	}
}

func revalidatorCrossMintLegFixture() fleet.CrossMintLegPreparation {
	return fleet.CrossMintLegPreparation{
		Preparation: fleet.RoutePreparation{Transaction: fleet.PreparedTransaction{FeeLamports: 5000}}, LastValidBlockHeight: 900, PolicyAccount: "policy",
		ExpectedEffect: json.RawMessage(`{"debit":{"mint":"source","tokenAccount":"debit","amountRaw":10},"creditMint":"target","creditTokenAccount":"credit","minimumCreditAmountRaw":9}`),
		BalanceAnchors: json.RawMessage(`{"debit":{"mint":"source","tokenAccount":"debit","amountRaw":99},"credit":{"mint":"target","tokenAccount":"credit","amountRaw":7},"kaminoPosition":null}`),
		ConflictKeys:   []string{"vault-write:1", "fleet-shared-write-lane:01"}, AltSelectionFingerprint: "exact-selection",
		SelectedALTs:    []fleet.ExecutionALT{{TableID: 2, FamilyID: 3, Generation: 0, MutationEpoch: 0, Address: "table-b", Addresses: []string{"member-b", "member-a"}}, {TableID: 1, FamilyID: 4, Address: "table-a", Addresses: []string{"member-c"}}},
		SharedAddresses: []fleet.ALTManifestAddress{{Address: "shared", SemanticClass: "shared_market", AccountRole: "market", Ordinal: 0}},
		VaultAddresses:  []fleet.ALTManifestAddress{{Address: "vault", SemanticClass: "vault", AccountRole: "vault_token_account", Ordinal: 0, Writable: true}},
	}
}

func TestRevalidatorCrossMintPreparationPreservesOrderedALTAndExactContracts(t *testing.T) {
	p := revalidatorCrossMintLegFixture()
	got, err := revalidatorCrossMintPreparedLeg(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SelectedALTs, p.SelectedALTs) || !reflect.DeepEqual(got.ConflictKeys, p.ConflictKeys) || got.AltSelectionFingerprint != p.AltSelectionFingerprint || got.ExpectedEffect.Debit.AmountRaw != 10 || got.BalanceAnchors.Debit.AmountRaw != 99 || *got.ExpectedEffect.MinimumCreditAmountRaw != 9 || got.BalanceAnchors.Credit.AmountRaw != 7 || !got.VaultAddresses[0].Writable || got.SharedAddresses[0].SemanticClass != "shared_market" {
		t.Fatalf("contract mapping lost delta, aggregate, access or ALT order: %+v", got)
	}
	p.WaitingALT, p.ExpectedEffect, p.BalanceAnchors = true, nil, nil
	p.MissingAddresses = []string{"vault"}
	got, err = revalidatorCrossMintPreparedLeg(p)
	if err != nil || !got.WaitingALT || got.ExpectedEffect.Debit != nil || got.BalanceAnchors.Debit != nil || !reflect.DeepEqual(got.MissingAddresses, p.MissingAddresses) {
		t.Fatalf("unsigned waiting demand was promoted to proof: %+v %v", got, err)
	}
}

func TestRevalidatorCrossMintPreparationRejectsMalformedReceiptContracts(t *testing.T) {
	for name, mutate := range map[string]func(*fleet.CrossMintLegPreparation){
		"null":            func(p *fleet.CrossMintLegPreparation) { p.ExpectedEffect = json.RawMessage(`null`) },
		"generic":         func(p *fleet.CrossMintLegPreparation) { p.ExpectedEffect = json.RawMessage(`{}`) },
		"unknown field":   func(p *fleet.CrossMintLegPreparation) { p.ExpectedEffect = json.RawMessage(`{"trusted":true}`) },
		"trailing object": func(p *fleet.CrossMintLegPreparation) { p.ExpectedEffect = append(p.ExpectedEffect, []byte(` {}`)...) },
		"overflow": func(p *fleet.CrossMintLegPreparation) {
			p.ExpectedEffect = bytes.Replace(p.ExpectedEffect, []byte(`"amountRaw":10`), []byte(`"amountRaw":9223372036854775808`), 1)
		},
		"negative": func(p *fleet.CrossMintLegPreparation) {
			p.BalanceAnchors = bytes.Replace(p.BalanceAnchors, []byte(`"amountRaw":99`), []byte(`"amountRaw":-1`), 1)
		},
		"unfunded": func(p *fleet.CrossMintLegPreparation) {
			p.BalanceAnchors = bytes.Replace(p.BalanceAnchors, []byte(`"amountRaw":99`), []byte(`"amountRaw":9`), 1)
		},
		"mismatched account": func(p *fleet.CrossMintLegPreparation) {
			p.BalanceAnchors = bytes.Replace(p.BalanceAnchors, []byte(`"tokenAccount":"credit"`), []byte(`"tokenAccount":"other"`), 1)
		},
		"missing minimum": func(p *fleet.CrossMintLegPreparation) {
			p.ExpectedEffect = bytes.Replace(p.ExpectedEffect, []byte(`"minimumCreditAmountRaw":9`), []byte(`"minimumCreditAmountRaw":null`), 1)
		},
		"fee overflow": func(p *fleet.CrossMintLegPreparation) { p.Preparation.Transaction.FeeLamports = math.MaxUint64 },
	} {
		t.Run(name, func(t *testing.T) {
			p := revalidatorCrossMintLegFixture()
			mutate(&p)
			if _, err := revalidatorCrossMintPreparedLeg(p); err == nil {
				t.Fatal("malformed source contract accepted")
			}
		})
	}
}

// This is a source-shape fixture with synthetic readback/simulation hashes.
// It verifies conversion boundaries, and provides no chain or SVM evidence.
func revalidatorCrossMintActivationFixture(t *testing.T) fleet.CrossMintActivationPreparation {
	t.Helper()
	key := func(n byte) string { return solana.PublicKeyFromBytes(bytes.Repeat([]byte{n}, 32)).String() }
	settings := solana.MustPublicKeyFromBase58(key(1))
	swapKey, _, err := squads.PolicyAddress(settings, 3)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	b := fleet.CrossMintPolicyBindings{Settings: settings.String(), VaultPubkey: key(2), Withdraw: fleet.CrossMintEarnPolicyBinding{PolicyAccount: key(3), ObservedSlot: 100, SourceCommitment: "finalized"}, Deposit: fleet.CrossMintEarnPolicyBinding{PolicyAccount: key(4), ObservedSlot: 100, SourceCommitment: "finalized"}, Swap: fleet.CrossMintSwapPolicyBinding{PolicyAccount: swapKey.String(), ObservedSlot: 100, SourceCommitment: "finalized", SourceShard: "classic", MaxSlippageBPS: 50, DailySourceMintSpendingCap: 9999, ManifestFingerprint: hash}}
	plan, _ := json.Marshal(map[string]any{"kind": "cross_mint_jupiter", "source_liquidity_mint": fleet.USDCMint, "target_liquidity_mint": fleet.USDTMint, "amount_raw": 999, "redeemable_source_liquidity_amount_raw": 999, "cross_mint_maximum_value_loss_bps": 50, "policy_bindings": b})
	l := fleet.RevalidationLease{OpportunityID: 1, OptimizerEpochID: 2, VaultID: 3, Owner: "actual-d-owner", FencingToken: 4, ExpiresAt: time.Now().Add(time.Minute), Cluster: "localnet", VaultPubkey: key(2), SourceReserve: key(5), TargetReserve: key(6), SourceLiquidityMint: fleet.USDCMint, TargetLiquidityMint: fleet.USDTMint, LiquidityAmountRaw: 999, FeeCapLamports: 50000, ExecutionPlan: plan}
	policy := func(account string) fleet.CrossMintCertificatePolicy {
		return fleet.CrossMintCertificatePolicy{PolicyAccount: account, ContextSlot: 110, DataSHA256: hash}
	}
	c := fleet.CrossMintPreflightCertificate{Kind: "cross_mint_preflight", CertifiedAt: time.Now(), Cluster: l.Cluster, SourceMint: l.SourceLiquidityMint, TargetMint: l.TargetLiquidityMint, InputAmountRaw: "999", MinimumOutputAmountRaw: "998", EffectiveSlippageBPS: 1, EffectiveMaximumValueLossBPS: 50, FinalizedPolicyReadbacks: fleet.CrossMintCertificatePolicies{Withdraw: policy(b.Withdraw.PolicyAccount), Deposit: policy(b.Deposit.PolicyAccount), Swap: fleet.CrossMintCertificateSwapPolicy{CrossMintCertificatePolicy: policy(b.Swap.PolicyAccount), PolicySeed: "3", SourceShard: "classic", ManifestFingerprint: hash, Dialect: "route_v2", DailySourceMintSpendingCap: "9999"}}, JupiterBuild: fleet.CrossMintCertificateJupiter{ResponseSHA256: hash, RouteStepCount: 1, QuotedOutputAmountRaw: "999", LookupTables: []string{key(7)}, ComputeUnitLimit: 200000, PacketSizeBytes: 512, PacketDataSizeBytes: fleet.SolanaPacketLimit, FitsPacketDataSize: true, MessageSHA256: hash, LastValidBlockHeight: 500, ObservedBlockHeight: 400, InputPreBalanceRaw: "1", OutputPreBalanceRaw: "0", SimulationAttempted: true, SimulationUnits: 100000, SimulationTopology: "withdraw_then_swap_atomic_preflight_only", SimulationLookupTables: []string{key(7)}, TargetDepositPolicyValidated: true, TargetReserve: l.TargetReserve, TargetObligation: key(8)}}
	return fleet.CrossMintActivationPreparation{Lease: l, Certificate: c, InitialWithdrawalPreparation: fleet.CrossMintLegPreparation{Preparation: fleet.RoutePreparation{Transaction: fleet.PreparedTransaction{FeeLamports: 5000}}}, ObservedAt: time.Now(), ObservedSlot: 110, TargetObservedSupplyUSDMicros: 9000000, Capacity: fleet.CrossMintActivationCapacity{Cluster: l.Cluster, TargetReserve: l.TargetReserve, LiquidityMint: l.TargetLiquidityMint, ObservedSupplyUSDMicros: 9000000, ObservedSlot: 110, MaximumInflightUSDMicros: 4000000, TelemetryVersion: 0}}
}

func TestRevalidatorCrossMintActivationBindsConcreteFrontierAndSourceCertificate(t *testing.T) {
	p := revalidatorCrossMintActivationFixture(t)
	got, err := revalidatorCrossMintActivationAdmission(p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expectedCertificate, err := json.Marshal(p.Certificate)
	if err != nil || !bytes.Equal(got.Activation.PreflightCertification, expectedCertificate) || !reflect.DeepEqual(got.Lease, p.Lease) || got.Activation.SourceControlGeneration != p.ControlGeneration || got.Activation.Capacity.TelemetryVersion != 0 || got.Activation.Capacity.ObservedSlot != p.Capacity.ObservedSlot || got.Activation.Capacity.MaximumInflightUSDMicros != p.Capacity.MaximumInflightUSDMicros || got.Activation.InitialWithdrawCompiledFeeLamports != 5000 {
		t.Fatal("activation lost source certificate, zero generation or concrete capacity")
	}
	for name, mutate := range map[string]func(*fleet.CrossMintActivationPreparation){
		"waiting": func(p *fleet.CrossMintActivationPreparation) { p.WaitingALT = true },
		"fee overflow": func(p *fleet.CrossMintActivationPreparation) {
			p.InitialWithdrawalPreparation.Preparation.Transaction.FeeLamports = math.MaxUint64
		},
		"amount overflow":             func(p *fleet.CrossMintActivationPreparation) { p.Lease.LiquidityAmountRaw = math.MaxUint64 },
		"expired":                     func(p *fleet.CrossMintActivationPreparation) { p.Lease.ExpiresAt = time.Now().Add(4 * time.Second) },
		"old bank":                    func(p *fleet.CrossMintActivationPreparation) { p.ObservedAt = time.Now().Add(-16 * time.Second) },
		"different supply":            func(p *fleet.CrossMintActivationPreparation) { p.Capacity.ObservedSupplyUSDMicros++ },
		"different slot":              func(p *fleet.CrossMintActivationPreparation) { p.Capacity.ObservedSlot++ },
		"different mint":              func(p *fleet.CrossMintActivationPreparation) { p.Capacity.LiquidityMint = fleet.USDCMint },
		"negative control generation": func(p *fleet.CrossMintActivationPreparation) { p.ControlGeneration = -1 },
		"generic cert":                func(p *fleet.CrossMintActivationPreparation) { p.Certificate = fleet.CrossMintPreflightCertificate{} },
	} {
		t.Run(name, func(t *testing.T) {
			bad := p
			mutate(&bad)
			if _, err := revalidatorCrossMintActivationAdmission(bad, time.Now()); err == nil {
				t.Fatal("invalid activation mapped to executable admission")
			}
		})
	}
}

func TestRevalidatorCrossMintActivationPreservesCapturedControlGeneration(t *testing.T) {
	// D compares this captured generation with its separately locked current
	// control. The adapter cannot substitute newer activation authority.
	for _, generation := range []int64{0, 1, 9, math.MaxInt64} {
		p := revalidatorCrossMintActivationFixture(t)
		p.ControlGeneration = generation
		got, err := revalidatorCrossMintActivationAdmission(p, time.Now())
		if err != nil || got.Activation.SourceControlGeneration != generation {
			t.Fatalf("captured source generation changed: %d %+v %v", generation, got, err)
		}
	}
}

func TestRevalidatorCrossMintFirstSendPreservesExactPersistedWireAndLookupOrder(t *testing.T) {
	m := CrossMintMovement{DecisionID: 1, OpportunityID: 2, Cluster: "localnet"}
	q := CrossMintFirstSendRequest{Movement: m, Submission: SubmissionRecord{DecisionID: &m.DecisionID, OpportunityID: m.OpportunityID, Cluster: m.Cluster, SignedTransaction: []byte{1, 2, 3}, MessageHash: "message-sha", Signature: "signature", RecentBlockhash: "hash", LastValidBlockHeight: 456, MovementLeg: LegSwap, LegPurpose: PurposeOptimizeYield}, ExpectedWireSHA256: "durable-wire-sha", PolicyAccount: "policy", MinimumSlot: 789, SelectedALTs: revalidatorCrossMintLegFixture().SelectedALTs}
	got, err := revalidatorCrossMintFirstSendRequest(q)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.SignedWire, q.Submission.SignedTransaction) || got.ExpectedWireSHA256 != q.ExpectedWireSHA256 || got.ExpectedMessageSHA256 != q.Submission.MessageHash || got.Signature != q.Submission.Signature || got.RecentBlockhash != q.Submission.RecentBlockhash || got.LastValidBlockHeight != q.Submission.LastValidBlockHeight || got.Leg != q.Submission.MovementLeg || got.Purpose != q.Submission.LegPurpose || got.MinimumSlot != q.MinimumSlot || !reflect.DeepEqual(got.SelectedALTs, q.SelectedALTs) {
		t.Fatal("first-send reconstructed or changed durable evidence")
	}
	q.Submission.SignedTransaction[0] = 99
	if got.SignedWire[0] != 1 {
		t.Fatal("first-send source aliases mutable signed bytes")
	}
	q.Submission.DecisionID = nil
	if _, err := revalidatorCrossMintFirstSendRequest(q); err == nil {
		t.Fatal("unbound journal accepted")
	}
	if _, err := NewRevalidatorCrossMint(nil); err == nil {
		t.Fatal("missing concrete source accepted")
	}
}

func TestRevalidatorCrossMintOnlyValidatedSwapShortfallPermitsRecovery(t *testing.T) {
	ctx := context.Background()
	if !errors.Is(revalidatorCrossMintLegError(ctx, LegSwap, fleet.ErrCrossMintQuoteUnavailable), ErrCrossMintSwapUnavailable) {
		t.Fatal("validated quote shortfall did not reach source recovery")
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("provider failure"), errors.Join(fleet.ErrCrossMintQuoteUnavailable, context.DeadlineExceeded)} {
		if got := revalidatorCrossMintLegError(ctx, LegSwap, err); got != err || errors.Is(got, ErrCrossMintSwapUnavailable) {
			t.Fatal("uncertain failure was converted to alternative custody spend")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if errors.Is(revalidatorCrossMintLegError(canceled, LegSwap, fleet.ErrCrossMintQuoteUnavailable), ErrCrossMintSwapUnavailable) || errors.Is(revalidatorCrossMintLegError(ctx, LegDeposit, fleet.ErrCrossMintQuoteUnavailable), ErrCrossMintSwapUnavailable) {
		t.Fatal("cancellation or unrelated leg selected recovery")
	}
}
