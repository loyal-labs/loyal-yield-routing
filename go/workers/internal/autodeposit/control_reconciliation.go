package autodeposit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
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
	expectedVault, err := findProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {1}}, squadsProgramID)
	if err != nil || base58Key(expectedVault[:]) != target.Vault {
		return o, errors.New("control vault differs from canonical settings PDA")
	}
	for _, binding := range []struct {
		owner   solana.PublicKey
		account string
	}{{wallet, target.WalletTokenATA}, {vault, target.VaultTokenATA}} {
		ata, err := deriveVaultATA(binding.owner, mint, mustKey(splTokenID))
		if err != nil || ata != binding.account {
			return o, errors.New("control token account differs from canonical ATA")
		}
	}
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(target.PolicySeed))
	policy, err := findProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], seed[:]}, squadsProgramID)
	if err != nil || base58Key(policy[:]) != target.Policy {
		return o, errors.New("control policy differs from canonical seed PDA")
	}
	addresses := []string{target.Policy, target.SubscriptionAuthority, target.RecurringDelegation, target.WalletTokenATA}
	slot, accounts, err := b.read(ctx, addresses, addresses...)
	if err != nil {
		return o, err
	}
	if slot <= 0 || slot < minimumSlot || len(accounts) != 4 {
		return o, errors.New("control observation is incomplete or behind requested slot")
	}
	o.ObservedSlot = slot
	for i, a := range accounts {
		if a.Address != addresses[i] || a.Executable {
			return o, errors.New("control account vector identity or executable flag invalid")
		}
	}
	o.PolicyExists = accounts[0].Owner != ""
	o.DelegationExists = accounts[2].Owner != ""
	if o.PolicyExists {
		if accounts[0].Owner != squadsProgramID {
			return o, errors.New("control policy has foreign owner")
		}
		decoded, err := fleet.DecodeSquadsPolicy(accounts[0].Data)
		if err != nil {
			return o, err
		}
		_, bump, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], seed[:]}, mustKey(squadsProgramID))
		if err != nil || decoded.Bump != bump || decoded.PolicySeed != uint64(target.PolicySeed) || decoded.AccountIndex != 1 || decoded.Settings != target.Settings {
			return o, errors.New("control policy header differs from target")
		}
		event, err := subscriptionEventAuthorityKey()
		if err != nil {
			return o, err
		}
		data := make([]byte, 73)
		data[0] = subscriptionsTransferRecurring
		binary.LittleEndian.PutUint64(data[1:9], uint64(*target.MaxAmountPerPeriod))
		copy(data[9:41], wallet[:])
		copy(data[41:], mint[:])
		inner := fleet.RouteInstruction{Step: "autodeposit", Program: SubscriptionsProgramID, Data: data, Accounts: []fleet.InstructionAccount{
			{Address: target.RecurringDelegation, Writable: true}, {Address: target.SubscriptionAuthority},
			{Address: target.WalletTokenATA, Writable: true}, {Address: target.VaultTokenATA, Writable: true},
			{Address: target.Mint}, {Address: splTokenID}, {Address: target.Vault, Signer: true},
			{Address: base58Key(event[:])}, {Address: SubscriptionsProgramID},
		}}
		if _, err = fleet.BuildPolicyEnvelope(target.Policy, target.Settings, b.delegate.String(), accounts[0].Data, []fleet.RouteInstruction{inner}); err != nil {
			return o, err
		}
		o.PolicyValid = true
	}
	if accounts[1].Owner != "" {
		if accounts[1].Owner != SubscriptionsProgramID {
			return o, errors.New("control authority has foreign owner")
		}
		o.AuthorityValid = true
	}
	if o.DelegationExists {
		nonce := uint64(*target.Nonce)
		if _, err = RemainingDelegationAllowance(accounts[2].Owner, accounts[2].Data, DelegationIdentity{Account: target.RecurringDelegation, Delegator: target.Wallet, Delegatee: target.Vault, Mint: target.Mint, Nonce: &nonce}); err != nil {
			return o, err
		}
		if binary.LittleEndian.Uint64(accounts[2].Data[delegationPerPeriodOffset:]) != uint64(*target.MaxAmountPerPeriod) {
			return o, errors.New("control delegation budget differs from target")
		}
		o.DelegationValid = true
	}
	if accounts[3].Owner != "" {
		if err := validateVaultUSDCATA(accounts[3], target.WalletTokenATA, target.Wallet); err != nil {
			return o, err
		}
		if accounts[3].Data[108] != 1 {
			return o, errors.New("control wallet token account is not initialized")
		}
		amount := binary.LittleEndian.Uint64(accounts[3].Data[64:72])
		if amount > 1<<63-1 {
			return o, errors.New("control wallet balance exceeds int64")
		}
		o.WalletBalanceRaw = int64(amount)
		hash := sha256.Sum256(accounts[3].Data)
		o.WalletAccountDataSHA256 = hex.EncodeToString(hash[:])
		o.TokenDelegateValid = binary.LittleEndian.Uint32(accounts[3].Data[72:76]) == 1 && base58Key(accounts[3].Data[76:108]) == target.SubscriptionAuthority
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
		if err != nil && ctx.Err() == nil {
			log.Print("autodeposit control_cycle_failed")
			if r.OnError != nil {
				r.OnError(errRuntimeProofUnavailable)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
