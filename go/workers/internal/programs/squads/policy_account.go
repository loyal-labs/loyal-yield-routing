package squads

import (
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type PolicyKind uint8

const (
	PolicyInternalFundTransfer PolicyKind = iota
	PolicySpendingLimit
	PolicySettingsChange
	PolicyProgramInteraction
)

var policyKindNames = [...]string{"InternalFundTransfer", "SpendingLimit", "SettingsChange", "ProgramInteraction"}

func (k PolicyKind) String() string {
	if int(k) < len(policyKindNames) {
		return policyKindNames[k]
	}
	return fmt.Sprintf("PolicyKind(%d)", uint8(k))
}

type PolicyAccount struct {
	Settings                                solana.PublicKey
	Seed                                    uint64
	TransactionIndex, StaleTransactionIndex uint64
	Signers                                 []Signer
	Threshold                               uint16
	TimeLock                                uint32
	Kind                                    PolicyKind
	AccountIndex                            *uint8
	SpendingLimit                           *SpendingLimitPolicy
}

type SpendingLimitPolicy struct {
	Destinations []solana.PublicKey
	Limit        SpendingLimitView
	Usage        SpendingLimitUsage
}

func DecodePolicyAccount(account *chain.Account) (PolicyAccount, error) {
	if account == nil || account.Owner != ProgramID || account.Executable {
		return PolicyAccount{}, errors.New("policy account is absent or not owned by Squads")
	}
	h, offset, err := decodeHeader(account.Data)
	if err != nil {
		return PolicyAccount{}, err
	}
	if int(h.Kind) >= len(policyKindNames) {
		return PolicyAccount{}, fmt.Errorf("unknown policy kind %d", h.Kind)
	}
	p := PolicyAccount{Settings: h.Settings, Seed: h.PolicySeed, TransactionIndex: h.TransactionIndex, StaleTransactionIndex: h.StaleTransactionIndex, Threshold: h.Threshold, TimeLock: h.TimeLock, Kind: PolicyKind(h.Kind)}
	for i, key := range h.Signers {
		p.Signers = append(p.Signers, Signer{Key: key, Permissions: h.Permissions[i]})
	}
	switch p.Kind {
	case PolicyProgramInteraction:
		index := h.VaultIndex
		p.AccountIndex = &index
	case PolicySpendingLimit:
		cursor := &borshCursor{data: account.Data, offset: offset}
		source, err := cursor.u8()
		if err != nil {
			return PolicyAccount{}, err
		}
		count, err := cursor.u32()
		if err != nil {
			return PolicyAccount{}, err
		}
		if int(count) > cursor.remaining()/32 {
			return PolicyAccount{}, errors.New("spending limit destinations exceed the account")
		}
		var limit SpendingLimitPolicy
		if limit.Destinations, err = cursor.pubkeys(int(count)); err != nil {
			return PolicyAccount{}, err
		}
		if limit.Limit, limit.Usage, err = readLegacySpendingLimit(cursor); err != nil {
			return PolicyAccount{}, err
		}
		p.AccountIndex, p.SpendingLimit = &source, &limit
	}
	return p, nil
}
