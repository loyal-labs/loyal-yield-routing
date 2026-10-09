// Package squads holds the Squads smart-account program facts the workers
// use, each defined once: the program ID, PDA derivations, the account
// layouts we read (Settings, Policy) and the instructions we send or decode
// (execute_transaction_sync_v2 and execute_settings_transaction_sync).
package squads

import (
	"encoding/binary"

	"github.com/solana-foundation/solana-go/v2"
)

// ProgramID is the Squads smart-account program.
var ProgramID = solana.MustPublicKeyFromBase58("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG")

// FullPermissions is a signer's initiate|vote|execute permission mask.
const FullPermissions = uint8(7)

// ErrSpendingLimitExceeded is the program's custom error code for a policy
// spending limit refused at execution time.
const ErrSpendingLimitExceeded uint32 = 6073

// Anchor discriminators: sha256("account:<Name>")[:8] and
// sha256("global:<instruction>")[:8].
var (
	SettingsDiscriminator                       = [8]byte{223, 179, 163, 190, 177, 224, 67, 173}
	PolicyDiscriminator                         = [8]byte{222, 135, 7, 163, 235, 177, 33, 68}
	ExecuteTransactionSyncV2Discriminator       = [8]byte{90, 81, 187, 81, 39, 70, 128, 78}
	ExecuteSettingsTransactionSyncDiscriminator = [8]byte{138, 209, 64, 163, 79, 67, 233, 76}
)

// SettingsAddress derives ["smart_account", "settings", seed_u128_le].
func SettingsAddress(seed [16]byte) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("settings"), seed[:]}, ProgramID)
}

// SmartAccountAddress derives the vault ["smart_account", settings,
// "smart_account", account_index].
func SmartAccountAddress(settings solana.PublicKey, accountIndex uint8) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {accountIndex}}, ProgramID)
}

// PolicyAddress derives ["smart_account", "policy", settings, seed_u64_le].
func PolicyAddress(settings solana.PublicKey, seed uint64) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], binary.LittleEndian.AppendUint64(nil, seed)}, ProgramID)
}
