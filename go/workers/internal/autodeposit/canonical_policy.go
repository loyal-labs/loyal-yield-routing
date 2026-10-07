package autodeposit

import (
	"errors"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

// CanonicalSubscriptionPolicyRequest names the deployed Autodeposit policy:
// Loyal Apps 45de590 smart-account-vaults/client.ts:1894..2004, ported from
// loyal-actions squads.rs build_canonical_subscription_sweep_policy_create.
// Its mutable wallet floor is deliberately absent.
type CanonicalSubscriptionPolicyRequest struct {
	Settings, RootAuthority, Payer, DelegatedSigner string
	PolicySeed                                      uint64
	Wallet, Vault                                   string
	MaxAmountPerPeriod                              uint64
}

type canonicalSubscriptionPolicy struct {
	settings, policy, delegate solana.PublicKey
	constraints                []squadspolicy.InstructionConstraintView
}

func (r CanonicalSubscriptionPolicyRequest) resolve() (canonicalSubscriptionPolicy, error) {
	var out canonicalSubscriptionPolicy
	var keys [6]solana.PublicKey
	for i, value := range []string{r.Settings, r.RootAuthority, r.Payer, r.DelegatedSigner, r.Wallet, r.Vault} {
		key, err := solana.PublicKeyFromBase58(value)
		if err != nil {
			return out, err
		}
		keys[i] = key
	}
	settings, wallet, vault := keys[0], keys[4], keys[5]
	expectedVault, err := findProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {1}}, squadsProgramID)
	if err != nil {
		return out, err
	}
	if r.PolicySeed == 0 || r.MaxAmountPerPeriod == 0 || vault != solana.PublicKey(expectedVault) {
		return out, errors.New("invalid canonical subscription policy constraint")
	}
	usdc := solana.MustPublicKeyFromBase58(USDCMint)
	subscriptions := solana.MustPublicKeyFromBase58(SubscriptionsProgramID)
	spl := solana.TokenProgramID
	authority, err := subscriptionAuthorityKey(wallet[:], usdc[:])
	if err != nil {
		return out, err
	}
	event, err := subscriptionEventAuthorityKey()
	if err != nil {
		return out, err
	}
	walletATA, _, err := solana.FindAssociatedTokenAddress(wallet, usdc)
	if err != nil {
		return out, err
	}
	vaultATA, _, err := solana.FindAssociatedTokenAddress(vault, usdc)
	if err != nil {
		return out, err
	}
	out.policy, _, err = squadspolicy.ActionAccount(settings, r.PolicySeed)
	if err != nil {
		return out, err
	}
	slice := func(offset uint64, key [32]byte) squadspolicy.DataConstraintView {
		return squadspolicy.DataConstraintView{DataOffset: offset, DataValue: squadspolicy.DataValueView{Kind: 5, Bytes: append([]byte(nil), key[:]...)}}
	}
	u8 := func(offset uint64, value uint8) squadspolicy.DataConstraintView {
		return squadspolicy.DataConstraintView{DataOffset: offset, DataValue: squadspolicy.DataValueView{Kind: 0, U8: value}}
	}
	pin := func(index uint8, key solana.PublicKey, owner *solana.PublicKey) squadspolicy.AccountConstraintView {
		return squadspolicy.AccountConstraintView{AccountIndex: index, Pubkeys: []solana.PublicKey{key}, Owner: owner}
	}
	out.settings, out.delegate = settings, keys[3]
	out.constraints = []squadspolicy.InstructionConstraintView{{
		ProgramID: subscriptions,
		AccountConstraints: []squadspolicy.AccountConstraintView{
			{AccountIndex: 0, Owner: &subscriptions, AccountData: []squadspolicy.DataConstraintView{
				u8(delegationDiscriminatorOffset, delegationDiscriminator),
				slice(delegationDelegatorOffset, wallet),
				slice(delegationDelegateeOffset, vault),
				slice(delegationAuthorityOffset, authority),
				slice(delegationMintOffset, usdc),
				{DataOffset: delegationPerPeriodOffset, DataValue: squadspolicy.DataValueView{Kind: 3, U64: r.MaxAmountPerPeriod}, Operator: squadspolicy.OpLessThanOrEqualTo},
			}},
			pin(1, authority, &subscriptions),
			pin(2, walletATA, &spl),
			pin(3, vaultATA, &spl),
			pin(4, usdc, &spl),
			pin(5, spl, nil),
			pin(6, vault, nil),
			pin(7, event, nil),
			pin(8, subscriptions, nil),
		},
		DataConstraints: []squadspolicy.DataConstraintView{
			u8(0, subscriptionsTransferRecurring),
			slice(subscriptionTransferDelegatorOffset, wallet),
			slice(subscriptionTransferMintOffset, usdc),
		},
	}}
	return out, nil
}

// BuildCanonicalSubscriptionPolicy builds the one PolicyCreate settings
// transaction that installs the canonical Autodeposit policy on vault index 1.
func BuildCanonicalSubscriptionPolicy(r CanonicalSubscriptionPolicyRequest) (fleet.RouteInstruction, error) {
	policy, err := r.resolve()
	if err != nil {
		return fleet.RouteInstruction{}, err
	}
	data, err := squadspolicy.EncodeLegacyPolicyCreate(r.PolicySeed, 1, policy.constraints, policy.delegate)
	if err != nil {
		return fleet.RouteInstruction{}, err
	}
	payer, _ := solana.PublicKeyFromBase58(r.Payer)
	root, _ := solana.PublicKeyFromBase58(r.RootAuthority)
	return fleet.RouteInstruction{Step: "canonical_subscription_policy_create", Program: squadsProgramID, Data: data, Accounts: []fleet.InstructionAccount{
		{Address: policy.settings.String(), Writable: true},
		{Address: payer.String(), Signer: true, Writable: true},
		{Address: solana.SystemProgramID.String()},
		{Address: squadsProgramID},
		{Address: root.String(), Signer: true},
		{Address: policy.policy.String(), Writable: true},
	}}, nil
}

// VerifyCanonicalSubscriptionPolicyAccount proves the current policy account
// has the complete canonical security properties and constraint matrix; only
// its transaction counters may have advanced since creation.
func VerifyCanonicalSubscriptionPolicyAccount(r CanonicalSubscriptionPolicyRequest, data []byte) error {
	policy, err := r.resolve()
	if err != nil {
		return err
	}
	current, err := squadspolicy.DecodeProgramInteractionPolicyAccount(data)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("current subscription policy lacks complete canonical security properties")
	}
	if current.Settings != policy.settings || current.PolicySeed != r.PolicySeed || current.PolicyAccount != policy.policy || current.DelegatedSigner != policy.delegate || current.Threshold != 1 {
		return errors.New("current subscription policy header differs from canonical target")
	}
	if current.Payload.VaultIndex != 1 || len(current.Payload.SpendingLimits) != 0 || !squadspolicy.ConstraintsEqual(current.Payload.Constraints, policy.constraints) {
		return errors.New("current subscription policy full constraint matrix differs from canonical target")
	}
	return nil
}
