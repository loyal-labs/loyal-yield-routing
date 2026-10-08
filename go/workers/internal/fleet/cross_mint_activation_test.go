package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func activationLeaseFixture(t *testing.T) (RevalidationLease, crossMintPlan, crossMintPreparationBank) {
	t.Helper()
	q, plan, bank := crossMintPreparationFixture(t)
	return RevalidationLease{OpportunityID: q.Movement.OpportunityID, OptimizerEpochID: q.Movement.OptimizerEpochID, Cluster: q.Movement.Cluster, Owner: q.ContinuationOwner, FencingToken: q.ContinuationFencingToken, ExpiresAt: q.ExpiresAt, VaultID: q.Movement.VaultID, VaultPubkey: q.Movement.VaultPubkey, PolicyAccount: plan.Bindings.Withdraw.PolicyAccount, DelegatedSigners: []string{plan.Bindings.DelegatedSigner}, SourceReserve: q.Movement.SourceReserve, TargetReserve: q.Movement.IntendedTargetReserve, SourceLiquidityMint: q.Movement.SourceMint, TargetLiquidityMint: q.Movement.TargetMint, LiquidityMint: q.Movement.TargetMint, RouteKind: plan.Kind, LiquidityAmountRaw: uint64(q.Movement.PlannedAmountRaw), SourceCollateralRaw: 999, FeeCapLamports: q.RemainingFeeLamports, ExecutionPlan: bytes.Clone(q.Movement.ExecutionPlan)}, plan, bank
}

// This fixture tests source certificate serialization from supplied observations;
// it does not represent a Solana simulation or a finalized policy readback.
func TestCrossMintSourceCertificateBindsObservedBankAndExactVerifier(t *testing.T) {
	l, plan, bank := activationLeaseFixture(t)
	for _, address := range []string{plan.Bindings.Withdraw.PolicyAccount, plan.Bindings.Swap.PolicyAccount, plan.Bindings.Deposit.PolicyAccount} {
		bank.accounts[address] = Account{Address: address, Owner: SquadsProgram, Lamports: 1, Data: []byte(address)}
	}
	tx := PreparedTransaction{MessageSHA256: hex.EncodeToString(bytes.Repeat([]byte{2}, 32)), WireSHA256: hex.EncodeToString(bytes.Repeat([]byte{3}, 32)), PacketBytes: 512, ComputeLimit: 200_000, LookupTables: []string{testPubkey(111)}}
	build := validatedJupiterBuild{ResponseSHA256: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), RouteSteps: 1, QuotedOutput: 999, MinimumOutput: 998, Slippage: 1, Dialect: "route_v2", LastValidBlockHeight: 2000, ObservedBlockHeight: 1900}
	sim := SimulationEvidence{Succeeded: true, Slot: bank.slot, UnitsConsumed: 100_000, WireSHA256: tx.WireSHA256}
	c, err := crossMintSourceCertificate(l, plan, bank, build, DecodedSquadsPolicy{PolicySeed: 3}, 50, tx, sim, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(bank.accounts[plan.Bindings.Withdraw.PolicyAccount].Data)
	if c.FinalizedPolicyReadbacks.Withdraw.DataSHA256 != hex.EncodeToString(want[:]) || c.FinalizedPolicyReadbacks.Withdraw.ContextSlot != bank.slot || c.JupiterBuild.TargetObligation != bank.target.Obligation || c.JupiterBuild.MessageSHA256 != tx.MessageSHA256 || c.JupiterBuild.SimulationTopology != "withdraw_then_swap_atomic_preflight_only" {
		t.Fatalf("certificate dropped actual source observations: %+v", c)
	}
	raw, _ := json.Marshal(c)
	m := crossMintActivationRequest(l).Movement
	m.PreflightCertification = raw
	if _, err := ValidateCrossMintPreflightCertificate(m, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SimulationEvidence){
		"failed":         func(s *SimulationEvidence) { s.Succeeded = false },
		"different wire": func(s *SimulationEvidence) { s.WireSHA256 = tx.MessageSHA256 },
		"different bank": func(s *SimulationEvidence) { s.Slot = bank.slot - 1 },
		"missing units":  func(s *SimulationEvidence) { s.UnitsConsumed = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := sim
			change(&bad)
			if _, err := crossMintSourceCertificate(l, plan, bank, build, DecodedSquadsPolicy{PolicySeed: 3}, 50, tx, bad, nil); err == nil {
				t.Fatal("accepted verifier evidence mismatch")
			}
		})
	}
	build.LastValidBlockHeight = math.MaxUint64
	if _, err := crossMintSourceCertificate(l, plan, bank, build, DecodedSquadsPolicy{PolicySeed: 3}, 50, tx, sim, nil); err == nil {
		t.Fatal("accepted SQL-range overflow")
	}
}

type activationAuthorityStore struct {
	revalidationStore
	check func(context.Context, RevalidationLease) (int64, error)
}

func (s activationAuthorityStore) CheckCrossMintActivationLease(ctx context.Context, l RevalidationLease) (int64, error) {
	return s.check(ctx, l)
}

func TestCrossMintActivationUsesActualLeaseDeadlineBeforeProviders(t *testing.T) {
	l, plan, _ := activationLeaseFixture(t)
	l.ExpiresAt = time.Now().Add(6 * time.Second)
	stale := errors.New("source execute authority revoked")
	var calls int
	r := &Revalidator{owner: l.Owner, signer: plan.Bindings.DelegatedSigner, crossMintEnabled: true, jupiter: &JupiterBuildClient{}, store: activationAuthorityStore{check: func(ctx context.Context, got RevalidationLease) (int64, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(l.ExpiresAt.Add(-5*time.Second)) || got.FencingToken != l.FencingToken || !bytes.Equal(got.ExecutionPlan, l.ExecutionPlan) {
			t.Fatal("preflight used a manufactured deadline or plan")
		}
		return 0, stale
	}}}
	if _, err := r.PrepareCrossMintActivation(context.Background(), l); !errors.Is(err, stale) || calls != 1 {
		t.Fatalf("revoked authority reached providers: calls=%d err=%v", calls, err)
	}
	l.Owner = "another-worker"
	if _, err := r.PrepareCrossMintActivation(context.Background(), l); err == nil || calls != 1 {
		t.Fatal("foreign lease reached source store")
	}
}
