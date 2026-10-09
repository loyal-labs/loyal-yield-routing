package squads

import (
	"bytes"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

// Signer is one smart-account signer and its permission mask.
type Signer struct {
	Key         solana.PublicKey
	Permissions uint8
}

// Settings is a smart account's Settings account.
type Settings struct {
	Seed                                    [16]byte
	SettingsAuthority                       solana.PublicKey
	Threshold                               uint16
	TimeLock                                uint32
	TransactionIndex, StaleTransactionIndex uint64
	Bump                                    uint8
	Signers                                 []Signer
	PolicySeed                              *uint64
	Reserved                                uint8
}

// DecodeSettings decodes the Settings layout of the deployed program: seed,
// settings authority, threshold, time lock, transaction indexes, archival
// authority and time, bump, signers, account utilization, the next policy
// seed and a reserved byte. Any byte after the reserved byte must be zero.
func DecodeSettings(account *chain.Account) (Settings, error) {
	var s Settings
	if account == nil || account.Owner != ProgramID || account.Executable {
		return s, errors.New("settings account is absent or not owned by Squads")
	}
	c := &borshCursor{data: account.Data}
	discriminator, err := c.take(8)
	if err != nil || !bytes.Equal(discriminator, SettingsDiscriminator[:]) {
		return s, errors.New("account is not a Squads Settings account")
	}
	fail := func(err error) (Settings, error) { return Settings{}, err }
	seed, err := c.take(16)
	if err != nil {
		return fail(err)
	}
	copy(s.Seed[:], seed)
	if s.SettingsAuthority, err = c.pubkey(); err != nil {
		return fail(err)
	}
	if s.Threshold, err = c.u16(); err != nil {
		return fail(err)
	}
	if s.TimeLock, err = c.u32(); err != nil {
		return fail(err)
	}
	if s.TransactionIndex, err = c.u64(); err != nil {
		return fail(err)
	}
	if s.StaleTransactionIndex, err = c.u64(); err != nil {
		return fail(err)
	}
	if err = c.skipOption(32); err != nil { // archival_authority
		return fail(err)
	}
	if err = c.skip(8); err != nil { // archivable_after
		return fail(err)
	}
	if s.Bump, err = c.u8(); err != nil {
		return fail(err)
	}
	count, err := c.u32()
	if err != nil {
		return fail(err)
	}
	if int(count) > c.remaining()/33 {
		return fail(errors.New("settings signer vector exceeds the account"))
	}
	for range count {
		var signer Signer
		if signer.Key, err = c.pubkey(); err != nil {
			return fail(err)
		}
		if signer.Permissions, err = c.u8(); err != nil {
			return fail(err)
		}
		s.Signers = append(s.Signers, signer)
	}
	if err = c.skip(1); err != nil { // account_utilization
		return fail(err)
	}
	if _, err = c.option(func() error {
		seed, err := c.u64()
		s.PolicySeed = &seed
		return err
	}); err != nil {
		return fail(err)
	}
	if s.Reserved, err = c.u8(); err != nil {
		return fail(err)
	}
	for _, b := range c.data[c.offset:] {
		if b != 0 {
			return fail(errors.New("settings account has unsupported trailing state"))
		}
	}
	return s, nil
}
