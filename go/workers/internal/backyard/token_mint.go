package backyard

import (
	"encoding/binary"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// validateExecutionMint checks current transfer semantics, not a stablecoin
// symbol or historical mint snapshot. Authority identities still belong to the
// lane's pinned evidence. Confidential account operations are not supported by
// DecodeTokenCustody even when the mint permits creating such accounts.
func validateExecutionMint(account ConfirmedAccount, program string, decimals uint8) error {
	owner, _ := solana.PublicKeyFromBase58(account.Owner)
	mint, err := spl.DecodeMint(&chain.Account{Owner: owner, Executable: account.Executable, Data: account.Data})
	if err != nil {
		return budgetHold("execution_mint_layout_mismatch")
	}
	if account.Owner != program || decimals > 18 || mint.Decimals != decimals {
		return budgetHold("execution_mint_metadata_mismatch")
	}
	for _, extension := range mint.Extensions {
		kind, value, length := extension.Type, extension.Value, len(extension.Value)
		valid := false
		switch kind {
		case 1: // TransferFeeConfig: reject both current and scheduled fees.
			valid = length == 108
			if valid && (binary.LittleEndian.Uint16(value[88:90]) != 0 || binary.LittleEndian.Uint16(value[106:108]) != 0) {
				return budgetHold("execution_mint_transfer_fee_enabled")
			}
		case 3, 12: // MintCloseAuthority / PermanentDelegate; no authority mutation.
			valid = length == 32
		case 4: // ConfidentialTransferMint, account-side confidential state rejected.
			valid = length == 65 && value[32] <= 1
		case 14: // TransferHook: only the disabled program is supported.
			valid = length == 64
			if valid {
				for _, b := range value[32:] {
					if b != 0 {
						return budgetHold("execution_mint_transfer_hook_enabled")
					}
				}
			}
		case 16: // ConfidentialTransferFeeConfig, SPL Token-2022 POD layout.
			valid = length == 129 && value[64] <= 1
		case 18: // MetadataPointer does not alter raw token transfer accounting.
			valid = length == 64
		case 19: // TokenMetadata; variable strings are irrelevant to raw transfers.
			valid = length >= 80
		default:
			return budgetHold("execution_mint_extension_unsupported")
		}
		if !valid {
			return budgetHold("execution_mint_extension_malformed")
		}
	}
	return nil
}
