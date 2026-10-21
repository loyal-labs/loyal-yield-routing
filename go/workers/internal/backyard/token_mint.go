package backyard

import (
	"encoding/binary"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const (
	transferFeeConfigLen         = 108
	olderTransferFeeBPSOffset    = 88
	newerTransferFeeBPSOffset    = 106
	authorityExtensionLen        = 32
	confidentialMintLen          = 65
	confidentialMintAutoApprove  = 32
	transferHookLen              = 64
	transferHookProgramOffset    = 32
	confidentialFeeConfigLen     = 129
	confidentialFeeHarvestToMint = 64
	metadataPointerLen           = 64
	tokenMetadataMinLen          = 80
)

func validateExecutionMint(account ConfirmedAccount, program string, decimals uint8) error {
	owner, _ := solana.PublicKeyFromBase58(account.Owner)
	mint, err := spl.DecodeMint(&chain.Account{Owner: owner, Executable: account.Executable, Data: account.Data})
	if err != nil {
		return staleHoldFrom(err, "execution_mint_layout_mismatch")
	}
	if account.Owner != program || decimals > 18 || mint.Decimals != decimals {
		return staleHold("execution_mint_metadata_mismatch")
	}
	for _, extension := range mint.Extensions {
		kind, value, length := extension.Type, extension.Value, len(extension.Value)
		valid := false
		switch kind {
		case spl.ExtensionTransferFeeConfig:
			valid = length == transferFeeConfigLen
			if valid && (binary.LittleEndian.Uint16(value[olderTransferFeeBPSOffset:]) != 0 || binary.LittleEndian.Uint16(value[newerTransferFeeBPSOffset:]) != 0) {
				return staleHold("execution_mint_transfer_fee_enabled")
			}
		case spl.ExtensionMintCloseAuthority, spl.ExtensionPermanentDelegate:
			valid = length == authorityExtensionLen
		case spl.ExtensionConfidentialTransferMint:
			valid = length == confidentialMintLen && value[confidentialMintAutoApprove] <= 1
		case spl.ExtensionTransferHook:
			valid = length == transferHookLen
			if valid {
				for _, b := range value[transferHookProgramOffset:] {
					if b != 0 {
						return staleHold("execution_mint_transfer_hook_enabled")
					}
				}
			}
		case spl.ExtensionConfidentialTransferFeeConfig:
			valid = length == confidentialFeeConfigLen && value[confidentialFeeHarvestToMint] <= 1
		case spl.ExtensionMetadataPointer:
			valid = length == metadataPointerLen
		case spl.ExtensionTokenMetadata:
			valid = length >= tokenMetadataMinLen
		default:
			return staleHold("execution_mint_extension_unsupported")
		}
		if !valid {
			return staleHold("execution_mint_extension_malformed")
		}
	}
	return nil
}
