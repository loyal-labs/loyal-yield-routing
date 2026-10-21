package autodeposit

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
)

// Subscriptions-program layout, owned by the program. The recurring-delegation
// account is a zero-copy Anchor account: a u8 discriminator at offset 0, then
// four pubkeys and two u64 counters. A decoder that guessed these offsets could
// authorize a pull against a fabricated allowance.
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

type DelegationIdentity struct {
	Account            string
	Delegator          string
	Delegatee          string
	Mint               string
	Nonce              uint64
	WalletTokenAccount string
}

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
	vault, err := solana.PublicKeyFromBase58(identity.Delegatee)
	if err != nil {
		return 0, err
	}
	delegation, err := delegationAccountKey(authority[:], wallet[:], vault[:], identity.Nonce)
	if err != nil || base58Key(delegation[:]) != identity.Account {
		return 0, errors.New("delegation PDA differs from frozen nonce and identity")
	}
	perPeriod := binary.LittleEndian.Uint64(data[delegationPerPeriodOffset : delegationPerPeriodOffset+8])
	pulled := binary.LittleEndian.Uint64(data[delegationAmountPulledOffset : delegationAmountPulledOffset+8])
	if pulled > perPeriod {
		return 0, fmt.Errorf("delegation account %s pulled %d exceeds its %d per-period budget", identity.Account, pulled, perPeriod)
	}
	remaining := perPeriod - pulled
	if remaining > 1<<63-1 {
		return 0, fmt.Errorf("delegation account %s allowance exceeds the family's int64 range", identity.Account)
	}
	return int64(remaining), nil
}

func subscriptionAuthorityKey(delegator, mint []byte) ([32]byte, error) {
	return findProgramAddress([][]byte{[]byte(subscriptionAuthoritySeed), delegator, mint}, SubscriptionsProgramID)
}

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

var ErrAllowanceCorrupt = errors.New("autodeposit recurring delegation allowance is unreadable")
