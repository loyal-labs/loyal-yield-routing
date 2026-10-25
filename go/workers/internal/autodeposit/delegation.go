package autodeposit

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
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
	if owner != earncatalog.SubscriptionsProgram {
		return 0, fmt.Errorf("delegation account %s is owned by %s, want the Subscriptions program", identity.Account, owner)
	}
	if len(data) < earncatalog.DelegationDataLen {
		return 0, fmt.Errorf("delegation account %s data is %d bytes, want at least %d", identity.Account, len(data), earncatalog.DelegationDataLen)
	}
	if data[earncatalog.DelegationDiscriminatorOffset] != earncatalog.DelegationDiscriminator {
		return 0, fmt.Errorf("delegation account %s discriminator is %d, want %d", identity.Account, data[earncatalog.DelegationDiscriminatorOffset], earncatalog.DelegationDiscriminator)
	}
	if key := base58Key(data[earncatalog.DelegationDelegatorOffset : earncatalog.DelegationDelegatorOffset+32]); key != identity.Delegator {
		return 0, fmt.Errorf("delegation account %s delegator is %s, want the frozen wallet %s", identity.Account, key, identity.Delegator)
	}
	if key := base58Key(data[earncatalog.DelegationDelegateeOffset : earncatalog.DelegationDelegateeOffset+32]); key != identity.Delegatee {
		return 0, fmt.Errorf("delegation account %s delegatee is %s, want the frozen vault %s", identity.Account, key, identity.Delegatee)
	}
	if key := base58Key(data[earncatalog.DelegationMintOffset : earncatalog.DelegationMintOffset+32]); key != identity.Mint {
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
	if base58Key(data[earncatalog.DelegationAuthorityOffset:earncatalog.DelegationAuthorityOffset+32]) != base58Key(authority[:]) {
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
	perPeriod := binary.LittleEndian.Uint64(data[earncatalog.DelegationPerPeriodOffset : earncatalog.DelegationPerPeriodOffset+8])
	pulled := binary.LittleEndian.Uint64(data[earncatalog.DelegationAmountPulledOffset : earncatalog.DelegationAmountPulledOffset+8])
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
	return findProgramAddress([][]byte{[]byte(earncatalog.SubscriptionAuthoritySeed), delegator, mint}, earncatalog.SubscriptionsProgram)
}

func delegationAccountKey(subscriptionAuthority, delegator, delegatee []byte, nonce uint64) ([32]byte, error) {
	var nonceBytes [8]byte
	binary.LittleEndian.PutUint64(nonceBytes[:], nonce)
	return findProgramAddress([][]byte{
		[]byte(earncatalog.SubscriptionDelegateSeed), subscriptionAuthority, delegator, delegatee, nonceBytes[:],
	}, earncatalog.SubscriptionsProgram)
}

func subscriptionEventAuthorityKey() ([32]byte, error) {
	return findProgramAddress([][]byte{[]byte(earncatalog.SubscriptionEventSeed)}, earncatalog.SubscriptionsProgram)
}

var ErrAllowanceCorrupt = errors.New("autodeposit recurring delegation allowance is unreadable")
