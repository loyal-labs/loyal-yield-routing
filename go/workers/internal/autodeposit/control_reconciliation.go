package autodeposit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// ControlTarget follows the single-target projection introduced by migration
// 0059. Missing wallet-authorized artifacts remain pending; this reconciler
// never creates a delegation or changes the user's desired enablement/floor.
type ControlTarget struct {
	Cluster                                                      string
	TargetID, SetupGeneration, PolicySeed                        int64
	Settings, Wallet, WalletTokenATA, Vault, VaultTokenATA, Mint string
	Policy, SubscriptionAuthority, RecurringDelegation           string
	Nonce, MaxAmountPerPeriod, StartTimestamp                    *int64
}

type ControlObservation struct {
	Target                                                           ControlTarget
	ObservedSlot                                                     int64
	PolicyExists, DelegationExists                                   bool
	PolicyValid, AuthorityValid, DelegationValid, TokenDelegateValid bool
	WalletBalanceRaw                                                 int64
	WalletAccountDataSHA256                                          string
}

func (o ControlObservation) status() string {
	if o.PolicyValid && o.AuthorityValid && o.DelegationValid && o.TokenDelegateValid {
		return "active"
	}
	if !o.PolicyExists && !o.DelegationExists {
		return "closed"
	}
	return "inconsistent"
}

// ObserveControl uses the same full policy constraint matcher as a real pull,
// plus the official delegation identity/PDA and SPL token-delegate layouts.
// Unlike the original owner-only monitor, malformed artifacts cannot activate
// a target. All four accounts come from one confirmed RPC context.
func (b *SweepWireBuilder) ObserveControl(ctx context.Context, target ControlTarget, minimumSlot int64) (ControlObservation, error) {
	o := ControlObservation{Target: target}
	if target.Cluster != mainnetCluster {
		return o, ErrChainNamespace
	}
	if target.Nonce == nil || *target.Nonce < 0 || target.MaxAmountPerPeriod == nil || *target.MaxAmountPerPeriod <= 0 {
		return o, errors.New("control target lacks confirmed delegation nonce or budget")
	}
	for _, value := range []string{target.Settings, target.Wallet, target.WalletTokenATA, target.Vault, target.VaultTokenATA, target.Policy, target.SubscriptionAuthority, target.RecurringDelegation} {
		if _, err := solana.PublicKeyFromBase58(value); err != nil {
			return o, err
		}
	}
	if target.Mint != USDCMint || target.PolicySeed <= 0 {
		return o, errors.New("control target has unsupported mint or policy seed")
	}
	wallet, _ := solana.PublicKeyFromBase58(target.Wallet)
	vault, _ := solana.PublicKeyFromBase58(target.Vault)
	mint, _ := solana.PublicKeyFromBase58(target.Mint)
	authority, err := subscriptionAuthorityKey(wallet[:], mint[:])
	if err != nil {
		return o, err
	}
	if base58Key(authority[:]) != target.SubscriptionAuthority {
		return o, errors.New("control subscription authority differs from canonical PDA")
	}
	delegation, err := delegationAccountKey(authority[:], wallet[:], vault[:], uint64(*target.Nonce))
	if err != nil {
		return o, err
	}
	if base58Key(delegation[:]) != target.RecurringDelegation {
		return o, errors.New("control delegation differs from canonical nonce PDA")
	}
	settings, _ := solana.PublicKeyFromBase58(target.Settings)
	// Loyal smart-accounts core spec/pda-registry.ts, smartAccount index 1.
	expectedVault, _, err := squads.SmartAccountAddress(settings, 1)
	if err != nil || expectedVault.String() != target.Vault {
		return o, errors.New("control vault differs from canonical settings PDA")
	}
	for _, binding := range []struct {
		owner   solana.PublicKey
		account string
	}{{wallet, target.WalletTokenATA}, {vault, target.VaultTokenATA}} {
		ata, err := spl.AssociatedTokenAddress(binding.owner, mustKey(USDCMint), solana.TokenProgramID)
		if err != nil || ata.String() != binding.account {
			return o, errors.New("control token account differs from canonical ATA")
		}
	}
	policy, policyBump, err := squads.PolicyAddress(settings, uint64(target.PolicySeed))
	if err != nil || policy.String() != target.Policy {
		return o, errors.New("control policy differs from canonical seed PDA")
	}
	addresses := []string{target.Policy, target.SubscriptionAuthority, target.RecurringDelegation, target.WalletTokenATA}
	slot, accounts, err := b.read(ctx, minimumSlot, addresses, addresses...)
	if err != nil {
		return o, err
	}
	o.ObservedSlot = slot
	for _, a := range accounts {
		if a != nil && a.Executable {
			return o, errors.New("control account is executable")
		}
	}
	o.PolicyExists = accounts[0] != nil
	o.DelegationExists = accounts[2] != nil
	if o.PolicyExists {
		if accounts[0].Owner != squads.ProgramID {
			return o, errors.New("control policy has foreign owner")
		}
		decoded, err := fleet.DecodeSquadsPolicy(accounts[0])
		if err != nil {
			return o, err
		}
		if decoded.Bump != policyBump || decoded.PolicySeed != uint64(target.PolicySeed) || decoded.AccountIndex != 1 || decoded.Settings != target.Settings {
			return o, errors.New("control policy header differs from target")
		}
		inner, err := transferRecurring(uint64(*target.MaxAmountPerPeriod), wallet, vault, mint, target.RecurringDelegation, target.WalletTokenATA, target.VaultTokenATA)
		if err != nil {
			return o, err
		}
		if _, err = fleet.BuildPolicyEnvelope(target.Policy, target.Settings, b.delegate.String(), accounts[0], []fleet.RouteInstruction{inner}); err != nil {
			return o, err
		}
		o.PolicyValid = true
	}
	if accounts[1] != nil {
		if accounts[1].Owner.String() != SubscriptionsProgramID {
			return o, errors.New("control authority has foreign owner")
		}
		o.AuthorityValid = true
	}
	if o.DelegationExists {
		nonce := uint64(*target.Nonce)
		if _, err = RemainingDelegationAllowance(accounts[2].Owner.String(), accounts[2].Data, DelegationIdentity{Account: target.RecurringDelegation, Delegator: target.Wallet, Delegatee: target.Vault, Mint: target.Mint, Nonce: &nonce}); err != nil {
			return o, err
		}
		if binary.LittleEndian.Uint64(accounts[2].Data[delegationPerPeriodOffset:]) != uint64(*target.MaxAmountPerPeriod) {
			return o, errors.New("control delegation budget differs from target")
		}
		o.DelegationValid = true
	}
	if accounts[3] != nil {
		held, err := usdcTokenAccount(accounts[3], target.Wallet)
		if err != nil {
			return o, err
		}
		o.WalletBalanceRaw = int64(held.Amount)
		hash := sha256.Sum256(accounts[3].Data)
		o.WalletAccountDataSHA256 = hex.EncodeToString(hash[:])
		o.TokenDelegateValid = held.Delegate != nil && held.Delegate.String() == target.SubscriptionAuthority
	}
	return o, nil
}

