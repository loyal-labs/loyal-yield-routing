package squads

import (
	"encoding/binary"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
)

var ProgramID = solana.MustPublicKeyFromBase58("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG")

const FullPermissions = PermissionInitiate | PermissionVote | PermissionExecute

const ErrSpendingLimitExceeded uint32 = 6073

var (
	SettingsDiscriminator                       = anchor.Account("Settings")
	PolicyDiscriminator                         = anchor.Account("Policy")
	ExecuteTransactionSyncV2Discriminator       = anchor.Instruction("execute_transaction_sync_v2")
	ExecuteSettingsTransactionSyncDiscriminator = anchor.Instruction("execute_settings_transaction_sync")
)

func SettingsAddress(seed [16]byte) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("settings"), seed[:]}, ProgramID)
}

func SmartAccountAddress(settings solana.PublicKey, accountIndex uint8) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {accountIndex}}, ProgramID)
}

func PolicyAddress(settings solana.PublicKey, seed uint64) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], binary.LittleEndian.AppendUint64(nil, seed)}, ProgramID)
}
