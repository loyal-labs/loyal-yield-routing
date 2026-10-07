package autodeposit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/gagliardetto/solana-go"
)

// Official Subscriptions-program layout, imported byte-for-byte from
// crates/loyal-actions/src/ids.rs. The recurring-delegation account is a
// zero-copy Anchor account: a u8 discriminator at offset 0, then four pubkeys
// and two u64 counters. A decoder that guessed these offsets could authorize a
// pull against a fabricated allowance, so every constant below is the
// loyal-actions constant of the same name.
const (
	SubscriptionsProgramID = "De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44"

	subscriptionAuthoritySeed = "SubscriptionAuthority"
	subscriptionDelegateSeed  = "delegation"
	subscriptionEventSeed     = "event_authority"

	subscriptionsTransferRecurring      = 5
	subscriptionTransferDelegatorOffset = 9
	subscriptionTransferMintOffset      = 41

	delegationDiscriminator       = 3
	delegationDiscriminatorOffset = 0
	delegationDelegatorOffset     = 3
	delegationDelegateeOffset     = 35
	delegationAuthorityOffset     = 107
	delegationMintOffset          = 139
	delegationPerPeriodOffset     = 195
	delegationAmountPulledOffset  = 203
	delegationDataLen             = 211
)

// DelegationIdentity is the on-chain identity a delegation account must prove.
// The delegator, delegatee and mint are the plan's frozen wallet, vault and
// token; a delegation whose embedded keys disagree with them authorizes
// nothing this family may pull.
type DelegationIdentity struct {
	Account            string
	Delegator          string
	Delegatee          string
	Mint               string
	Nonce              *uint64
	WalletTokenAccount string
}

// RemainingDelegationAllowance decodes one delegation account's unused
// authorization. The account owner is checked against the Subscriptions
// program before any byte is read, and the embedded delegator, delegatee and
// mint must equal the supplied identity, so an unrelated or spoofed account
// can never fund a sweep decision.
func RemainingDelegationAllowance(owner string, data []byte, identity DelegationIdentity) (int64, error) {
	if owner != SubscriptionsProgramID {
		return 0, fmt.Errorf("delegation account %s is owned by %s, want the Subscriptions program", identity.Account, owner)
	}
	if len(data) < delegationDataLen {
		return 0, fmt.Errorf("delegation account %s data is %d bytes, want at least %d", identity.Account, len(data), delegationDataLen)
	}
	if data[delegationDiscriminatorOffset] != delegationDiscriminator {
		return 0, fmt.Errorf("delegation account %s discriminator is %d, want %d", identity.Account, data[delegationDiscriminatorOffset], delegationDiscriminator)
	}
	if key := base58Key(data[delegationDelegatorOffset : delegationDelegatorOffset+32]); key != identity.Delegator {
		return 0, fmt.Errorf("delegation account %s delegator is %s, want the frozen wallet %s", identity.Account, key, identity.Delegator)
	}
	if key := base58Key(data[delegationDelegateeOffset : delegationDelegateeOffset+32]); key != identity.Delegatee {
		return 0, fmt.Errorf("delegation account %s delegatee is %s, want the frozen vault %s", identity.Account, key, identity.Delegatee)
	}
	if key := base58Key(data[delegationMintOffset : delegationMintOffset+32]); key != identity.Mint {
		return 0, fmt.Errorf("delegation account %s mint is %s, want the frozen token %s", identity.Account, key, identity.Mint)
	}
	wallet, err := solana.PublicKeyFromBase58(identity.Delegator)
	if err != nil {
		return 0, err
	}
	mint, err := solana.PublicKeyFromBase58(identity.Mint)
	if err != nil {
		return 0, err
	}
	authority, err := subscriptionAuthorityKey(wallet[:], mint[:])
	if err != nil {
		return 0, err
	}
	if base58Key(data[delegationAuthorityOffset:delegationAuthorityOffset+32]) != base58Key(authority[:]) {
		return 0, errors.New("delegation subscription authority differs from frozen wallet and mint")
	}
	if identity.Nonce != nil {
		vault, err := solana.PublicKeyFromBase58(identity.Delegatee)
		if err != nil {
			return 0, err
		}
		delegation, err := delegationAccountKey(authority[:], wallet[:], vault[:], *identity.Nonce)
		if err != nil || base58Key(delegation[:]) != identity.Account {
			return 0, errors.New("delegation PDA differs from frozen nonce and identity")
		}
	}
	perPeriod := binary.LittleEndian.Uint64(data[delegationPerPeriodOffset : delegationPerPeriodOffset+8])
	pulled := binary.LittleEndian.Uint64(data[delegationAmountPulledOffset : delegationAmountPulledOffset+8])
	// A pulled counter above its period budget is a corrupt or hostile account:
	// unknown is never zero, and a corrupt counter is never a fresh allowance.
	if pulled > perPeriod {
		return 0, fmt.Errorf("delegation account %s pulled %d exceeds its %d per-period budget", identity.Account, pulled, perPeriod)
	}
	remaining := perPeriod - pulled
	if remaining > 1<<63-1 {
		return 0, fmt.Errorf("delegation account %s allowance exceeds the family's int64 range", identity.Account)
	}
	return int64(remaining), nil
}

// subscriptionAuthority derives the delegation's subscription authority PDA:
// seeds ["SubscriptionAuthority", delegator, mint] over the Subscriptions
// program, exactly as derive_subscription_authority does in loyal-actions.
func subscriptionAuthorityKey(delegator, mint []byte) ([32]byte, error) {
	return findProgramAddress([][]byte{[]byte(subscriptionAuthoritySeed), delegator, mint}, SubscriptionsProgramID)
}

// delegationAccount derives the recurring delegation PDA the plan's identity
// implies. A store value that disagrees with this derivation is not this
// target's delegation and must not be read as one.
func delegationAccountKey(subscriptionAuthority, delegator, delegatee []byte, nonce uint64) ([32]byte, error) {
	var nonceBytes [8]byte
	binary.LittleEndian.PutUint64(nonceBytes[:], nonce)
	return findProgramAddress([][]byte{
		[]byte(subscriptionDelegateSeed), subscriptionAuthority, delegator, delegatee, nonceBytes[:],
	}, SubscriptionsProgramID)
}

func subscriptionEventAuthorityKey() ([32]byte, error) {
	return findProgramAddress([][]byte{[]byte(subscriptionEventSeed)}, SubscriptionsProgramID)
}

// ErrAllowanceCorrupt reports a delegation account whose bytes exist but do
// not decode to a usable allowance. It is distinct from a transport failure:
// the controller defers on either, but an operator alert reads them differently.
var ErrAllowanceCorrupt = errors.New("autodeposit recurring delegation allowance is unreadable")