type ControlReader interface {
	ObserveControl(context.Context, ControlTarget, int64) (ControlObservation, error)
}

// ControlReconciler consumes the existing coalesced high-water outbox. One
// bounded Tick owns one existing lease; cancellation never invents a retry
// transaction. Run is composed by the root runtime alongside the same family.
// ControlArtifactReconciler verifies creator history under the same outbox
// ownership before current account state can acknowledge a reconciliation ask.
type ControlArtifactReconciler interface {
	ReconcileArtifacts(context.Context, ReconciliationRequest, string) error
}

type ControlReconciler struct {
	Store                       *Store
	Reader                      ControlReader
	Artifacts                   ControlArtifactReconciler
	OnError                     func(error)
	PollInterval, LeaseDuration time.Duration
}

func (r *ControlReconciler) Tick(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.Store == nil || r.Reader == nil {
		return false, errors.New("control reconciler requires store and reader")
	}
	lease := r.LeaseDuration
	if lease <= 0 {
		lease = 120 * time.Second
	}
	if lease < time.Second {
		return false, errors.New("control reconciliation lease is shorter than one second")
	}
	owner, err := newClaimToken()
	if err != nil {
		return false, err
	}
	request, err := r.Store.ClaimAutodepositReconciliationRequest(ctx, owner, int64(lease/time.Second))
	if err != nil || request == nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, lease)
	defer cancel()
	target, err := r.Store.LoadControlTarget(work, request.TargetID)
	if err == nil && target == nil {
		return true, r.Store.AwaitAutodepositSetupReconciliationRequest(work, request.TargetID, owner, 3600)
	}
	if err == nil {
		var observation ControlObservation
		observation, err = r.Reader.ObserveControl(work, *target, request.RequestedSlot)
		if err == nil && observation.status() == "active" && r.Artifacts != nil {
			// Known close/cancel state does not require creation history.
			// Active state requires creator proof before acknowledgement.
			// Backfill changes creator metadata only; ApplyControlObservation
			// rechecks the complete generation and control binding under lock.
			err = r.Artifacts.ReconcileArtifacts(work, *request, owner)
		}
		if err == nil {
			err = r.Store.ApplyControlObservation(work, *request, owner, observation)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		// The proof deadline may have expired. Cleanup uses the still-live caller
		// context and its own short bound; an expired lease remains recoverable.
		retryCtx, retryCancel := context.WithTimeout(ctx, runtimeHealthTimeout)
		defer retryCancel()
		retryErr := r.Store.RetryAutodepositReconciliationRequest(retryCtx, request.TargetID, owner, "control reconciliation proof unavailable", ReconciliationRetryBackoffSeconds(15, request.AttemptCount))
		return true, errors.Join(fmt.Errorf("control reconciliation target %d: %w", request.TargetID, err), retryErr)
	}
	return true, nil
}

func (r *ControlReconciler) Run(ctx context.Context) error {
	if r.Store == nil || r.Reader == nil || (r.LeaseDuration > 0 && r.LeaseDuration < time.Second) {
		return errors.New("autodeposit control runtime dependencies or lease invalid")
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, runtimeCycleTimeout)
		_, err := r.Tick(cycle)
		cancel()
		if err != nil && ctx.Err() == nil && r.OnError != nil {
			r.OnError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
